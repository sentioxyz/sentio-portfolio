//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"runtime"
	"syscall/js"

	portfolio "github.com/sentioxyz/sentio-portfolio"
)

type request struct {
	Op     string                      `json:"op"`
	Input  portfolio.SuiPortfolioInput `json:"input"`
	Offset int                         `json:"offset"`
	Limit  int                         `json:"limit"`
}

var input portfolio.SuiPortfolioInput
var calculator *portfolio.SuiPortfolioCalculator
var accounts []string

func dispatch(raw string) (result any, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = fmt.Errorf("daily calculator panic")
		}
	}()
	var req request
	if err = json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, fmt.Errorf("invalid calculator request")
	}
	switch req.Op {
	case "begin":
		input = req.Input
		calculator = nil
		accounts = nil
		return map[string]bool{"ok": true}, nil
	case "append":
		if calculator != nil {
			return nil, fmt.Errorf("snapshot already prepared")
		}
		input.Objects = append(input.Objects, req.Input.Objects...)
		input.Quotes = append(input.Quotes, req.Input.Quotes...)
		input.Metadata = append(input.Metadata, req.Input.Metadata...)
		return map[string]bool{"ok": true}, nil
	case "prepare":
		calculator, err = portfolio.NewSuiPortfolioCalculator(input)
		if err != nil {
			return nil, err
		}
		input = portfolio.SuiPortfolioInput{}
		accounts = calculator.Accounts()
		return map[string]int{"accounts": len(accounts)}, nil
	case "calculate":
		if calculator == nil || req.Offset < 0 || req.Limit < 1 || req.Limit > 128 || req.Offset > len(accounts) {
			return nil, fmt.Errorf("invalid calculator batch")
		}
		end := min(req.Offset+req.Limit, len(accounts))
		events := make([]portfolio.SuiPortfolioEvent, 0, end-req.Offset)
		for _, account := range accounts[req.Offset:end] {
			event, e := calculator.Calculate(account)
			if e != nil {
				event.Positions = []portfolio.SuiPortfolioPosition{}
				event.Errors = []string{e.Error()}
			}
			events = append(events, event)
		}
		return events, nil
	case "reset":
		input = portfolio.SuiPortfolioInput{}
		calculator = nil
		accounts = nil
		runtime.GC()
		return map[string]bool{"ok": true}, nil
	default:
		return nil, fmt.Errorf("unknown calculator operation")
	}
}

func main() {
	js.Global().Set("suiPortfolioCalculate", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].Type() != js.TypeString {
			return `{"error":"invalid calculator arguments"}`
		}
		result, err := dispatch(args[0].String())
		if err != nil {
			result = map[string]string{"error": err.Error()}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return `{"error":"calculator serialization failed"}`
		}
		return string(encoded)
	}))
	select {}
}
