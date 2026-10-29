package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// startBlockRPC is a node that answers the one read an index start makes: its block's header.
type startBlockRPC struct {
	t         *testing.T
	chainID   ChainID
	block     uint64
	timestamp uint64
	reads     atomic.Int32
}

func (s *startBlockRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var call rpcTestRequest
	if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = hexutil.EncodeUint64(uint64(s.chainID))
	case "eth_getBlockByNumber":
		var tag string
		if len(call.Params) == 0 || json.Unmarshal(call.Params[0], &tag) != nil || tag != hexutil.EncodeUint64(s.block) {
			s.t.Errorf("unexpected block read %s", call.Params)
		}
		s.reads.Add(1)
		response["result"] = map[string]any{
			"number":    hexutil.EncodeUint64(s.block),
			"hash":      common.HexToHash("0x01"),
			"timestamp": hexutil.EncodeUint64(s.timestamp),
		}
	default:
		s.t.Errorf("unexpected RPC method %q", call.Method)
		response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(response)
}

// uncheckpointedMorphoIndex is Morpho's index, served by a processor that has passed processed but
// has not written its first checkpoint yet, and holds nothing for any account.
func uncheckpointedMorphoIndex(t *testing.T, chainID ChainID, processed uint64) *sentioIndex {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("8", []ChainID{chainID}, processed))
			return
		}
		_, _ = writer.Write([]byte(
			`{"data":{"indexerCheckpoints":[],"morphoMarketPositionRefs":[],"morphoVaultPositionRefs":[]}}`,
		))
	}))
	t.Cleanup(server.Close)
	index := newMorphoIndexer(SentioIndexerConfig{}).index
	index.api = &sentioAPIClient{apiKey: "test", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
	index.config = SentioIndexerConfig{
		GraphQLURL: server.URL + "/graphql", StatusURL: server.URL + "/status", ProcessorVersion: "8",
	}
	index.chains = []ChainID{chainID}
	return index
}

// A processor writes its first checkpoint on the first weekly boundary after its index starts, so
// a pin in between finds none although the index covers it. Morpho's index stands its deployment's
// activation in, the block its processor's checkpoint binding starts at, for the backfill lag and
// no longer: past it, a missing checkpoint is an error again.
func TestMorphoIndexStartStandsInForTheFirstCheckpoint(t *testing.T) {
	start := morphoDeployments[Plasma].Window.ActivationBlock
	const startTime = uint64(1_759_849_065)
	node := &startBlockRPC{t: t, chainID: Plasma, block: start, timestamp: startTime}
	server := httptest.NewServer(node)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Plasma, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	for _, test := range []struct {
		name     string
		sinceRun time.Duration
		accepted bool
	}{
		{name: "three days in", sinceRun: 3 * 24 * time.Hour, accepted: true},
		{name: "at the backfill lag", sinceRun: indexBackfillMaxLag, accepted: true},
		{name: "past the backfill lag", sinceRun: indexBackfillMaxLag + time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			pin := BlockRef{
				ChainID: Plasma, Number: start + 250_000,
				Timestamp: startTime + uint64(test.sinceRun/time.Second), Fixed: true,
			}
			index := uncheckpointedMorphoIndex(t, Plasma, pin.Number)
			at, err := index.read(context.Background(), client, pin,
				moolahMarketStream("morphoMarketPositionRefs", presenceTestAccount, morphoMaxIndexedMarkets),
				morphoVaultStream(presenceTestAccount),
			)
			if !test.accepted {
				if err == nil || !strings.Contains(err.Error(), "stale") {
					t.Fatalf("error = %v, want the checkpoint staleness error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if at.QueryBlock != pin.Number {
				t.Fatalf("read at %d, want the pin %d", at.QueryBlock, pin.Number)
			}
		})
	}
	if node.reads.Load() == 0 {
		t.Fatal("the index start's block was never read")
	}
}
