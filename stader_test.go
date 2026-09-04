package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStaderBSCDeploymentAndRegistration(t *testing.T) {
	adapter := newStaderAdapter().(*StaderAdapter)
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, BSC, Polygon}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Stader chains = %v, want %v", got, want)
	}
	if got, want := staderBSCDeployment.liquidToken.Address,
		common.HexToAddress("0x1bdd3cf7f79cfb8edbb955f20ad99211551ba275"); got != want {
		t.Fatalf("BNBx token = %s, want %s", got, want)
	}
	if got, want := staderBSCDeployment.manager,
		common.HexToAddress("0x3b961e83400D51e6E1AF5c450d3C7d7b80588d28"); got != want {
		t.Fatalf("BNBx manager = %s, want %s", got, want)
	}
	if staderBSCDeployment.tokenActivationBlock != 19_907_065 {
		t.Fatalf("BNBx token activation block = %d, want 19907065", staderBSCDeployment.tokenActivationBlock)
	}
	if staderBSCDeployment.managerActivationBlock != 40_990_394 {
		t.Fatalf("BNBx manager activation block = %d, want 40990394", staderBSCDeployment.managerActivationBlock)
	}
}

func TestStaderPolygonDeploymentAndConversions(t *testing.T) {
	deployment := staderPolygonDeployment
	if got, want := deployment.liquidToken.Address,
		common.HexToAddress("0xfa68FB4628DFF1028CFEc22b4162FCcd0d45efb6"); got != want {
		t.Fatalf("MaticX token = %s, want %s", got, want)
	}
	if got, want := deployment.childPool,
		common.HexToAddress("0xfd225C9e6601C9d38d8F98d8731BF59eFcF8C0E3"); got != want {
		t.Fatalf("MaticX child pool = %s, want %s", got, want)
	}
	if got, want := deployment.rateProvider,
		common.HexToAddress("0xeE652bbF72689AA59F0B8F981c9c90e2A8Af8d8f"); got != want {
		t.Fatalf("MaticX rate provider = %s, want %s", got, want)
	}
	if deployment.tokenActivationBlock != 27_403_468 ||
		deployment.rateActivationBlock != 27_449_856 ||
		deployment.conversionActivationBlock != 27_683_276 ||
		deployment.withdrawalsActivationBlock != 29_081_643 {
		t.Fatalf("MaticX activation boundaries = %+v", deployment)
	}
	if got := staderDeploymentWindows[Polygon].ActivationBlock; got != deployment.tokenActivationBlock {
		t.Fatalf("Polygon adapter activation = %d, want MaticX token activation %d", got, deployment.tokenActivationBlock)
	}
	conversion, exists := staderPolygonChildPoolABI.Methods["convertMaticXToMatic"]
	if !exists || len(conversion.Inputs) != 1 || len(conversion.Outputs) != 3 {
		t.Fatalf("MaticX conversion ABI = %+v", conversion)
	}
	amount, err := staderPolygonMaticAmount(
		big.NewInt(2_000_000_000_000_000_000),
		big.NewInt(1_195_653_883_023_860_333),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := amount.String(), "2391307766047720666"; got != want {
		t.Fatalf("MaticX converted amount = %s, want %s", got, want)
	}
	if _, err := staderPolygonMaticAmount(big.NewInt(1), big.NewInt(0)); err == nil {
		t.Fatal("zero MaticX rate did not fail closed")
	}
}

func TestStaderPolygonReportsMaticXBeforeRateProviderActivation(t *testing.T) {
	shares := big.NewInt(1_250_000_000_000_000_000)
	encoded, err := erc20ABI.Methods["balanceOf"].Outputs.Pack(shares)
	if err != nil {
		t.Fatal(err)
	}
	respond := func(call rpcTestRequest) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if call.Method == "eth_chainId" {
			response["result"] = "0x89"
			return response
		}
		if call.Method != "eth_call" || len(call.Params) < 1 {
			t.Errorf("unexpected RPC call: %+v", call)
			response["error"] = map[string]any{"code": -32601, "message": "unexpected call"}
			return response
		}
		var input struct {
			Data string `json:"data"`
		}
		if decodeErr := json.Unmarshal(call.Params[0], &input); decodeErr != nil {
			t.Errorf("decode eth_call: %v", decodeErr)
			response["error"] = map[string]any{"code": -32602, "message": "bad input"}
			return response
		}
		data := common.FromHex(input.Data)
		if len(data) < 4 || string(data[:4]) != string(erc20ABI.Methods["balanceOf"].ID) {
			t.Errorf("pre-rate scan emitted unexpected selector %x", data)
			response["error"] = map[string]any{"code": -32601, "message": "rate provider must not be called"}
			return response
		}
		response["result"] = "0x" + common.Bytes2Hex(encoded)
		return response
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var raw json.RawMessage
		if decodeErr := json.NewDecoder(request.Body).Decode(&raw); decodeErr != nil {
			t.Errorf("decode RPC request: %v", decodeErr)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if len(raw) > 0 && raw[0] == '{' {
			var call rpcTestRequest
			_ = json.Unmarshal(raw, &call)
			_ = json.NewEncoder(writer).Encode(respond(call))
			return
		}
		var calls []rpcTestRequest
		_ = json.Unmarshal(raw, &calls)
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

	groups, err := newStaderAdapter().Positions(
		context.Background(),
		client,
		BlockRef{ChainID: Polygon, Number: staderPolygonDeployment.rateActivationBlock - 1, Fixed: true},
		common.HexToAddress("0x00000000000000000000000000000000000000a1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one raw MaticX position", groups)
	}
	component := groups[0].Components[0]
	if component.Token.Address != staderPolygonDeployment.liquidToken.Address {
		t.Fatalf("token = %s, want MaticX %s", component.Token.Address, staderPolygonDeployment.liquidToken.Address)
	}
	if component.AmountRaw != shares.String() {
		t.Fatalf("amount = %s, want %s", component.AmountRaw, shares)
	}
}

func TestStaderPolygonSwapRequestDecoding(t *testing.T) {
	want := []staderPolygonSwapRequest{
		{Amount: big.NewInt(11), RequestTime: big.NewInt(22), WithdrawalTime: big.NewInt(33)},
		{Amount: big.NewInt(44), RequestTime: big.NewInt(55), WithdrawalTime: big.NewInt(66)},
	}
	encoded, err := staderPolygonChildPoolABI.Methods["getUserMaticXSwapRequests"].Outputs.Pack(want)
	if err != nil {
		t.Fatal(err)
	}
	values, err := staderPolygonChildPoolABI.Methods["getUserMaticXSwapRequests"].Outputs.Unpack(encoded)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeStaderPolygonSwapRequests(values[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded MaticX requests = %+v, want %+v", got, want)
	}
}

func TestStaderBSCWithdrawalAmount(t *testing.T) {
	amount, err := staderBSCWithdrawalAmount(
		big.NewInt(125),
		[]staderBSCProcessedWithdrawal{
			{amountInBNBx: big.NewInt(20), batchAmountInBNB: big.NewInt(33), batchAmountInBNBx: big.NewInt(30)},
			{amountInBNBx: big.NewInt(10), batchAmountInBNB: big.NewInt(33), batchAmountInBNBx: big.NewInt(30)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 125 is the already-converted total for unprocessed requests. The processed
	// requests receive 20/30 and 10/30 of a 33 BNB batch: 22 + 11 BNB.
	if got, want := amount, big.NewInt(158); got.Cmp(want) != 0 {
		t.Fatalf("withdrawal amount = %s, want %s", got, want)
	}
	if _, err := staderBSCWithdrawalAmount(
		big.NewInt(0),
		[]staderBSCProcessedWithdrawal{{
			amountInBNBx: big.NewInt(1), batchAmountInBNB: big.NewInt(1), batchAmountInBNBx: big.NewInt(0),
		}},
	); err == nil {
		t.Fatal("zero batch BNBx denominator did not fail")
	}
}
