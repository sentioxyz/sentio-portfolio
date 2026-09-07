package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rpc"
)

// recordingObserver keeps every observation so a test can assert on what the kernel reported.
type recordingObserver struct {
	mu        sync.Mutex
	scans     []ScanObservation
	protocols []ProtocolObservation
	rpcs      []RPCObservation
	indexers  []IndexerObservation
}

func (r *recordingObserver) ObserveScan(observation ScanObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scans = append(r.scans, observation)
}

func (r *recordingObserver) ObserveProtocol(observation ProtocolObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.protocols = append(r.protocols, observation)
}

func (r *recordingObserver) ObserveRPC(observation RPCObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rpcs = append(r.rpcs, observation)
}

func (r *recordingObserver) ObserveIndexer(observation IndexerObservation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indexers = append(r.indexers, observation)
}

func TestScopeWithoutObserverIsNoop(t *testing.T) {
	scope := scopeFrom(context.Background())
	if _, ok := scope.observer.(noopObserver); !ok {
		t.Fatalf("observer = %T, want noopObserver", scope.observer)
	}
	// withDeployment on an unscoped context must not invent a scope.
	scoped := withDeployment(context.Background(), "aave-v3", Ethereum)
	if scoped.Value(scanScopeKey{}) != nil {
		t.Fatalf("withDeployment created a scope without an observer")
	}
	// A nil observer installs nothing.
	if withObserver(context.Background(), nil).Value(scanScopeKey{}) != nil {
		t.Fatalf("withObserver(nil) created a scope")
	}
}

func TestRPCClientReportsEveryRoundTrip(t *testing.T) {
	calls := rpcTestCalls()
	encoded, err := calls[0].ABI.Methods["value"].Outputs.Pack(big.NewInt(7))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&rpcTestServer{t: t, retryable: true, result: "0x" + common.Bytes2Hex(encoded)})
	defer server.Close()

	recorder := &recordingObserver{}
	ctx := withDeployment(withObserver(context.Background(), recorder), "aave-v3", Ethereum)
	client, err := DialRPC(ctx, Ethereum, server.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	block := BlockRef{ChainID: Ethereum, Number: 100}
	// The first batch attempt fails with a retryable element error; the second succeeds.
	if _, err := client.ParallelCalls(ctx, block, calls); err != nil {
		t.Fatalf("parallel calls: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.rpcs) != 3 {
		t.Fatalf("rpc observations = %d, want eth_chainId plus two batch attempts: %+v", len(recorder.rpcs), recorder.rpcs)
	}
	chainID := recorder.rpcs[0]
	if chainID.Method != "eth_chainId" || chainID.BatchSize != 0 || chainID.Attempt != 1 || chainID.Err != nil {
		t.Fatalf("eth_chainId observation = %+v", chainID)
	}
	if chainID.ChainID != Ethereum || chainID.ProtocolID != "aave-v3" {
		t.Fatalf("eth_chainId observation carried scope %d/%q, want 1/aave-v3", chainID.ChainID, chainID.ProtocolID)
	}
	first, second := recorder.rpcs[1], recorder.rpcs[2]
	if first.Method != "eth_call" || first.BatchSize != 2 || first.Attempt != 1 || first.Err == nil {
		t.Fatalf("first batch observation = %+v, want a failed eth_call batch of 2", first)
	}
	if second.Method != "eth_call" || second.BatchSize != 2 || second.Attempt != 2 || second.Err != nil {
		t.Fatalf("second batch observation = %+v, want a successful retry", second)
	}
	for _, observation := range recorder.rpcs {
		if observation.Duration <= 0 {
			t.Fatalf("observation without a duration: %+v", observation)
		}
	}
}

func TestRPCObservationErrorsAreRedacted(t *testing.T) {
	recorder := &recordingObserver{}
	ctx := withObserver(context.Background(), recorder)
	// A closed server makes every attempt fail with a transport error that quotes the URL.
	server := httptest.NewServer(http.NotFoundHandler())
	endpoint := server.URL
	server.Close()
	if _, err := DialRPC(ctx, Ethereum, endpoint); err == nil {
		t.Fatalf("dial against a closed server succeeded")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.rpcs) == 0 {
		t.Fatalf("failed dial reported no RPC observations")
	}
	for _, observation := range recorder.rpcs {
		if observation.Err == nil {
			t.Fatalf("failed attempt reported without an error: %+v", observation)
		}
		if strings.Contains(observation.Err.Error(), endpoint) {
			t.Fatalf("observation error quotes the endpoint: %v", observation.Err)
		}
	}
}

func TestBatchMethodNamesHomogeneousBatches(t *testing.T) {
	if got := batchMethod(nil); got != "batch" {
		t.Fatalf("empty batch method = %q", got)
	}
	homogeneous := []rpc.BatchElem{{Method: "eth_getCode"}, {Method: "eth_getCode"}}
	if got := batchMethod(homogeneous); got != "eth_getCode" {
		t.Fatalf("homogeneous batch method = %q", got)
	}
	mixed := []rpc.BatchElem{{Method: "eth_call"}, {Method: "eth_getCode"}}
	if got := batchMethod(mixed); got != "batch" {
		t.Fatalf("mixed batch method = %q", got)
	}
}

func TestSentioAPIClientReportsRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"processors":[{"version":42,"versionState":"ACTIVE",
				"processorStatus":{"state":"PROCESSING"},"states":[
				{"chainId":"1","processedBlockNumber":"100","estimatedLatestBlockNumber":"100",
				"status":{"state":"PROCESSING_LATEST"}}]}]}`)
			return
		}
		http.Error(w, "bad query", http.StatusBadRequest)
	}))
	defer server.Close()

	recorder := &recordingObserver{}
	ctx := withDeployment(withObserver(context.Background(), recorder), "morpho-blue", Base)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
	config := SentioIndexerConfig{GraphQLURL: server.URL + "/graphql", StatusURL: server.URL + "/status", ProcessorVersion: "42"}

	if _, err := client.chainStatuses(ctx, config, []ChainID{Ethereum}, false); err != nil {
		t.Fatalf("status: %v", err)
	}
	var payload json.RawMessage
	if err := client.doJSON(ctx, http.MethodPost, config.GraphQLURL, map[string]any{"query": "{}"}, &payload); err == nil {
		t.Fatalf("rejected GraphQL query succeeded")
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.indexers) != 2 {
		t.Fatalf("indexer observations = %+v, want one status and one graphql", recorder.indexers)
	}
	status := recorder.indexers[0]
	if status.Kind != IndexerStatus || status.ProtocolID != "morpho-blue" || status.ChainID != 0 ||
		status.Attempt != 1 || status.Err != nil || status.Duration <= 0 {
		t.Fatalf("status observation = %+v", status)
	}
	graphql := recorder.indexers[1]
	if graphql.Kind != IndexerGraphQL || graphql.ProtocolID != "morpho-blue" || graphql.ChainID != Base ||
		graphql.Attempt != 1 || graphql.Err == nil {
		t.Fatalf("graphql observation = %+v", graphql)
	}
}

func TestSentioAPIClientReportsEveryRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	recorder := &recordingObserver{}
	ctx := withObserver(context.Background(), recorder)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
	var payload json.RawMessage
	if err := client.doJSON(ctx, http.MethodGet, server.URL, nil, &payload); err == nil {
		t.Fatalf("unavailable endpoint succeeded")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if attempts != 3 || len(recorder.indexers) != 3 {
		t.Fatalf("attempts = %d, observations = %d, want 3 and 3", attempts, len(recorder.indexers))
	}
	for index, observation := range recorder.indexers {
		if observation.Attempt != index+1 || observation.Err == nil || observation.Kind != IndexerStatus {
			t.Fatalf("observation %d = %+v", index, observation)
		}
	}
}

func TestSentioLaneReportsWait(t *testing.T) {
	recorder := &recordingObserver{}
	ctx := withDeployment(withObserver(context.Background(), recorder), "pendle", Arbitrum)

	// A lane of one slot makes the second acquisition wait for the first to release.
	lane := newIndexerLane(1)
	scoped := withScanLane(ctx, lane)
	if err := lockSentioLane(withScanLane(context.Background(), lane)); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		unlockSentioLane(withScanLane(context.Background(), lane))
		close(released)
	}()
	if err := lockSentioLane(scoped); err != nil {
		t.Fatal(err)
	}
	unlockSentioLane(scoped)
	<-released

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.indexers) != 1 {
		t.Fatalf("lane observations = %+v, want exactly one for the scoped acquisition", recorder.indexers)
	}
	observation := recorder.indexers[0]
	if observation.Kind != IndexerLane || observation.ProtocolID != "pendle" ||
		observation.ChainID != Arbitrum || observation.Attempt != 0 {
		t.Fatalf("lane observation = %+v", observation)
	}
	if observation.Duration < 15*time.Millisecond {
		t.Fatalf("lane wait = %s, want at least the 20ms the lane was held", observation.Duration)
	}
}

func TestEngineReportsScanObservation(t *testing.T) {
	recorder := &recordingObserver{}
	// No RPC URLs: every chain fails during setup, so the scan reports without any network.
	engine := NewEngineWithConfig(nil, nil, EngineConfig{Observer: recorder})
	options := ScanOptions{
		ChainIDs:    map[ChainID]struct{}{Ethereum: {}, Base: {}},
		ProtocolIDs: map[string]struct{}{"aave-v3": {}},
		SkipPrices:  true,
	}
	response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), options)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.scans) != 1 {
		t.Fatalf("scan observations = %d, want 1", len(recorder.scans))
	}
	scan := recorder.scans[0]
	if scan.Chains != 0 || scan.Deployments != 0 || scan.Snapshots != 0 || scan.Canceled {
		t.Fatalf("scan observation = %+v, want no pinned chains and no deployments", scan)
	}
	if len(scan.Errors) != len(response.Errors) || len(scan.Errors) != 2 {
		t.Fatalf("scan errors = %+v, want the response's two chain errors", scan.Errors)
	}
	if scan.Duration <= 0 || scan.ChainSetup <= 0 {
		t.Fatalf("scan observation lacks durations: %+v", scan)
	}
	if scan.WalletDiscovery != 0 || scan.Pricing != 0 {
		t.Fatalf("excluded phases reported durations: %+v", scan)
	}
	if !scan.Options.SkipPrices || len(scan.Options.ProtocolIDs) != 1 {
		t.Fatalf("scan observation lost its options: %+v", scan.Options)
	}
	if len(recorder.protocols) != 0 {
		t.Fatalf("protocol observations = %+v, want none without a pinned chain", recorder.protocols)
	}
}

func TestEngineWithoutObserverStillScans(t *testing.T) {
	engine := NewEngine(nil, nil)
	response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), ScanOptions{
		ChainIDs: map[ChainID]struct{}{Ethereum: {}},
	})
	if len(response.Errors) != 1 || response.Errors[0].Scope != "chain" {
		t.Fatalf("errors = %+v, want the single unconfigured chain", response.Errors)
	}
}
