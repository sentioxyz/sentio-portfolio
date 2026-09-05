package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestYearnV3ManifestAndRegistration(t *testing.T) {
	wantVaults := map[ChainID]int{Ethereum: 180, Base: 33, Arbitrum: 48, Polygon: 41}
	for chainID, want := range wantVaults {
		if got := len(yearnV3Deployments[chainID].Vaults); got != want {
			t.Fatalf("Yearn v3 chain %d manifest vaults = %d, want %d", chainID, got, want)
		}
	}
	engine := NewEngine(nil, nil)
	for _, protocol := range engine.Protocols() {
		if protocol.ID == "yearn-v3" {
			if want := []ChainID{Ethereum, Base, Arbitrum, Polygon}; !reflect.DeepEqual(protocol.Chains, want) {
				t.Fatalf("yearn-v3 chains = %v, want %v", protocol.Chains, want)
			}
			return
		}
	}
	t.Fatal("yearn-v3 is not registered")
}

func TestYearnV3PolygonUsesCanonicalWPOLMetadata(t *testing.T) {
	wpol := common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270")
	count := 0
	for _, vault := range yearnV3Deployments[Polygon].Vaults {
		if vault.Token.Address != wpol {
			continue
		}
		count++
		if vault.Token.Symbol != "WPOL" || vault.Token.Decimals != 18 {
			t.Errorf("vault %s WPOL metadata = %+v", vault.Address, vault.Token)
		}
	}
	if count != 5 {
		t.Fatalf("Polygon WPOL vaults = %d, want 5", count)
	}
}

func TestYearnV3ManifestHasComponentDeploymentAnchors(t *testing.T) {
	wantStaking := map[ChainID]int{Ethereum: 7, Base: 0, Arbitrum: 6, Polygon: 0}
	for chainID, deployment := range yearnV3Deployments {
		stakingCount := 0
		for _, vault := range deployment.Vaults {
			if vault.ActivationBlock == 0 {
				t.Errorf("chain %d vault %s has no activation block", chainID, vault.Address)
			}
			if vault.Staking == nil {
				continue
			}
			stakingCount++
			if vault.Staking.ActivationBlock < vault.ActivationBlock {
				t.Errorf(
					"chain %d staking %s activation %d predates vault %s activation %d",
					chainID,
					vault.Staking.Address,
					vault.Staking.ActivationBlock,
					vault.Address,
					vault.ActivationBlock,
				)
			}
		}
		if stakingCount != wantStaking[chainID] {
			t.Fatalf("chain %d staking component count = %d, want %d", chainID, stakingCount, wantStaking[chainID])
		}
	}
}

type yearnHistoricalRPC struct {
	t                *testing.T
	vault            yearnManifestVault
	mu               sync.Mutex
	stakingCallCount int
}

type yearnHistoricalCall struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

func (s *yearnHistoricalRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call yearnHistoricalCall
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode singleton RPC call: %v", err)
			return
		}
		if call.Method != "eth_chainId" {
			s.t.Errorf("unexpected singleton method %q", call.Method)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": call.ID, "result": "0x1",
		})
		return
	}

	var calls []yearnHistoricalCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode RPC batch: %v", err)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if call.Method != "eth_call" || len(call.Params) != 2 {
			response["error"] = map[string]any{"code": -32602, "message": "unexpected RPC call"}
			responses[index] = response
			continue
		}
		var input struct {
			To   common.Address `json:"to"`
			Data string         `json:"data"`
		}
		if err := json.Unmarshal(call.Params[0], &input); err != nil {
			s.t.Errorf("decode eth_call input: %v", err)
			return
		}
		if input.To == s.vault.Staking.Address {
			s.mu.Lock()
			s.stakingCallCount++
			s.mu.Unlock()
			response["error"] = map[string]any{
				"code": 3, "message": "execution reverted: staking not deployed",
			}
			responses[index] = response
			continue
		}

		data := common.FromHex(input.Data)
		if len(data) < 4 {
			response["error"] = map[string]any{"code": -32602, "message": "missing selector"}
			responses[index] = response
			continue
		}
		var (
			encoded []byte
			err     error
		)
		switch string(data[:4]) {
		case string(yearnV3ABI.Methods["balanceOf"].ID):
			encoded, err = yearnV3ABI.Methods["balanceOf"].Outputs.Pack(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
		case string(yearnV3ABI.Methods["pricePerShare"].ID):
			encoded, err = yearnV3ABI.Methods["pricePerShare"].Outputs.Pack(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
		case string(yearnV3ABI.Methods["decimals"].ID):
			encoded, err = yearnV3ABI.Methods["decimals"].Outputs.Pack(uint8(18))
		case string(yearnV3ABI.Methods["asset"].ID):
			encoded, err = yearnV3ABI.Methods["asset"].Outputs.Pack(s.vault.Token.Address)
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected selector"}
			responses[index] = response
			continue
		}
		if err != nil {
			s.t.Errorf("encode RPC result: %v", err)
			return
		}
		response["result"] = "0x" + common.Bytes2Hex(encoded)
		responses[index] = response
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func TestYearnV3SkipsStakingBeforeComponentActivation(t *testing.T) {
	vaultAddress := common.HexToAddress("0x028eC7330ff87667b6dfb0D94b954c820195336c")
	vault, exists := yearnV3AdapterForTestVault(vaultAddress)
	if !exists {
		t.Fatalf("Yearn manifest does not contain %s", vaultAddress)
	}
	if vault.Staking == nil {
		t.Fatalf("Yearn vault %s has no staking component", vaultAddress)
	}

	handler := &yearnHistoricalRPC{t: t, vault: vault}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	adapter := &yearnV3Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "yearn-v3", Name: "Yearn V3", Chains: []ChainID{Ethereum},
		}},
		vaults:  map[ChainID][]yearnManifestVault{Ethereum: {vault}},
		byVault: map[ChainID]map[common.Address]yearnManifestVault{Ethereum: {vault.Address: vault}},
	}
	groups, err := adapter.Positions(
		context.Background(),
		client,
		BlockRef{ChainID: Ethereum, Number: 19_419_991, Fixed: true},
		common.HexToAddress("0x00000000000000000000000000000000000000a1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one direct vault component", groups)
	}
	if got, want := groups[0].Components[0].AmountRaw, "1000000000000000000"; got != want {
		t.Fatalf("direct amount = %s, want %s", got, want)
	}
	handler.mu.Lock()
	stakingCalls := handler.stakingCallCount
	handler.mu.Unlock()
	if stakingCalls != 0 {
		t.Fatalf("staking calls = %d, want 0 before component activation", stakingCalls)
	}
}

func yearnV3AdapterForTestVault(address common.Address) (yearnManifestVault, bool) {
	for _, vault := range yearnV3Deployments[Ethereum].Vaults {
		if vault.Address == address {
			return vault, true
		}
	}
	return yearnManifestVault{}, false
}
