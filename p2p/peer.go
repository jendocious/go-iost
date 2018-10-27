package p2p

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/iost-official/go-iost/ilog"

	"github.com/iost-official/go-iost/common"
	"github.com/iost-official/go-iost/metrics"
	libnet "github.com/libp2p/go-libp2p-net"
	"github.com/libp2p/go-libp2p-peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/willf/bloom"
)

// errors
var (
	ErrStreamCountExceed         = errors.New("stream count exceed")
	ErrMessageChannelFull        = errors.New("message channel is full")
	ErrDuplicateMessage          = errors.New("reduplicate message")
	metricsBlockHeaderArriveTime = metrics.NewGauge("iost_header_arrive_time", nil)
	metricsGetStreamTimeCost     = metrics.NewGauge("iost_get_stream_time_cost", nil)
	metricsWriteStreamTimeCost   = metrics.NewGauge("iost_write_stream_time_cost", nil)
	metricsGetStreamStartTime    = metrics.NewGauge("iost_get_stream_start_time", nil)
	metricsWriteStreamStartTime  = metrics.NewGauge("iost_write_stream_start_time", nil)

	id2Node = map[string]string{
		"12D3KooWET6Hb5xYm2HkoqDUj5PAH4YDNvi8tmxVuoEFhq8GyWdq": "node01",
		"12D3KooWCiySXaC9rxLcmdatptEbWRJkLRWZbCR7vXkvWAQc7Qit": "node02",
		"12D3KooWDEaC2moDFZM444AViJ4qw4bRYJrMHf5rAPi6MkhxCtu6": "node03",
		"12D3KooWS49zFyryuovXMJB4QD9ggS9Rj7aQuY8ArDiJTHu926Hz": "node04",
		"12D3KooWRKQQL1AaafaYTwS8gFDFURuaQA65FWTiGC4pjxwM7mko": "node05",
		"12D3KooWPUbYHZvcXv825FwDAtyyzaMNGnLHzwctXCFt4z5DgnYi": "node06",
		"12D3KooWHHuSZBKb7Fq4AZa7YPHajWHTvRidwZ3McBk3TAJbtF58": "node07",
		"12D3KooWCKC6YNr9nZbVqNesofJscb4oruUnP3yHkzxeomW24k5v": "node08",
		"12D3KooWPXZomMoouWgFuUw4guGqKxAtnwboM61XkgEwjo17zD2c": "node09",
	}
)

const (
	bloomMaxItemCount = 100000
	bloomErrRate      = 0.001

	msgChanSize = 1024

	maxStreamCount = 8
)

// Peer represents a neighbor which we connect directily.
//
// Peer's jobs are:
//   * managing streams which are responsible for sending and reading messages.
//   * recording messages we have sent and received so as to reduce redundant message in network.
//   * maintaning a priority queue of message to be sending.
type Peer struct {
	id          peer.ID
	addr        multiaddr.Multiaddr
	conn        libnet.Conn
	peerManager *PeerManager

	// streams is a chan type from which we get a stream to send data and put it back after finishing.
	streams     chan libnet.Stream
	streamCount int
	streamMutex sync.Mutex

	recentMsg      *bloom.BloomFilter
	bloomMutex     sync.Mutex
	bloomItemCount int

	urgentMsgCh chan *p2pMessage
	normalMsgCh chan *p2pMessage

	quitWriteCh chan struct{}
	once        sync.Once
}

// NewPeer returns a new instance of Peer struct.
func NewPeer(stream libnet.Stream, pm *PeerManager) *Peer {
	peer := &Peer{
		id:          stream.Conn().RemotePeer(),
		addr:        stream.Conn().RemoteMultiaddr(),
		conn:        stream.Conn(),
		peerManager: pm,
		streams:     make(chan libnet.Stream, maxStreamCount),
		recentMsg:   bloom.NewWithEstimates(bloomMaxItemCount, bloomErrRate),
		urgentMsgCh: make(chan *p2pMessage, msgChanSize),
		normalMsgCh: make(chan *p2pMessage, msgChanSize),
		quitWriteCh: make(chan struct{}),
	}
	peer.AddStream(stream)
	return peer
}

// Start starts peer's loop.
func (p *Peer) Start() {
	ilog.Infof("peer is started. id=%s", p.id.Pretty())

	go p.writeLoop()
}

// Stop stops peer's loop and cuts off the TCP connection.
func (p *Peer) Stop() {
	ilog.Infof("peer is stopped. id=%s", p.id.Pretty())

	p.once.Do(func() {
		close(p.quitWriteCh)
	})
	p.conn.Close()
}

// AddStream tries to add a Stream in stream pool.
func (p *Peer) AddStream(stream libnet.Stream) error {
	p.streamMutex.Lock()
	defer p.streamMutex.Unlock()

	if p.streamCount >= maxStreamCount {
		return ErrStreamCountExceed
	}
	p.streams <- stream
	p.streamCount++
	go p.readLoop(stream)
	return nil
}

// CloseStream closes a stream and decrease the stream count.
//
// Notice that it only closes the stream for writing. Reading will still work (that
// is, the remote side can still write).
func (p *Peer) CloseStream(stream libnet.Stream) {
	p.streamMutex.Lock()
	defer p.streamMutex.Unlock()

	stream.Close()
	p.streamCount--
}

func (p *Peer) newStream() (libnet.Stream, error) {
	p.streamMutex.Lock()
	defer p.streamMutex.Unlock()
	if p.streamCount >= maxStreamCount {
		return nil, ErrStreamCountExceed
	}
	stream, err := p.peerManager.host.NewStream(context.Background(), p.id, protocolID)
	if err != nil {
		ilog.Errorf("creating stream failed. pid=%v, err=%v", p.id.Pretty(), err)
		return nil, err
	}
	p.streamCount++
	go p.readLoop(stream)
	return stream, nil
}

// getStream tries to get a stream from the stream pool.
//
// If the stream pool is empty and the stream count is less than maxStreamCount, it would create a
// new stream and use it. Otherwise it would wait for a free stream.
func (p *Peer) getStream() (libnet.Stream, error) {
	select {
	case stream := <-p.streams:
		return stream, nil
	default:
		stream, err := p.newStream()
		if err == ErrStreamCountExceed {
			break
		}
		return stream, err
	}
	return <-p.streams, nil
}

func (p *Peer) write(m *p2pMessage) error {
	//id := time.Now().UnixNano()
	//ilog.Infoln("************ start write *************", id)
	//defer ilog.Infoln("************* end write ************", id)

	t1 := time.Now()
	stream, err := p.getStream()
	t2 := time.Now()
	// if getStream fails, the TCP connection may be broken and we should stop the peer.
	if err != nil {
		ilog.Errorf("get stream fails. err=%v", err)
		p.peerManager.RemoveNeighbor(p.id)
		return err
	}

	// 5 kB/s
	deadline := time.Now().Add(time.Duration(len(m.content())/1024/5+1) * time.Second)
	if err = stream.SetWriteDeadline(deadline); err != nil {
		ilog.Warnf("set write deadline failed. err=%v", err)
		p.CloseStream(stream)
		return err
	}
	t3 := time.Now()
	_, err = stream.Write(m.setTime().content())

	if err != nil {
		ilog.Warnf("write message failed. err=%v", err)
		p.CloseStream(stream)
		return err
	}
	t4 := time.Now()
	if m.messageType() == NewBlock {
		metricsGetStreamTimeCost.Set(float64(t2.Sub(t1).Nanoseconds()/1e6), nil)
		metricsWriteStreamTimeCost.Set(float64(t4.Sub(t3).Nanoseconds()/1e6), nil)
		metricsGetStreamStartTime.Set(calculateTime(t1), nil)
		metricsWriteStreamStartTime.Set(calculateTime(t3), nil)
	}
	tagkv := map[string]string{"mtype": m.messageType().String()}
	byteOutCounter.Add(float64(len(m.content())), tagkv)
	packetOutCounter.Add(1, tagkv)

	p.streams <- stream
	return nil
}

func (p *Peer) writeLoop() {
	for {
		select {
		case <-p.quitWriteCh:
			ilog.Infof("peer is stopped. pid=%v, addr=%v", p.id.Pretty(), p.addr)
			return
		case um := <-p.urgentMsgCh:
			//ilog.Info(um.messageType())
			//go p.write(um)
			p.write(um)
		case nm := <-p.normalMsgCh:
			for done := false; !done; {
				select {
				case <-p.quitWriteCh:
					ilog.Infof("peer is stopped. pid=%v, addr=%v", p.id.Pretty(), p.addr)
					return
				case um := <-p.urgentMsgCh:
					//go p.write(um)
					p.write(um)
					//ilog.Info(um.messageType())
				default:
					done = true
				}
			}
			//go p.write(nm)
			go p.write(nm)
			//ilog.Info(nm.messageType())
		}
	}
}

func calculateTime(t time.Time) float64 {
	currentSlot := t.UnixNano() / (1e9 * common.SlotLength)
	return float64((t.UnixNano() - currentSlot*1e9*common.SlotLength) / 1e6)
}

func (p *Peer) readLoop(stream libnet.Stream) {
	header := make([]byte, dataBegin)
	for {
		_, err := io.ReadFull(stream, header) //wait up to 3000ms
		if err != nil {
			ilog.Warnf("read header failed. err=%v", err)
			return
		}
		t1 := time.Now()
		chainID := binary.BigEndian.Uint32(header[chainIDBegin:chainIDEnd])
		if chainID != p.peerManager.config.ChainID {
			ilog.Warnf("mismatched chainID. chainID=%d", chainID)
			return
		}
		length := binary.BigEndian.Uint32(header[dataLengthBegin:dataLengthEnd])
		// data := make([]byte, dataBegin+length)
		data := make([]byte, dataBegin+length+8)
		_, err = io.ReadFull(stream, data[dataBegin:])
		if err != nil {
			ilog.Warnf("read message failed. err=%v", err)
			return
		}
		copy(data[0:dataBegin], header)
		msg, err := parseP2PMessage(data)
		if msg.messageType() == NewBlock {
			metricsBlockHeaderArriveTime.Set(calculateTime(t1), nil)
			metricsRecvBlockTimeCost.Set(float64(time.Since(t1).Nanoseconds()/1e6), nil)
		}
		if err != nil {
			ilog.Errorf("parse p2pmessage failed. err=%v", err)
			return
		}
		tagkv := map[string]string{"mtype": msg.messageType().String()}
		byteInCounter.Add(float64(len(msg.content())), tagkv)
		packetInCounter.Add(1, tagkv)

		sendingTime := binary.BigEndian.Uint64(data[dataBegin+length:])
		latency := time.Now().UnixNano() - int64(sendingTime)
		nodeNum := id2Node[p.id.Pretty()]
		if nodeNum == "" {
			nodeNum = p.id.Pretty()
		}
		latencyGauge.Set(float64(latency), map[string]string{
			"mtype": msg.messageType().String(),
			"from":  nodeNum,
		})

		p.handleMessage(msg)
	}
}

// SendMessage puts message into the corresponding channel.
func (p *Peer) SendMessage(msg *p2pMessage, mp MessagePriority, deduplicate bool) error {
	if deduplicate && msg.needDedup() {
		if p.hasMessage(msg) {
			// ilog.Debug("ignore reduplicate message")
			return ErrDuplicateMessage
		}
	}
	/*  if msg.messageType() == NewBlock { */
	// p.write(msg)
	// return nil
	/* } */
	ch := p.urgentMsgCh
	if mp == NormalMessage {
		ch = p.normalMsgCh
	}
	select {
	case ch <- msg:
	default:
		//ilog.Errorf("sending message failed. channel is full. messagePriority=%d", mp)
		return ErrMessageChannelFull
	}
	if msg.needDedup() {
		p.recordMessage(msg)
	}
	return nil
}

func (p *Peer) handleMessage(msg *p2pMessage) error {
	if msg.needDedup() {
		p.recordMessage(msg)
	}
	p.peerManager.HandleMessage(msg, p.id)
	return nil
}

func (p *Peer) recordMessage(msg *p2pMessage) {
	p.bloomMutex.Lock()
	defer p.bloomMutex.Unlock()

	if p.bloomItemCount >= bloomMaxItemCount {
		p.recentMsg = bloom.NewWithEstimates(bloomMaxItemCount, bloomErrRate)
		p.bloomItemCount = 0
	}

	p.recentMsg.Add(msg.content())
	p.bloomItemCount++
}

func (p *Peer) hasMessage(msg *p2pMessage) bool {
	p.bloomMutex.Lock()
	defer p.bloomMutex.Unlock()

	return p.recentMsg.Test(msg.content())
}

// GetID return the net id
func (p *Peer) GetID() string {
	return p.id.Pretty()
}

// GetAddr return the address
func (p *Peer) GetAddr() string {
	return p.addr.String()
}
