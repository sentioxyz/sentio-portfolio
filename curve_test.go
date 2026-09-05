package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestCurveLendingDeployments(t *testing.T) {
	adapter := newCurveLendingAdapter().(*curveLendingAdapter)
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, Arbitrum, Optimism}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Curve Lending chains = %v, want %v", got, want)
	}
	arbitrum, exists := adapter.deployments[Arbitrum]
	if !exists {
		t.Fatal("Curve Lending has no Arbitrum deployment")
	}
	if got, want := arbitrum.oneWayFactory,
		common.HexToAddress("0xcaEC110C784c9DF37240a8Ce096D352A75922DeA"); got != want {
		t.Fatalf("Arbitrum one-way factory = %s, want %s", got, want)
	}
	if arbitrum.oneWayActivation != 193_652_535 {
		t.Fatalf("Arbitrum one-way activation = %d, want 193652535", arbitrum.oneWayActivation)
	}
	if arbitrum.v2Factory != (common.Address{}) || arbitrum.v2Activation != 0 {
		t.Fatalf("Arbitrum unexpectedly has Curve Lending v2: %#v", arbitrum)
	}
	optimism, exists := adapter.deployments[Optimism]
	if !exists {
		t.Fatal("Curve Lending has no Optimism deployment")
	}
	if got, want := optimism.v2Factory,
		common.HexToAddress("0x5F94073E3f51c1FFf92ffc6b4B06b7Af193B3640"); got != want {
		t.Fatalf("Optimism v2 factory = %s, want %s", got, want)
	}
	if optimism.v2Activation != 152_707_578 {
		t.Fatalf("Optimism v2 activation = %d, want 152707578", optimism.v2Activation)
	}
	if got, want := optimism.oneWayFactory,
		common.HexToAddress("0x5EA8f3D674C70b020586933A0a5b250734798BeF"); got != want {
		t.Fatalf("Optimism one-way factory = %s, want %s", got, want)
	}
	if optimism.oneWayActivation != 125_072_267 {
		t.Fatalf("Optimism one-way activation = %d, want 125072267", optimism.oneWayActivation)
	}
}

func TestCurveLendingOptimismEnumeratesV2MarketTuple(t *testing.T) {
	addresses := []common.Address{
		common.HexToAddress("0x0000000000000000000000000000000000000011"),
		common.HexToAddress("0x0000000000000000000000000000000000000022"),
		common.HexToAddress("0x0000000000000000000000000000000000000033"),
		common.HexToAddress("0x0000000000000000000000000000000000000044"),
		common.HexToAddress("0x0000000000000000000000000000000000000055"),
		common.HexToAddress("0x0000000000000000000000000000000000000066"),
		common.HexToAddress("0x0000000000000000000000000000000000000077"),
	}
	marketCount, err := curveV2FactoryABI.Methods["market_count"].Outputs.Pack(big.NewInt(1))
	if err != nil {
		t.Fatal(err)
	}
	marketTuple, err := curveV2FactoryABI.Methods["markets"].Outputs.Pack(
		addresses[0],
		addresses[1],
		addresses[2],
		addresses[3],
		addresses[4],
		addresses[5],
		addresses[6],
	)
	if err != nil {
		t.Fatal(err)
	}

	respond := func(call rpcTestRequest) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if call.Method == "eth_chainId" {
			response["result"] = "0xa"
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
			t.Errorf("decode eth_call: %v", err)
			response["error"] = map[string]any{"code": -32602, "message": "invalid eth_call"}
			return response
		}
		switch {
		case bytes.HasPrefix(params.Data, curveV2FactoryABI.Methods["market_count"].ID):
			response["result"] = hexutil.Encode(marketCount)
		case bytes.HasPrefix(params.Data, curveV2FactoryABI.Methods["markets"].ID):
			response["result"] = hexutil.Encode(marketTuple)
		default:
			t.Errorf("unexpected eth_call data %x", params.Data)
			response["error"] = map[string]any{"code": -32601, "message": "unexpected selector"}
		}
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
			if err := json.NewEncoder(writer).Encode(respond(call)); err != nil {
				t.Errorf("encode singleton RPC response: %v", err)
			}
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
		if err := json.NewEncoder(writer).Encode(responses); err != nil {
			t.Errorf("encode RPC batch response: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Optimism, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	deployment := curveLendingDeployments[Optimism]
	markets, err := enumerateCurveV2Markets(
		context.Background(),
		client,
		BlockRef{ChainID: Optimism, Number: deployment.v2Activation, Fixed: true},
		deployment,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) != 1 || markets[0] != (curveLendingMarket{
		generation:      "v2",
		vault:           addresses[0],
		controller:      addresses[1],
		collateralToken: addresses[3],
		borrowedToken:   addresses[4],
	}) {
		t.Fatalf("Curve v2 markets = %+v", markets)
	}
}

func TestCurveBoundedCount(t *testing.T) {
	tests := []struct {
		name    string
		value   *big.Int
		want    int
		wantErr bool
	}{
		{name: "zero", value: big.NewInt(0), want: 0},
		{name: "maximum", value: big.NewInt(curveMaxMarkets), want: curveMaxMarkets},
		{name: "over maximum", value: big.NewInt(curveMaxMarkets + 1), wantErr: true},
		{name: "negative", value: big.NewInt(-1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := curveBoundedCount([]any{test.value}, "test")
			if (err != nil) != test.wantErr {
				t.Fatalf("curveBoundedCount() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("curveBoundedCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCurveCollateralPresentationRules(t *testing.T) {
	savingsREUSD := common.HexToAddress("0x557AB1e003951A73c12D16F0fEA8490E39C33C35")
	savingsDOLA := common.HexToAddress("0xb45ad160634c528cc3d2926d9807104fa3157305")
	savingsFRXUSD := common.HexToAddress("0xcf62f905562626cfcdd2261162a51fd02fc9c5b6")
	if _, exists := curveUnderlyingCollateral[savingsREUSD]; !exists {
		t.Fatal("Savings reUSD must be normalized to reUSD")
	}
	for _, wrapper := range []common.Address{savingsDOLA, savingsFRXUSD} {
		if _, exists := curveUnderlyingCollateral[wrapper]; exists {
			t.Fatalf("%s must remain a wrapper token", wrapper)
		}
	}
}
