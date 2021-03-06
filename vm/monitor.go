package vm

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bitly/go-simplejson"

	"time"

	"encoding/json"
	"math"

	"github.com/iost-official/go-iost/common"
	"github.com/iost-official/go-iost/core/contract"
	"github.com/iost-official/go-iost/core/tx"
	"github.com/iost-official/go-iost/ilog"
	"github.com/iost-official/go-iost/vm/host"
	"github.com/iost-official/go-iost/vm/native"
	"github.com/iost-official/go-iost/vm/v8vm"
)

// Monitor ...
type Monitor struct {
	vms map[string]VM
}

// NewMonitor ...
func NewMonitor() *Monitor {
	m := &Monitor{
		vms: make(map[string]VM),
	}
	jsvm := Factory("javascript")
	m.vms["javascript"] = jsvm
	return m
}

func (m *Monitor) prepareContract(h *host.Host, contractName, api, jarg string) (c *contract.Contract, abi *contract.ABI, args []interface{}, err error) {
	var cid string
	if h.IsDomain(contractName) {
		cid = h.ContractID(contractName)
		if cid == "" {
			cid = contractName
		}
	} else {
		cid = contractName
	}

	c = h.DB().Contract(cid)
	if c == nil {
		return nil, nil, nil, fmt.Errorf("contract %s not found", cid)
	}

	abi = c.ABI(api)

	if abi == nil {
		return nil, nil, nil, fmt.Errorf("abi %s not found", api)
	}

	args, err = UnmarshalArgs(abi, jarg)

	return
}

func checkLimit(amountLimit map[string]*common.Fixed, token string, amount *common.Fixed) bool {
	if amount.Value > 0 {
		if limit, ok := amountLimit[token]; ok {
			return amount.Value <= limit.Value
		} else if limit, ok := amountLimit["*"]; ok {
			val := amount.Value / int64(math.Pow10(amount.Decimal))
			if amount.Value%int64(math.Pow10(amount.Decimal)) > 0 {
				val++
			}
			return val <= limit.Value
		}
		return false
	}
	return true
}

func getAmountLimitMap(h *host.Host, amountList []*contract.Amount) (map[string]*common.Fixed, error) {
	amountLimit := make(map[string]*common.Fixed)
	for _, limit := range amountList {
		if limit.Val == "unlimited" {
			amountLimit[limit.Token] = &common.Fixed{Value: math.MaxInt64, Decimal: h.DB().Decimal(limit.Token)}
		} else {
			decimal := h.DB().Decimal(limit.Token)
			if limit.Token == "*" {
				decimal = 0
			}
			v0, err := common.NewFixed(limit.Val, decimal)
			if err != nil {
				return nil, err
			}
			amountLimit[limit.Token] = v0
		}
	}
	return amountLimit, nil
}

// Call ...
// nolint
func (m *Monitor) Call(h *host.Host, contractName, api string, jarg string) (rtn []interface{}, cost contract.Cost, err error) {
	c, abi, args, err := m.prepareContract(h, contractName, api, jarg)
	if err != nil {
		return nil, host.Costs["GetCost"], fmt.Errorf("prepare contract: %v", err)
	}

	h.PushCtx()
	defer func() {
		h.PopCtx()
	}()
	cost = contract.Cost0()

	stackHeight := h.Context().Value("stack_height").(int)
	if stackHeight > 5 {
		return nil, cost, fmt.Errorf("stack height exceed. actual %v", stackHeight)
	}

	h.Context().Set("contract_name", c.ID)
	h.Context().Set("abi_name", api)

	// flag-down fare
	switch c.Info.Lang {
	case "javascript":
		cost.AddAssign(host.Costs["JSCost"])
	}

	vm, ok := m.vms[c.Info.Lang]
	if !ok {
		vm = Factory(c.Info.Lang)
		m.vms[c.Info.Lang] = vm
	}
	currentDeadline := h.Deadline()
	h.SetDeadline(currentDeadline.Add(time.Duration(-100 * time.Microsecond)))

	oldCacheCost := h.CacheCost()
	h.ClearCacheCost()

	// generate amount limit
	oldReceiptLen := 0
	if h.Context().GValue("receipts") != nil {
		oldReceiptLen = len(h.Context().GValue("receipts").([]*tx.Receipt))
	}
	amountLimit := make(map[string]*common.Fixed)
	txAmountLimit := make(map[string]*common.Fixed)

	if h.Context().Value("stack_height") == 1 {
		cost.AddAssign(host.CommonOpCost(len(abi.AmountLimit)))
		amountLimit, err = getAmountLimitMap(h, abi.AmountLimit)
		if err != nil {
			return nil, cost, err
		}

		if h.Context().Value("amount_limit") != nil {
			txLimit := h.Context().Value("amount_limit").([]*contract.Amount)
			txAmountLimit, err = getAmountLimitMap(h, txLimit)
			if err != nil {
				return nil, cost, err
			}
		}
	}

	rtn, cost0, err := vm.LoadAndCall(h, c, api, args...)
	cost.AddAssign(cost0)
	if err != nil {
		return
	}

	// check amount limit
	if h.Context().Value("stack_height") == 1 {
		receipts := []*tx.Receipt{}
		if h.Context().GValue("receipts") != nil {
			receipts = h.Context().GValue("receipts").([]*tx.Receipt)
		}
		needLimit := make(map[string]*common.Fixed)
		for i := oldReceiptLen; i < len(receipts); i++ {
			cost.AddAssign(host.CommonOpCost(1))
			receipt := receipts[i]
			token := ""
			amount, _ := common.NewFixed("0", 0)
			args := []interface{}{}
			if receipt.FuncName == "token.iost/transfer" || receipt.FuncName == "token.iost/transferFreeze" {
				_ = json.Unmarshal([]byte(receipt.Content), &args)
				token = args[0].(string)
				from := args[1].(string)
				to := args[2].(string)
				if from != to && !h.IsContract(from) {
					amount, _ = common.NewFixed(args[3].(string), h.DB().Decimal(token))
				}
			} else if receipt.FuncName == "token.iost/destroy" {
				_ = json.Unmarshal([]byte(receipt.Content), &args)
				token = args[0].(string)
				from := args[1].(string)
				if !h.IsContract(from) {
					amount, _ = common.NewFixed(args[2].(string), h.DB().Decimal(token))
				}
			}
			if token != "" && amount.Value >= 0 {
				if a, ok := needLimit[token]; ok {
					needLimit[token] = a.Add(amount)
				} else {
					needLimit[token] = amount
				}
			}
		}
		for token, amount := range needLimit {
			if !checkLimit(amountLimit, token, amount) {
				return nil, cost,
					fmt.Errorf("token %s exceed amountLimit in abi. need %v",
						token, amount.ToString())
			}
			if !checkLimit(txAmountLimit, token, amount) {
				return nil, cost,
					fmt.Errorf("token %s exceed amountLimit in tx. need %v",
						token, amount.ToString())
			}
		}
	}
	// check ram auth
	cacheCost := h.CacheCost()
	h.FlushCacheCost()
	payer := make(map[string]bool)
	for _, c := range cacheCost.DataList {
		if c.Val > 0 {
			payer[c.Payer] = true
		}
	}
	for p := range payer {
		if strings.HasSuffix(p, ".iost") {
			continue
		}
		ok, _ := h.RequireAuth(p, "active")
		if !ok {
			return nil, cost, errors.New("pay ram failed. no permission. need " + p + "@active")
		}
	}
	h.AddCacheCost(oldCacheCost)
	return
}

// Compile ...
func (m *Monitor) Compile(con *contract.Contract) (string, error) {
	switch con.Info.Lang {
	case "native":
		return "", nil
	case "javascript":
		jsvm, _ := m.vms["javascript"]
		return jsvm.Compile(con)
	}
	return "", errors.New("vm unsupported")
}

// Validate ...
func (m *Monitor) Validate(con *contract.Contract) error {
	switch con.Info.Lang {
	case "native":
		return nil
	case "javascript":
		jsvm, _ := m.vms["javascript"]
		return jsvm.Validate(con)
	}
	return errors.New("vm unsupported")
}

// Factory ...
func Factory(lang string) VM {
	switch lang {
	case "native":
		vm := native.Impl{}
		vm.Init()
		return &vm
	case "javascript":
		vm := v8.NewVMPool(10, 400)
		vm.Init()
		//vm.SetJSPath(jsPath)
		return vm
	}
	return nil
}

// UnmarshalArgs convert action data to args according to abi
func UnmarshalArgs(abi *contract.ABI, data string) ([]interface{}, error) {
	if strings.HasSuffix(data, ",]") {
		data = data[:len(data)-2] + "]"
	}
	js, err := simplejson.NewJson([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("error in data: %v, %v", err, data)
	}

	rtn := make([]interface{}, 0)
	arr, err := js.Array()
	if err != nil {
		ilog.Error(js.EncodePretty())
		return nil, fmt.Errorf("error args should be array, %v, %v", err, js)
	}

	if len(arr) != len(abi.Args) {
		return nil, fmt.Errorf("args length unmatched to abi %v. need %v, got %v", abi.Name, len(abi.Args), len(arr))
	}
	for i := range arr {
		switch abi.Args[i] {
		case "string":
			s, err := js.GetIndex(i).String()
			if err != nil {
				return nil, fmt.Errorf("error parse string arg %v, %v", js.GetIndex(i), err)
			}
			rtn = append(rtn, s)
		case "bool":
			s, err := js.GetIndex(i).Bool()
			if err != nil {
				return nil, fmt.Errorf("error parse bool arg %v, %v", js.GetIndex(i), err)
			}
			rtn = append(rtn, s)
		case "number":
			s, err := js.GetIndex(i).Int64()
			if err != nil {
				return nil, fmt.Errorf("error parse number arg %v, %v", js.GetIndex(i), err)
			}
			rtn = append(rtn, s)
		case "json":
			s, err := js.GetIndex(i).Encode()
			if err != nil {
				return nil, fmt.Errorf("error parse json arg %v, %v", js.GetIndex(i), err)
			}
			// make sure s is a valid json
			_, err = simplejson.NewJson(s)
			if err != nil {
				ilog.Error(string(s))
				return nil, fmt.Errorf("error parse json arg %v, %v", js.GetIndex(i), err)
			}
			rtn = append(rtn, s)
		}
	}

	return rtn, nil
}
