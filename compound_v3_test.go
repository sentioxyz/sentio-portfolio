package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestCompoundV3NewChainRewardRegistrationBoundaries(t *testing.T) {
	want := map[ChainID]map[string]uint64{
		Polygon: {
			"USDC.e": 39_413_527,
			"USDT":   58_793_297,
		},
		Optimism: {
			"USDC": 118_840_983,
			"USDT": 121_727_936,
			"WETH": 123_072_627,
		},
	}
	adapter := newCompoundV3Adapter().(*CompoundV3Adapter)
	for chainID, markets := range want {
		if len(adapter.markets[chainID]) != len(markets) {
			t.Fatalf("Compound v3 chain %d markets = %d, want %d", chainID, len(adapter.markets[chainID]), len(markets))
		}
		for _, market := range adapter.markets[chainID] {
			activation, exists := markets[market.Label]
			if !exists {
				t.Fatalf("Compound v3 chain %d has unexpected market %q", chainID, market.Label)
			}
			if market.RewardsActivationBlock != activation {
				t.Errorf(
					"Compound v3 chain %d market %s reward start = %d, want %d",
					chainID,
					market.Label,
					market.RewardsActivationBlock,
					activation,
				)
			}
			if market.RewardsActiveAt(activation - 1) {
				t.Errorf("Compound v3 chain %d market %s rewards active before block %d", chainID, market.Label, activation)
			}
			if !market.RewardsActiveAt(activation) {
				t.Errorf("Compound v3 chain %d market %s rewards inactive at block %d", chainID, market.Label, activation)
			}
		}
	}
}

func TestCompoundV3PositionsRespectsRewardRegistrationBoundary(t *testing.T) {
	const rewardActivation = uint64(100)
	market := cometMarket{
		deploymentWindow:       deploymentWindow{ActivationBlock: 1},
		Label:                  "test",
		Comet:                  common.HexToAddress("0x0000000000000000000000000000000000000100"),
		Rewards:                common.HexToAddress("0x0000000000000000000000000000000000000200"),
		RewardsActivationBlock: rewardActivation,
	}
	adapter := &CompoundV3Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "compound-v3", Name: "Compound III", Chains: []ChainID{Polygon}}},
		markets:     map[ChainID][]cometMarket{Polygon: {market}},
	}

	pack := func(contractABIMethod string, contractABIOutputs any, values ...any) []byte {
		var packed []byte
		var err error
		switch contractABIMethod {
		case "baseToken", "numAssets", "balanceOf", "borrowBalanceOf":
			packed, err = cometABI.Methods[contractABIMethod].Outputs.Pack(values...)
		case "rewardConfig", "getRewardOwed":
			packed, err = cometRewardsABI.Methods[contractABIMethod].Outputs.Pack(contractABIOutputs)
		default:
			t.Fatalf("unsupported ABI method %q", contractABIMethod)
		}
		if err != nil {
			t.Fatalf("pack %s output: %v", contractABIMethod, err)
		}
		return packed
	}
	results := map[string][]byte{
		string(cometABI.Methods["baseToken"].ID):            pack("baseToken", nil, common.HexToAddress("0x0000000000000000000000000000000000000300")),
		string(cometABI.Methods["numAssets"].ID):            pack("numAssets", nil, uint8(0)),
		string(cometABI.Methods["balanceOf"].ID):            pack("balanceOf", nil, new(big.Int)),
		string(cometABI.Methods["borrowBalanceOf"].ID):      pack("borrowBalanceOf", nil, new(big.Int)),
		string(cometRewardsABI.Methods["rewardConfig"].ID):  pack("rewardConfig", cometRewardConfig{}),
		string(cometRewardsABI.Methods["getRewardOwed"].ID): pack("getRewardOwed", cometRewardOwed{Owed: new(big.Int)}),
	}

	var callsMu sync.Mutex
	rewardCalls := map[string]int{
		"rewardConfig":  0,
		"getRewardOwed": 0,
	}
	respond := func(call rpcTestRequest) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if call.Method == "eth_chainId" {
			response["result"] = "0x89"
			return response
		}
		if call.Method != "eth_call" || len(call.Params) < 1 {
			t.Errorf("unexpected RPC call: %+v", call)
			response["error"] = map[string]any{"code": -32601, "message": "unexpected RPC call"}
			return response
		}
		var params struct {
			Data hexutil.Bytes `json:"data"`
		}
		if err := json.Unmarshal(call.Params[0], &params); err != nil {
			t.Errorf("decode eth_call params: %v", err)
			response["error"] = map[string]any{"code": -32602, "message": "invalid eth_call"}
			return response
		}
		if len(params.Data) < 4 {
			t.Errorf("eth_call data is too short")
			response["error"] = map[string]any{"code": -32602, "message": "missing selector"}
			return response
		}
		selector := string(params.Data[:4])
		result, exists := results[selector]
		if !exists {
			t.Errorf("unexpected selector 0x%x", params.Data[:4])
			response["error"] = map[string]any{"code": -32601, "message": "unexpected selector"}
			return response
		}
		callsMu.Lock()
		switch selector {
		case string(cometRewardsABI.Methods["rewardConfig"].ID):
			rewardCalls["rewardConfig"]++
		case string(cometRewardsABI.Methods["getRewardOwed"].ID):
			rewardCalls["getRewardOwed"]++
		}
		callsMu.Unlock()
		response["result"] = hexutil.Encode(result)
		return response
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
			t.Errorf("decode RPC request: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if len(raw) > 0 && raw[0] == '{' {
			var call rpcTestRequest
			if err := json.Unmarshal(raw, &call); err != nil {
				t.Errorf("decode singleton RPC call: %v", err)
				http.Error(writer, "invalid request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(respond(call))
			return
		}
		var calls []rpcTestRequest
		if err := json.Unmarshal(raw, &calls); err != nil {
			t.Errorf("decode RPC batch: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		responses := make([]map[string]any, len(calls))
		for index, call := range calls {
			responses[index] = respond(call)
		}
		_ = json.NewEncoder(writer).Encode(responses)
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Polygon, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	account := common.HexToAddress("0x0000000000000000000000000000000000000400")
	for _, blockNumber := range []uint64{rewardActivation - 1, rewardActivation} {
		groups, err := adapter.Positions(
			context.Background(),
			client,
			BlockRef{ChainID: Polygon, Number: blockNumber, Fixed: true},
			account,
		)
		if err != nil {
			t.Fatalf("positions at block %d: %v", blockNumber, err)
		}
		if len(groups) != 0 {
			t.Fatalf("positions at block %d returned %d groups, want none", blockNumber, len(groups))
		}
		callsMu.Lock()
		want := 0
		if blockNumber == rewardActivation {
			want = 1
		}
		for method, count := range rewardCalls {
			if count != want {
				t.Errorf("%s calls after block %d = %d, want %d", method, blockNumber, count, want)
			}
		}
		callsMu.Unlock()
	}
}
