package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestEngineRegistersCompleteMapleManifest(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		if protocol.ID != "maple" {
			continue
		}
		adapter := newMapleAdapter().(*MapleAdapter)
		if got := len(adapter.vaults[Ethereum]); got != 21 {
			t.Fatalf("Maple vault count = %d, want 21", got)
		}
		if got := len(adapter.queues); got != 11 {
			t.Fatalf("Maple queue count = %d, want 11", got)
		}
		return
	}
	t.Fatal("maple is not registered")
}

// Every queue was deployed at version 100 except syrupUSDG's, which arrived at version 200, and one
// transaction upgraded all of them but cashUSDT's. A queue that gains no upgrade block while it was
// deployed earlier calls userEscrowedShares where it reverts, which fails the whole Maple scan.
func TestMapleQueueUpgradeBlocksMatchTheFactoryHistory(t *testing.T) {
	for _, queue := range newMapleAdapter().(*MapleAdapter).queues {
		switch {
		case queue.Legacy:
			if queue.UpgradeBlock != 0 {
				t.Errorf("legacy queue %s has upgrade block %d", queue.Address, queue.UpgradeBlock)
			}
		case queue.ActivationBlock < mapleQueueV200Block:
			if queue.UpgradeBlock != mapleQueueV200Block {
				t.Errorf("queue %s deployed at %d upgrades at %d, want %d",
					queue.Address, queue.ActivationBlock, queue.UpgradeBlock, mapleQueueV200Block)
			}
		default:
			if queue.UpgradeBlock != 0 {
				t.Errorf("queue %s deployed at version 200 has upgrade block %d", queue.Address, queue.UpgradeBlock)
			}
		}
	}
}

type mapleQueueRPCFixture struct {
	t         *testing.T
	queue     mapleQueue
	owner     common.Address
	requestID *big.Int
	shares    *big.Int
	mu        sync.Mutex
	methods   []string
}

func (s *mapleQueueRPCFixture) answer(call rpcTestRequest) map[string]any {
	s.t.Helper()
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	if call.Method == "eth_chainId" {
		response["result"] = "0x1"
		return response
	}
	var input struct {
		To   common.Address `json:"to"`
		Data hexutil.Bytes  `json:"data"`
	}
	var blockText string
	if call.Method != "eth_call" || len(call.Params) != 2 ||
		json.Unmarshal(call.Params[0], &input) != nil || json.Unmarshal(call.Params[1], &blockText) != nil ||
		len(input.Data) < 4 || input.To != s.queue.Address {
		s.t.Errorf("unexpected RPC call %s %s", call.Method, call.Params)
		response["error"] = map[string]any{"code": -32601, "message": "unexpected RPC call"}
		return response
	}
	block, err := hexutil.DecodeUint64(blockText)
	if err != nil {
		s.t.Errorf("decode eth_call block %q: %v", blockText, err)
		response["error"] = map[string]any{"code": -32602, "message": "invalid block"}
		return response
	}
	method, err := mapleQueueABI.MethodById(input.Data[:4])
	if err != nil {
		s.t.Errorf("unexpected queue selector %x", input.Data[:4])
		response["error"] = map[string]any{"code": -32601, "message": "unexpected selector"}
		return response
	}
	s.mu.Lock()
	s.methods = append(s.methods, method.Name)
	s.mu.Unlock()
	var outputs []any
	switch method.Name {
	case "pool":
		outputs = []any{s.queue.Pool}
	case "asset":
		outputs = []any{s.queue.Asset.Address}
	case "userEscrowedShares":
		if block < s.queue.UpgradeBlock {
			// Version 100 has no such function, and the real queues revert exactly like this.
			response["error"] = map[string]any{"code": 3, "message": "execution reverted"}
			return response
		}
		outputs = []any{s.shares}
	case "requestIds":
		outputs = []any{s.requestID}
	case "requests":
		outputs = []any{s.owner, s.shares}
	}
	encoded, err := method.Outputs.Pack(outputs...)
	if err != nil {
		s.t.Fatalf("pack %s output: %v", method.Name, err)
	}
	response["result"] = hexutil.Encode(encoded)
	return response
}

func (s *mapleQueueRPCFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode singleton RPC call: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(s.answer(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode batch RPC calls: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = s.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

// Before the gate, every fixed-block scan between a queue's deployment and its upgrade to version
// 200 failed on the userEscrowedShares revert. The boundary is inclusive: the upgrade block itself
// reads the version 200 view, and the block before it reads the one request version 100 keeps.
func TestMapleQueueReadsLegacyRequestsBeforeItsUpgrade(t *testing.T) {
	owner := common.HexToAddress("0x0000000000000000000000000000000000000500")
	pool := common.HexToAddress("0x0000000000000000000000000000000000000200")
	queue := mapleQueue{
		Address:         common.HexToAddress("0x0000000000000000000000000000000000000100"),
		Pool:            pool,
		Asset:           Token{ChainID: Ethereum, Address: common.HexToAddress("0x0000000000000000000000000000000000000300"), Symbol: "USDC", Decimals: 6},
		Share:           Token{ChainID: Ethereum, Address: pool, Symbol: "syrupUSDC", Decimals: 6},
		ActivationBlock: 100,
		UpgradeBlock:    200,
		OutputShares:    true,
	}
	fixture := &mapleQueueRPCFixture{t: t, queue: queue, owner: owner, requestID: big.NewInt(7), shares: big.NewInt(1_000_000)}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	adapter := &MapleAdapter{queues: []mapleQueue{queue}}
	for _, test := range []struct {
		block     uint64
		method    string
		requestID any
		reads     []string
	}{
		{block: 199, method: "requests(requestIds(account))", requestID: "7", reads: []string{"pool", "asset", "requestIds", "requests"}},
		{block: 200, method: "userEscrowedShares(account)", reads: []string{"pool", "asset", "userEscrowedShares"}},
	} {
		fixture.methods = nil
		groups, err := adapter.queuePositions(
			context.Background(), client, BlockRef{ChainID: Ethereum, Number: test.block, Fixed: true}, owner,
		)
		if err != nil {
			t.Fatalf("block %d: %v", test.block, err)
		}
		if !slices.Equal(fixture.methods, test.reads) {
			t.Errorf("block %d read %v, want %v", test.block, fixture.methods, test.reads)
		}
		if len(groups) != 1 || len(groups[0].Components) != 1 {
			t.Fatalf("block %d groups = %+v, want one queue component", test.block, groups)
		}
		component := groups[0].Components[0]
		if component.Source.Method != test.method || component.AmountRaw != "1000000" || component.Token != queue.Share {
			t.Errorf("block %d component = %+v, want 1000000 %s from %s",
				test.block, component, queue.Share.Symbol, test.method)
		}
		if got := component.Metadata["requestId"]; got != test.requestID {
			t.Errorf("block %d requestId = %v, want %v", test.block, got, test.requestID)
		}
	}
}
