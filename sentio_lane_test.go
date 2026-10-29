package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// withScanLane opens a scan scope around lane, keeping any observer the context already carries.
func withScanLane(ctx context.Context, lane *indexerLane) context.Context {
	scope := scopeFrom(ctx)
	scope.lane = lane
	if scope.memo == nil {
		scope.memo = newScanMemo()
	}
	return context.WithValue(ctx, scanScopeKey{}, scope)
}

// mustLockLane takes a lane slot for a test and returns its release function.
func mustLockLane(t *testing.T, ctx context.Context) func() {
	t.Helper()
	release, err := lockSentioLane(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

// waitForLaneStats polls the lane until it reports want, since a waiter registers asynchronously.
func waitForLaneStats(t *testing.T, lane *indexerLane, want IndexerLaneStats) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for lane.stats() != want {
		if time.Now().After(deadline) {
			t.Fatalf("lane stats = %+v, want %+v", lane.stats(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIndexerLaneAdmitsUpToItsConcurrency(t *testing.T) {
	lane := newIndexerLane(2)
	ctx := withScanLane(context.Background(), lane)
	first := mustLockLane(t, ctx)
	second := mustLockLane(t, ctx)
	type acquisition struct {
		release func()
		err     error
	}
	third := make(chan acquisition, 1)
	go func() {
		release, err := lockSentioLane(ctx)
		third <- acquisition{release, err}
	}()
	select {
	case result := <-third:
		t.Fatalf("third acquisition did not wait: %v", result.err)
	case <-time.After(30 * time.Millisecond):
	}
	first()
	select {
	case result := <-third:
		if result.err != nil {
			t.Fatalf("third acquisition after a release: %v", result.err)
		}
		result.release()
	case <-time.After(time.Second):
		t.Fatalf("third acquisition did not proceed after a release")
	}
	second()
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2})
}

func TestIndexerLaneHonoursCancellationWhileWaiting(t *testing.T) {
	lane := newIndexerLane(1)
	holder := withScanLane(context.Background(), lane)
	defer mustLockLane(t, holder)()
	ctx, cancel := context.WithTimeout(withScanLane(context.Background(), lane), 20*time.Millisecond)
	defer cancel()
	release, err := lockSentioLane(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatalf("waiting acquisition = (%v, %v), want no slot and deadline exceeded", release != nil, err)
	}
	// The abandoned wait is no longer counted, and the holder's slot is still the only one in use.
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 1, InUse: 1})
}

func TestIndexerLaneReleaseIsIdempotent(t *testing.T) {
	lane := newIndexerLane(1)
	ctx := withScanLane(context.Background(), lane)
	release := mustLockLane(t, ctx)
	done := make(chan struct{})
	go func() {
		// A second release must not take another value out of the lane: that would block on an
		// empty lane, or free a slot someone else holds.
		release()
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("a repeated release blocked")
	}
	other := mustLockLane(t, ctx)
	release()
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 1, InUse: 1})
	other()
}

func TestIndexerLaneReportsItsStats(t *testing.T) {
	lane := newIndexerLane(2)
	ctx := withScanLane(context.Background(), lane)
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2})
	first := mustLockLane(t, ctx)
	second := mustLockLane(t, ctx)
	// Taking free slots is not waiting.
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2, InUse: 2})
	waiterCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		_, err := lockSentioLane(waiterCtx)
		waited <- err
	}()
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2, InUse: 2, Waiting: 1})
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v, want canceled", err)
	}
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2, InUse: 2})
	first()
	second()
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 2})

	engine := NewEngineWithConfig(nil, nil, EngineConfig{IndexerConcurrency: 16})
	if got := engine.IndexerLaneStats(); got != (IndexerLaneStats{Capacity: 16}) {
		t.Fatalf("engine lane stats = %+v, want an idle lane of 16", got)
	}
}

func TestIndexerLaneDefaults(t *testing.T) {
	if got := cap(newIndexerLane(0).slots); got != defaultIndexerConcurrency {
		t.Fatalf("zero concurrency lane capacity = %d, want %d", got, defaultIndexerConcurrency)
	}
	if got := cap(newIndexerLane(-3).slots); got != defaultIndexerConcurrency {
		t.Fatalf("negative concurrency lane capacity = %d, want %d", got, defaultIndexerConcurrency)
	}
	if laneFrom(context.Background()) != defaultIndexerLane {
		t.Fatalf("a context without a scan scope must use the default lane")
	}
	engine := NewEngineWithConfig(nil, nil, EngineConfig{IndexerConcurrency: 7})
	if got := cap(engine.indexerLane.slots); got != 7 {
		t.Fatalf("engine lane capacity = %d, want 7", got)
	}
	if got := cap(NewEngine(nil, nil).indexerLane.slots); got != defaultIndexerConcurrency {
		t.Fatalf("default engine lane capacity = %d, want %d", got, defaultIndexerConcurrency)
	}
}

func TestScanMemoComputesOnce(t *testing.T) {
	memo := newScanMemo()
	var computed atomic.Int32
	var wait sync.WaitGroup
	results := make([]any, 8)
	for index := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index] = memo.once(context.Background(), "key", func() any {
				computed.Add(1)
				time.Sleep(5 * time.Millisecond)
				return 42
			})
		}(index)
	}
	wait.Wait()
	if computed.Load() != 1 {
		t.Fatalf("compute ran %d times, want once", computed.Load())
	}
	for index, result := range results {
		if result != 42 {
			t.Fatalf("caller %d got %v, want 42", index, result)
		}
	}
	if memo.once(context.Background(), "other", func() any { return "fresh" }) != "fresh" {
		t.Fatalf("a different key reused a cached value")
	}
	var nilMemo *scanMemo
	if nilMemo.once(context.Background(), "key", func() any { return "computed" }) != "computed" {
		t.Fatalf("a nil memo must compute every time")
	}
}

// Every indexer request must take a lane slot, including the market lookup valuation makes after
// PositionRefs. With a single slot held elsewhere, the lookup waits.
func TestPendleMarketLookupWaitsForTheLane(t *testing.T) {
	pt := common.HexToAddress("0x1111111111111111111111111111111111111111")
	market := common.HexToAddress("0x2222222222222222222222222222222222222222")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
			"indexerCheckpoints": []map[string]any{{"blockNumber": "100", "timestampMs": "1000"}},
			"pendleTokens": []map[string]any{{
				"id":      pendleTokenRowID(Ethereum, market),
				"chainId": int(Ethereum),
				"address": strings.ToLower(market.Hex()),
				"kind":    string(pendleLP),
				"pt":      strings.ToLower(pt.Hex()),
			}},
		}})
	}))
	t.Cleanup(server.Close)
	indexer := newPendleIndexer(SentioIndexerConfig{GraphQLURL: server.URL, StatusURL: server.URL, ProcessorVersion: "2"})
	indexer.index.api = &sentioAPIClient{apiKey: "test", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
	// Where PositionRefs read: the lookup asks there, held to the same checkpoint.
	at := indexAt{
		index: indexer.index, block: BlockRef{ChainID: Ethereum, Number: 100}, QueryBlock: 100,
		checkpoint: indexCheckpoint{written: true, block: 100, ms: 1000},
	}

	lane := newIndexerLane(1)
	holder := withScanLane(context.Background(), lane)
	releaseHolder := mustLockLane(t, holder)
	type outcome struct {
		markets map[common.Address][]common.Address
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		markets, err := indexer.MarketsForPT(withScanLane(context.Background(), lane), at, []common.Address{pt})
		done <- outcome{markets, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("market lookup finished while the only slot was held: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	if requests.Load() != 0 {
		t.Fatalf("market lookup sent %d requests while the only slot was held", requests.Load())
	}
	releaseHolder()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if got := result.markets[pt]; len(got) != 1 || got[0] != market {
			t.Fatalf("markets = %+v, want the one indexed market", result.markets)
		}
	case <-time.After(time.Second):
		t.Fatalf("market lookup did not proceed after the slot was released")
	}
	if requests.Load() != 1 {
		t.Fatalf("market lookup sent %d requests, want 1", requests.Load())
	}
	// The slot is free again afterwards.
	mustLockLane(t, holder)()
}

func TestScanMemoStartComputesInTheBackground(t *testing.T) {
	memo := newScanMemo()
	release := make(chan struct{})
	var computed atomic.Int32
	memo.start("key", func() any {
		computed.Add(1)
		<-release
		return "prefetched"
	})
	memo.start("key", func() any {
		computed.Add(1)
		return "duplicate"
	})
	waited := make(chan any, 1)
	go func() {
		waited <- memo.once(context.Background(), "key", func() any { computed.Add(1); return "recomputed" })
	}()
	select {
	case value := <-waited:
		t.Fatalf("once returned %v before the background computation finished", value)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case value := <-waited:
		if value != "prefetched" {
			t.Fatalf("once returned %v, want the prefetched value", value)
		}
	case <-time.After(time.Second):
		t.Fatalf("once did not return after the background computation finished")
	}
	if computed.Load() != 1 {
		t.Fatalf("compute ran %d times, want once", computed.Load())
	}
	var nilMemo *scanMemo
	nilMemo.start("key", func() any { t.Fatalf("a nil memo must not start anything"); return nil })
}

const (
	emptyIndexBlock     = uint64(100_000_000)
	emptyIndexTimestamp = uint64(1_700_000_000)
)

// emptyIndexFields are the entity fields every index the kernel reads asks for.
var emptyIndexFields = []string{
	"morphoMarketPositionRefs", "morphoVaultPositionRefs", "morphoFeeMarkets",
	"pendlePositionRefs", "pendleTokens",
	"eulerPositionRefs", "eulerRewardTokens", "eulerVaults",
	"listaMarketPositionRefs", "listaVaultPositionRefs", "listaFeeMarkets",
	"portfolioOwnerTokenRefs", "portfolioAccountRequestRefs",
	"positions", "withdrawalRequests",
}

// emptyIndexServer stands in for any index the kernel reads, for an account holding nothing: a
// status read reports every chain processed up to emptyIndexBlock, and every query — a page of
// any indexer or a presence probe — finds a fresh checkpoint and no rows.
type emptyIndexServer struct {
	statuses atomic.Int32
	queries  atomic.Int32
}

func (s *emptyIndexServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("content-type", "application/json")
	if request.Method == http.MethodGet {
		s.statuses.Add(1)
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("1", SupportedChainIDs, emptyIndexBlock))
		return
	}
	s.queries.Add(1)
	checkpoint := []map[string]any{{
		"blockNumber": strconv.FormatUint(emptyIndexBlock, 10),
		"timestampMs": strconv.FormatUint(emptyIndexTimestamp*1_000, 10),
	}}
	data := map[string]any{"indexerCheckpoints": checkpoint}
	for _, field := range emptyIndexFields {
		data[field] = []any{}
	}
	for _, chainID := range SupportedChainIDs {
		data[presenceAlias("cp", chainID)] = checkpoint
		data[presenceAlias("f0_", chainID)] = []any{}
		data[presenceAlias("f1_", chainID)] = []any{}
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
}

// laneOfOneCase is one index-backed read, run the way a chain job runs it.
type laneOfOneCase struct {
	name     string
	chainID  ChainID
	prefetch func(ctx context.Context)
	read     func(ctx context.Context) error
}

// laneOfOneCases builds every index-backed read against server, each with its own client and so
// its own cold status cache.
func laneOfOneCases(t *testing.T, server *httptest.Server, account common.Address) []laneOfOneCase {
	t.Helper()
	config := SentioIndexerConfig{GraphQLURL: server.URL + "/graphql", StatusURL: server.URL + "/status", ProcessorVersion: "1"}
	block := func(chainID ChainID) BlockRef {
		return BlockRef{ChainID: chainID, Number: emptyIndexBlock, Timestamp: emptyIndexTimestamp}
	}
	morpho := newMorphoIndexer(config)
	morphoRPC := newContractStub(t, Ethereum, account)
	morphoRPC.answer(morphoDeployments[Ethereum].Morpho, morphoCoreABI, "feeRecipient", nil, common.Address{})
	morphoClient := morphoRPC.dial()
	lista := newListaIndexer(config)
	listaRPC := newContractStub(t, BSC, account)
	listaRPC.answer(listaMoolahDeployments[BSC].Address, listaMoolahABI, "feeRecipient", nil, common.Address{})
	listaClient := listaRPC.dial()
	pendle := newPendleIndexer(config)
	// The generations are separate projects, so each has a status of its own to refresh.
	uniswapV4Config := config
	uniswapV4Config.StatusURL += "/v4"
	uniswap := newUniswapIndexer(config, uniswapV4Config)
	euler := newEulerIndexer(config)
	ownerTokens := newOwnerTokenIndexer(config, []ChainID{Ethereum})
	requests := newAccountRequestIndexer(config, []ChainID{BSC})
	meth := newMethAdapter(config).(*MethAdapter)
	contracts := []common.Address{common.HexToAddress("0x00000000000000000000000000000000000000c1")}
	return []laneOfOneCase{
		{
			name: "morpho-blue", chainID: Ethereum,
			prefetch: func(ctx context.Context) { morpho.prefetchPresence(ctx, account) },
			read: func(ctx context.Context) error {
				_, err := morpho.PositionRefs(ctx, morphoClient, block(Ethereum), account, morphoDeployments[Ethereum])
				return err
			},
		},
		{
			name: "pendle", chainID: Ethereum,
			prefetch: func(ctx context.Context) { pendle.prefetchPresence(ctx, account) },
			read: func(ctx context.Context) error {
				_, _, err := pendle.PositionRefs(ctx, nil, block(Ethereum), account)
				return err
			},
		},
		{
			name: "uniswap-v3", chainID: Ethereum,
			prefetch: func(ctx context.Context) { uniswap.prefetchPresence(ctx, uniswapV3, account) },
			read: func(ctx context.Context) error {
				_, err := uniswap.indexedNFTs(ctx, uniswapV3, block(Ethereum), account)
				return err
			},
		},
		{
			name: "uniswap-v4", chainID: Ethereum,
			prefetch: func(ctx context.Context) { uniswap.prefetchPresence(ctx, uniswapV4, account) },
			read: func(ctx context.Context) error {
				_, err := uniswap.indexedNFTs(ctx, uniswapV4, block(Ethereum), account)
				return err
			},
		},
		{
			name: "lista", chainID: BSC,
			read: func(ctx context.Context) error {
				_, err := lista.PositionRefs(ctx, listaClient, block(BSC), account, listaMoolahDeployments[BSC])
				return err
			},
		},
		{
			name: "euler-v2", chainID: Ethereum,
			read: func(ctx context.Context) error {
				_, err := euler.PositionRefs(ctx, nil, block(Ethereum), account)
				return err
			},
		},
		{
			name: "owner-token", chainID: Ethereum,
			read: func(ctx context.Context) error {
				_, err := ownerTokens.PositionRefs(ctx, block(Ethereum), account, contracts)
				return err
			},
		},
		{
			name: "account-request", chainID: BSC,
			read: func(ctx context.Context) error {
				_, err := requests.IndexedRefs(ctx, block(BSC), account, contracts)
				return err
			},
		},
		{
			name: "meth-protocol", chainID: Ethereum,
			read: func(ctx context.Context) error {
				_, err := meth.requestIDs(ctx, block(Ethereum), account)
				return err
			},
		},
	}
}

// Every index-backed read completes on a lane of one slot with a cold status cache, and so does a
// presence probe prefetched ahead of it: nothing on the path waits for a second slot.
//
// The index holds nothing, so a probing indexer proves every chain empty: its chain job takes no
// slot at all, and the probe takes one only to send. Every other indexer holds one for its pages,
// refreshing the status while it does.
func TestIndexReadsCompleteOnALaneOfOne(t *testing.T) {
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	account := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	pinned := make(map[ChainID]BlockRef, len(SupportedChainIDs))
	for _, chainID := range SupportedChainIDs {
		pinned[chainID] = BlockRef{ChainID: chainID, Number: emptyIndexBlock, Timestamp: emptyIndexTimestamp}
	}
	for _, prefetched := range []bool{false, true} {
		index := &emptyIndexServer{}
		server := httptest.NewServer(index)
		t.Cleanup(server.Close)
		for _, test := range laneOfOneCases(t, server, account) {
			if prefetched && test.prefetch == nil {
				continue
			}
			t.Run(fmt.Sprintf("%s/prefetched=%t", test.name, prefetched), func(t *testing.T) {
				lane := newIndexerLane(1)
				recorder := &recordingObserver{}
				scan := withPinnedBlocks(withScan(context.Background(), recorder, lane), pinned, nil)
				statusesBefore, queriesBefore := index.statuses.Load(), index.queries.Load()
				if prefetched {
					test.prefetch(withDeployment(scan, test.name, 0))
				}
				done := make(chan error, 1)
				go func() { done <- test.read(withDeployment(scan, test.name, test.chainID)) }()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("read did not finish on a lane of one: lane = %+v", lane.stats())
				}
				if index.statuses.Load() == statusesBefore {
					t.Fatalf("no status read reached the index, so the cache was not cold")
				}
				waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 1})

				recorder.mu.Lock()
				pageSlots, probeSlots := 0, 0
				for _, observation := range recorder.indexers {
					switch {
					case observation.Kind != IndexerLaneHold:
					case observation.Presence:
						probeSlots++
					default:
						pageSlots++
					}
				}
				recorder.mu.Unlock()
				wantPageSlots, wantProbeSlots := 1, 0
				if test.prefetch != nil {
					wantPageSlots, wantProbeSlots = 0, 1
				}
				if pageSlots != wantPageSlots || probeSlots != wantProbeSlots {
					t.Fatalf("lane holds: pages = %d, probe = %d, want %d and %d", pageSlots, probeSlots, wantPageSlots, wantProbeSlots)
				}
				if queries := index.queries.Load() - queriesBefore; test.prefetch != nil && queries != 1 {
					t.Fatalf("queries = %d, want the probe alone", queries)
				}
			})
		}
	}
}
