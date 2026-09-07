package portfolio

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
			results[index] = memo.once("key", func() any {
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
	if memo.once("other", func() any { return "fresh" }) != "fresh" {
		t.Fatalf("a different key reused a cached value")
	}
	var nilMemo *scanMemo
	if nilMemo.once("key", func() any { return "computed" }) != "computed" {
		t.Fatalf("a nil memo must compute every time")
	}
}
