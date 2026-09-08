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

func TestIndexerLaneAdmitsUpToItsConcurrency(t *testing.T) {
	lane := newIndexerLane(2)
	ctx := withScanLane(context.Background(), lane)
	for slot := 0; slot < 2; slot++ {
		if err := lockSentioLane(ctx); err != nil {
			t.Fatalf("slot %d: %v", slot, err)
		}
	}
	third := make(chan error, 1)
	go func() { third <- lockSentioLane(ctx) }()
	select {
	case err := <-third:
		t.Fatalf("third acquisition did not wait: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	unlockSentioLane(ctx)
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("third acquisition after a release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("third acquisition did not proceed after a release")
	}
	unlockSentioLane(ctx)
	unlockSentioLane(ctx)
}

func TestIndexerLaneHonoursCancellationWhileWaiting(t *testing.T) {
	lane := newIndexerLane(1)
	holder := withScanLane(context.Background(), lane)
	if err := lockSentioLane(holder); err != nil {
		t.Fatal(err)
	}
	defer unlockSentioLane(holder)
	ctx, cancel := context.WithTimeout(withScanLane(context.Background(), lane), 20*time.Millisecond)
	defer cancel()
	err := lockSentioLane(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting acquisition error = %v, want deadline exceeded", err)
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
	if err := lockSentioLane(holder); err != nil {
		t.Fatal(err)
	}
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
	unlockSentioLane(holder)
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
	if err := lockSentioLane(holder); err != nil {
		t.Fatal(err)
	}
	unlockSentioLane(holder)
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
