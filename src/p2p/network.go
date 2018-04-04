package p2p

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"iostdb"
	"net"
	"os"
)

type RequestHead struct {
	Length uint32 // Request的长度信息
}

const HEADLENGTH = 4

type Response struct {
	From        string
	To          string
	Code        int // http-like的状态码和描述
	Description string
}

// 最基本网络的模块API，之后gossip协议，虚拟的网络延迟都可以在模块内部实现
type Network interface {
	Send(req Request)
	Listen(port uint16) (chan<- Request, error)
	Close(port uint16) error
}

type NaiveNetwork struct {
	peerList *iostdb.LDBDatabase
	listen   net.Listener
	done     bool
}

func NewNaiveNetwork() *NaiveNetwork {
	dirname, _ := ioutil.TempDir(os.TempDir(), "p2p_test_")
	db, _ := iostdb.NewLDBDatabase(dirname, 0, 0)
	nn := &NaiveNetwork{
		//peerList: []string{"1.1.1.1", "2.2.2.2"},
		peerList: db,
		listen:   nil,
		done:     false,
	}
	nn.peerList.Put([]byte("1"), []byte("1.1.1.1"))
	nn.peerList.Put([]byte("2"), []byte("2.2.2.2"))
	return nn
}

func (network *NaiveNetwork) Close(port uint16) error {
	network.done = true
	return network.listen.Close()
}

func (network *NaiveNetwork) Send(req Request) {
	buf, err := req.Marshal(nil)
	if err != nil {
		fmt.Println("Error marshal body:", err.Error())
	}
	length := len(buf)
	int32buf := new(bytes.Buffer)
	binary.Write(int32buf, binary.BigEndian, length)
	for i := 1; i <= 2; i++ {
		bytesBuffer := bytes.NewBuffer([]byte{})
		binary.Write(bytesBuffer, binary.BigEndian, int32(i))
		addr, _ := network.peerList.Get(bytesBuffer.Bytes())

		conn, err := net.Dial("tcp", string(addr))
		if err != nil {
			fmt.Println("Error dialing to ", addr, err.Error())
		}
		if _, err = conn.Write(int32buf.Bytes()); err != nil {
			fmt.Println("Error sending request head:", err.Error())
		}
		if _, err = conn.Write(buf[:]); err != nil {
			fmt.Println("Error sending request body:", err.Error())
		}
		conn.Close()
	}
}

func (network *NaiveNetwork) Listen(port uint16) (chan<- Request, error) {
	var err error
	network.listen, err = net.Listen("tcp", ":"+string(port))
	if err != nil {
		fmt.Println("Error listening:", err.Error())
		return nil, err
	}
	fmt.Println("Listening on " + ":" + string(port))

	req := make(chan Request)
	go func() {
		for {
			// Listen for an incoming connection.
			conn, err := network.listen.Accept()
			if err != nil {
				fmt.Println("Error accepting: ", err.Error())
				if network.done {
					return
				}
				continue
			}
			// Handle connections in a new goroutine.
			go func(conn net.Conn) {
				defer conn.Close()
				// Make a buffer to hold incoming data.
				buf := make([]byte, HEADLENGTH)
				// Read the incoming connection into the buffer.
				_, err := conn.Read(buf)
				if err != nil {
					fmt.Println("Error reading request head:", err.Error())
				}
				length := binary.BigEndian.Uint32(buf)
				_buf := make([]byte, length)
				_, err = conn.Read(_buf)

				if err != nil {
					fmt.Println("Error reading request body:", err.Error())
				}
				var received Request
				received.Unmarshal(_buf)
				req <- received
				// Send a response back to person contacting us.
				//conn.Write([]byte("Message received."))
			}(conn)
		}

	}()
	return req, nil
}
