package portfolio

import (
	"context"
	"time"
)

// defaultIndexerConcurrency is how many Sentio indexer requests one engine keeps in flight. The
// index-backed adapters share one API key, so the bound is per engine rather than per protocol:
// it protects the key's server-side queue while letting the scan's workers query in parallel
// instead of behind one another. Four covers the three scan workers with one to spare for a
// concurrent scan.
const defaultIndexerConcurrency = 4

// indexerLane is the admission control in front of every Sentio indexer request: status reads,
// position pages and retries alike. It replaced a single mutex, whose serialization was the
// largest item in a full scan's wall clock.
type indexerLane struct {
	slots chan struct{}
}

func newIndexerLane(concurrency int) *indexerLane {
	if concurrency <= 0 {
		concurrency = defaultIndexerConcurrency
	}
	return &indexerLane{slots: make(chan struct{}, concurrency)}
}

func (l *indexerLane) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *indexerLane) release() {
	<-l.slots
}

// defaultIndexerLane serves callers outside a scan, such as tests that drive an indexer directly.
var defaultIndexerLane = newIndexerLane(defaultIndexerConcurrency)

func laneFrom(ctx context.Context) *indexerLane {
	if lane := scopeFrom(ctx).lane; lane != nil {
		return lane
	}
	return defaultIndexerLane
}

// lockSentioLane takes a slot in the indexer lane and reports how long the wait was. The lane is
// the one place a scan serializes across protocols, chains and concurrent requests, which makes
// the wait the first number to look at when index-backed protocols dominate a scan's duration.
// It fails only when ctx ends before a slot frees up; a successful acquisition must be paired
// with unlockSentioLane on the same context.
func lockSentioLane(ctx context.Context) error {
	startedAt := time.Now()
	if err := laneFrom(ctx).acquire(ctx); err != nil {
		return err
	}
	scope := scopeFrom(ctx)
	scope.observer.ObserveIndexer(IndexerObservation{
		ProtocolID: scope.protocolID,
		ChainID:    scope.chainID,
		Kind:       IndexerLane,
		Duration:   time.Since(startedAt),
	})
	return nil
}

func unlockSentioLane(ctx context.Context) {
	laneFrom(ctx).release()
}
