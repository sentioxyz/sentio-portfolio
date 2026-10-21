package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
// PositionRefs has released its own. With a single slot held elsewhere, the lookup waits.
func TestPendleMarketLookupWaitsForTheLane(t *testing.T) {
	pt := common.HexToAddress("0x1111111111111111111111111111111111111111")
	market := common.HexToAddress("0x2222222222222222222222222222222222222222")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
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
	indexer := &pendleIndexer{
		api:    &sentioAPIClient{apiKey: "test", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)},
		config: SentioIndexerConfig{GraphQLURL: server.URL, StatusURL: server.URL, ProcessorVersion: "2"},
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
		markets, err := indexer.MarketsForPT(
			withScanLane(context.Background(), lane), BlockRef{ChainID: Ethereum, Number: 100}, []common.Address{pt},
		)
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
