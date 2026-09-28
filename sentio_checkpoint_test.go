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

// uncheckpointedMorphoIndex is a Morpho index whose processor has passed processed but has not
// written its first checkpoint yet, and holds nothing for any account.
func uncheckpointedMorphoIndex(t *testing.T, chainID ChainID, processed uint64) *morphoIndexer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("8", []ChainID{chainID}, processed))
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
			return
		}
		if !strings.Contains(body.Query, "query MorphoWalletPositions") {
			// The presence probe finds no checkpoint either, so it proves nothing.
			_, _ = writer.Write([]byte(`{"data":{}}`))
			return
		}
		_, _ = writer.Write([]byte(
			`{"data":{"indexerCheckpoints":[],"morphoMarketPositionRefs":[],"morphoVaultPositionRefs":[]}}`,
		))
	}))
	t.Cleanup(server.Close)
	return &morphoIndexer{
		api: &sentioAPIClient{
			apiKey:     "test",
			httpClient: server.Client(),
			statuses:   make(map[string]sentioStatusCache),
		},
		config: SentioIndexerConfig{
			GraphQLURL:       server.URL + "/graphql",
			StatusURL:        server.URL + "/status",
			ProcessorVersion: "8",
		},
		requiredChains: []ChainID{chainID},
	}
}

// A processor writes its first checkpoint on the first weekly boundary after its index starts, so
// a pin in between finds none although the index covers it. Before the index start stood in, every
// such scan failed with "GraphQL returned 0 checkpoints". The stand-in holds for the backfill lag
// and no longer: past it, a missing checkpoint is an error again.
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
		{name: "at the backfill lag", sinceRun: morphoBackfillMaxLag, accepted: true},
		{name: "past the backfill lag", sinceRun: morphoBackfillMaxLag + time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			pin := BlockRef{
				ChainID: Plasma, Number: start + 250_000,
				Timestamp: startTime + uint64(test.sinceRun/time.Second), Fixed: true,
			}
			indexer := uncheckpointedMorphoIndex(t, Plasma, pin.Number)
			ctx := presenceScanContext(map[ChainID]BlockRef{Plasma: pin})
			refs, err := indexer.indexedRefs(ctx, pin, presenceTestAccount, false, indexStart{client: client, block: start})
			if !test.accepted {
				if err == nil || !strings.Contains(err.Error(), "stale") {
					t.Fatalf("error = %v, want the checkpoint staleness error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if refs.IndexerBlock != pin.Number || len(refs.MarketIDs) != 0 || len(refs.Vaults) != 0 {
				t.Fatalf("refs = %+v, want an empty index read at block %d", refs, pin.Number)
			}
		})
	}
	if node.reads.Load() == 0 {
		t.Fatal("the index start's block was never read")
	}
}

// A written checkpoint is read as is, without touching the chain; the nil client proves it.
func TestIndexStartOnlyStandsInWithoutACheckpoint(t *testing.T) {
	start := indexStart{block: 100}
	block, timestampMS, err := start.checkpoint(context.Background(), []indexerCheckpointRow{
		{BlockNumber: "250", TimestampMS: "1700000000000"},
	})
	if err != nil || block != 250 || timestampMS != 1_700_000_000_000 {
		t.Fatalf("checkpoint = %d %d %v, want the written row", block, timestampMS, err)
	}
	_, _, err = start.checkpoint(context.Background(), []indexerCheckpointRow{
		{BlockNumber: "250", TimestampMS: "1700000000000"},
		{BlockNumber: "251", TimestampMS: "1700000001000"},
	})
	if err == nil || !strings.Contains(err.Error(), "GraphQL returned 2 checkpoints") {
		t.Fatalf("error = %v, want two checkpoints rejected", err)
	}
}
