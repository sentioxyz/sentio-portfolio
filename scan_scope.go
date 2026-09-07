package portfolio

import (
	"context"
	"sync"
)

// scanScope is what a scan attaches to its context so that the RPC client and the indexer
// clients, which adapters call with signatures the kernel does not want to widen, can still reach
// the scan's observer, its indexer lane, the blocks it pinned and its per-scan memo — and report
// under the protocol and chain they serve.
type scanScope struct {
	observer Observer
	// lane bounds concurrent indexer requests for the engine that started the scan.
	lane *indexerLane
	// pinned holds the block every chain in this scan was pinned to. It is set once chain setup
	// finishes and read by the indexer presence probe, which needs every chain's block at once.
	pinned map[ChainID]BlockRef
	// memo caches per-scan results shared by the (protocol, chain) jobs of one scan.
	memo       *scanMemo
	protocolID string
	chainID    ChainID
}

type scanScopeKey struct{}

// withScan opens a scope for one scan. A nil observer still opens the scope, because the lane and
// the memo are part of how a scan runs, not of how it is observed.
func withScan(ctx context.Context, observer Observer, lane *indexerLane) context.Context {
	if observer == nil {
		observer = noopObserver{}
	}
	return context.WithValue(ctx, scanScopeKey{}, scanScope{
		observer: observer,
		lane:     lane,
		memo:     newScanMemo(),
	})
}

// withObserver opens a scope carrying only an observer. It keeps the observer tests independent
// of the lane and the memo.
func withObserver(ctx context.Context, observer Observer) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, scanScopeKey{}, scanScope{observer: observer})
}

// withPinnedBlocks records the blocks chain setup pinned. Chains that failed to pin are absent.
func withPinnedBlocks(ctx context.Context, pinned map[ChainID]BlockRef) context.Context {
	scope, ok := ctx.Value(scanScopeKey{}).(scanScope)
	if !ok {
		return ctx
	}
	scope.pinned = pinned
	return context.WithValue(ctx, scanScopeKey{}, scope)
}

// withDeployment narrows the scope to one adapter on one chain for the duration of its run.
func withDeployment(ctx context.Context, protocolID string, chainID ChainID) context.Context {
	scope, ok := ctx.Value(scanScopeKey{}).(scanScope)
	if !ok {
		return ctx
	}
	scope.protocolID = protocolID
	scope.chainID = chainID
	return context.WithValue(ctx, scanScopeKey{}, scope)
}

func scopeFrom(ctx context.Context) scanScope {
	if ctx == nil {
		return scanScope{observer: noopObserver{}}
	}
	scope, ok := ctx.Value(scanScopeKey{}).(scanScope)
	if !ok {
		return scanScope{observer: noopObserver{}}
	}
	if scope.observer == nil {
		scope.observer = noopObserver{}
	}
	return scope
}

// observerFrom never returns nil, so call sites report unconditionally.
func observerFrom(ctx context.Context) Observer {
	return scopeFrom(ctx).observer
}

// scanMemo shares the result of work several (protocol, chain) jobs of one scan would otherwise
// each redo. Entries are computed once, by the first job to ask, while the others wait for it.
type scanMemo struct {
	mu      sync.Mutex
	entries map[string]*memoEntry
}

type memoEntry struct {
	done  chan struct{}
	value any
}

func newScanMemo() *scanMemo {
	return &scanMemo{entries: make(map[string]*memoEntry)}
}

// once returns the memoized value for key, computing it with compute on the first call. A nil
// memo computes every time, which is what a caller outside a scan gets.
func (m *scanMemo) once(key string, compute func() any) any {
	if m == nil {
		return compute()
	}
	m.mu.Lock()
	entry, exists := m.entries[key]
	if !exists {
		entry = &memoEntry{done: make(chan struct{})}
		m.entries[key] = entry
	}
	m.mu.Unlock()
	if exists {
		<-entry.done
		return entry.value
	}
	defer close(entry.done)
	entry.value = compute()
	return entry.value
}
