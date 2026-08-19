package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// laggingPool imitates an RPC pool whose backends are behind the block it advertises: it answers
// eth_blockNumber / "latest" with announcedHead, but rejects any eth_call above servedHead the
// way a real node does. This is the shape that fails a live scan for a block that plainly exists.
type laggingPool struct {
	t             *testing.T
	announcedHead uint64
	servedHead    uint64
	callBlocks    []uint64
}

func (p *laggingPool) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		p.t.Fatalf("decode request: %v", err)
	}
	writer.Header().Set("Content-Type", "application/json")

	respond := func(id json.RawMessage, result any, errMessage string) map[string]any {
		out := map[string]any{"jsonrpc": "2.0", "id": id}
		if errMessage != "" {
			out["error"] = map[string]any{"code": -32000, "message": errMessage}
		} else {
			out["result"] = result
		}
		return out
	}

	handle := func(call rpcTestRequest) map[string]any {
		switch call.Method {
		case "eth_chainId":
			return respond(call.ID, "0x1", "")
		case "eth_getBlockByNumber":
			var tag string
			_ = json.Unmarshal(call.Params[0], &tag)
			number := p.announcedHead
			if tag != "latest" {
				parsed := new(big.Int)
				parsed.SetString(strings.TrimPrefix(tag, "0x"), 16)
				number = parsed.Uint64()
			}
			return respond(call.ID, map[string]any{
				"number":    "0x" + big.NewInt(int64(number)).Text(16),
				"hash":      common.BytesToHash([]byte{byte(number)}).Hex(),
				"timestamp": "0x1",
			}, "")
		case "eth_call":
			var object map[string]any
			_ = json.Unmarshal(call.Params[0], &object)
			var tag string
			_ = json.Unmarshal(call.Params[1], &tag)
			parsed := new(big.Int)
			parsed.SetString(strings.TrimPrefix(tag, "0x"), 16)
			p.callBlocks = append(p.callBlocks, parsed.Uint64())
			if parsed.Uint64() > p.servedHead {
				return respond(call.ID, nil, "block "+parsed.String()+
					" is beyond the latest block "+big.NewInt(int64(p.servedHead)).String()+" of this node, retry later")
			}
			// Enough zero words to decode as whatever the caller expects. This pool exists to
			// exercise block selection, not contract semantics.
			return respond(call.ID, "0x"+strings.Repeat("00", 32*8), "")
		default:
			p.t.Fatalf("unexpected method %q", call.Method)
			return nil
		}
	}

	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			p.t.Fatal(err)
		}
		_ = json.NewEncoder(writer).Encode(handle(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		p.t.Fatal(err)
	}
	responses := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		responses = append(responses, handle(call))
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

var headLagTestABI = MustABI(`[
  {"type":"function","name":"value","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

func newLaggingPoolClient(t *testing.T, announced, served uint64) (*RPCClient, *laggingPool) {
	t.Helper()
	pool := &laggingPool{t: t, announcedHead: announced, servedHead: served}
	server := httptest.NewServer(pool)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, pool
}

// The announced head is unusable while backends trail it; the settled head is usable. This is the
// whole point of the margin, so assert both halves rather than only the fix.
func TestLatestSettledBlockAvoidsTheAdvertisedHead(t *testing.T) {
	const announced, served = 1_000, 997
	client, _ := newLaggingPoolClient(t, announced, served)
	ctx := context.Background()
	contract := common.HexToAddress("0x1")

	head, err := client.LatestBlock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.Number != announced {
		t.Fatalf("advertised head = %d, want %d", head.Number, announced)
	}
	if _, err := client.Call(ctx, head, contract, headLagTestABI, "value"); err == nil {
		t.Fatal("expected the advertised head to be rejected by a lagging backend")
	} else if !strings.Contains(err.Error(), "beyond the latest block") {
		t.Fatalf("unexpected failure at the advertised head: %v", err)
	}

	settled, err := client.LatestSettledBlock(ctx, defaultHeadLagBlocks)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Number != announced-defaultHeadLagBlocks {
		t.Fatalf("settled head = %d, want %d", settled.Number, announced-defaultHeadLagBlocks)
	}
	if _, err := client.Call(ctx, settled, contract, headLagTestABI, "value"); err != nil {
		t.Fatalf("settled head must be usable, got: %v", err)
	}
}

func TestLatestSettledBlockDegradesSafely(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name           string
		announced, lag uint64
		want           uint64
	}{
		{name: "no lag requested", announced: 1_000, lag: 0, want: 1_000},
		{name: "lag equals head", announced: 4, lag: 4, want: 4},
		{name: "lag exceeds head", announced: 3, lag: 10, want: 3},
		{name: "normal margin", announced: 1_000, lag: 4, want: 996},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newLaggingPoolClient(t, test.announced, test.announced)
			block, err := client.LatestSettledBlock(ctx, test.lag)
			if err != nil {
				t.Fatal(err)
			}
			if block.Number != test.want {
				t.Fatalf("settled block = %d, want %d", block.Number, test.want)
			}
		})
	}
}

func TestEngineConfigHeadLagDefaultAndOverride(t *testing.T) {
	if got := (EngineConfig{}).headLagBlocks(); got != defaultHeadLagBlocks {
		t.Fatalf("default head lag = %d, want %d", got, defaultHeadLagBlocks)
	}
	if got := (EngineConfig{HeadLagBlocks: 1}).headLagBlocks(); got != 1 {
		t.Fatalf("override head lag = %d, want 1", got)
	}
	engine := NewEngine(map[ChainID]string{}, nil)
	if engine.headLagBlocks != defaultHeadLagBlocks {
		t.Fatalf("engine head lag = %d, want %d", engine.headLagBlocks, defaultHeadLagBlocks)
	}
}

// The engine's live path must pin to the settled head, and must report the block it actually
// used. Scanning a lagging pool proves the wiring, not just the helper: with the margin the
// reported block is the settled one and no chain-scoped rejection appears.
func TestEngineLiveScanPinsToTheSettledHead(t *testing.T) {
	const announced, served = 5_000, 4_997
	pool := &laggingPool{t: t, announcedHead: announced, servedHead: served}
	server := httptest.NewServer(pool)
	t.Cleanup(server.Close)

	engine := NewEngineWithConfig(
		map[ChainID]string{Ethereum: server.URL}, nil, EngineConfig{},
	)
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
		ScanOptions{ProtocolIDs: map[string]struct{}{"no-such-protocol": {}}},
	)
	got, scanned := response.ChainBlocks[Ethereum]
	if !scanned {
		t.Fatalf("chain was not scanned; errors=%v", response.Errors)
	}
	if got != announced-defaultHeadLagBlocks {
		t.Fatalf("scanned block = %d, want the settled head %d", got, announced-defaultHeadLagBlocks)
	}
	for _, scanError := range response.Errors {
		if strings.Contains(scanError.Message, "beyond the latest block") {
			t.Fatalf("live scan still tripped over the advertised head: %s", scanError.Message)
		}
	}
}
