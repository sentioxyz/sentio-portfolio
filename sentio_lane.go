package portfolio

import (
	"context"
	"sync"
	"sync/atomic"
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
	// waiting counts callers blocked in acquire, for IndexerLaneStats.
	waiting atomic.Int64
}

func newIndexerLane(concurrency int) *indexerLane {
	if concurrency <= 0 {
		concurrency = defaultIndexerConcurrency
	}
	return &indexerLane{slots: make(chan struct{}, concurrency)}
}

func (l *indexerLane) acquire(ctx context.Context) error {
	// A free slot is taken without counting as a wait; a context that has already ended goes
	// straight to the blocking select, which settles it as before.
	if ctx.Err() == nil {
		select {
		case l.slots <- struct{}{}:
			return nil
		default:
		}
	}
	l.waiting.Add(1)
	defer l.waiting.Add(-1)
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

// IndexerLaneStats is a snapshot of an engine's indexer lane, for a host to sample as gauges.
type IndexerLaneStats struct {
	// Capacity is how many indexer requests the lane admits at once.
	Capacity int
	// InUse is how many of those slots are held.
	InUse int
	// Waiting is how many callers are blocked waiting for a slot.
	Waiting int
}

func (l *indexerLane) stats() IndexerLaneStats {
	return IndexerLaneStats{
		Capacity: cap(l.slots),
		InUse:    len(l.slots),
		Waiting:  int(l.waiting.Load()),
	}
}

// IndexerLaneStats reports the lane every scan of this engine shares. It is safe to call at any
// time, including while scans run.
func (e *Engine) IndexerLaneStats() IndexerLaneStats {
	return e.indexerLane.stats()
}

// defaultIndexerLane serves callers outside a scan, such as tests that drive an indexer directly.
var defaultIndexerLane = newIndexerLane(defaultIndexerConcurrency)

func laneFrom(ctx context.Context) *indexerLane {
	if lane := scopeFrom(ctx).lane; lane != nil {
		return lane
	}
	return defaultIndexerLane
}

// lockSentioLane takes a slot in the indexer lane and returns the function that gives it back.
// The lane is the one place a scan serializes across protocols, chains and concurrent requests,
// so both the wait and the hold are reported: the wait is the first number to look at when
// index-backed protocols dominate a scan's duration, and a wait that ends with ctx — reported
// with its error — is a caller that gave up without a slot. It fails only when ctx ends before a
// slot frees up. Calling the release function more than once releases once, so a caller that
// hands the slot back early can still defer it.
func lockSentioLane(ctx context.Context) (func(), error) {
	lane := laneFrom(ctx)
	scope := scopeFrom(ctx)
	startedAt := time.Now()
	err := lane.acquire(ctx)
	scope.observer.ObserveIndexer(IndexerObservation{
		ProtocolID: scope.protocolID,
		ChainID:    scope.chainID,
		Kind:       IndexerLane,
		Presence:   scope.presence,
		Duration:   time.Since(startedAt),
		Err:        err,
	})
	if err != nil {
		return nil, err
	}
	heldFrom := time.Now()
	var once sync.Once
	return func() {
		once.Do(func() {
			lane.release()
			scope.observer.ObserveIndexer(IndexerObservation{
				ProtocolID: scope.protocolID,
				ChainID:    scope.chainID,
				Kind:       IndexerLaneHold,
				Presence:   scope.presence,
				Duration:   time.Since(heldFrom),
			})
		})
	}, nil
}
