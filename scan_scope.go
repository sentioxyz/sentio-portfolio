package portfolio

import (
	"context"
	"sync"
)

// scanScope is what a scan attaches to its context so that the RPC client and the indexer
// clients, which adapters call with signatures the kernel does not want to widen, can still reach
// the scan's observer, its indexer lane, the blocks it pinned, its per-scan memo and the running
// deployment's coverage sink — and report under the protocol and chain they serve.
type scanScope struct {
	observer Observer
	// lane bounds concurrent indexer requests for the engine that started the scan.
	lane *indexerLane
	// pinned holds the block every chain in this scan was pinned to. It is set once chain setup
	// finishes and read by the indexer presence probe, which needs every chain's block at once.
	pinned map[ChainID]BlockRef
	// accountChains lists the chains the scan reads each account on, keyed by the lowercase hex
	// the presence probe keys on: an account attributed through Instadapp is read on Ethereum
	// alone. Nil reads every account on every pinned chain.
	accountChains map[string][]ChainID
	// memo caches per-scan results shared by the (protocol, chain) jobs of one scan.
	memo *scanMemo
	// prefetchSlots bounds how many presence probes the scan runs ahead of its workers at once,
	// so the probes never take every lane slot from the adapters the workers are already running.
	prefetchSlots chan struct{}
	protocolID    string
	chainID       ChainID
	// presence marks work done for a presence probe, so its lane waits and requests report
	// apart from the per-chain pages.
	presence bool
	// coverage collects the coverage notes of the deployment running under the scope.
	coverage *coverageSink
}

// prefetchConcurrency is how many presence probes run ahead of the workers at once. Two leaves
// half of the default lane to the adapters the workers reach first.
const prefetchConcurrency = 2

type scanScopeKey struct{}

// withScan opens a scope for one scan. A nil observer still opens the scope, because the lane and
// the memo are part of how a scan runs, not of how it is observed.
func withScan(ctx context.Context, observer Observer, lane *indexerLane) context.Context {
	if observer == nil {
		observer = noopObserver{}
	}
	return context.WithValue(ctx, scanScopeKey{}, scanScope{
		observer:      observer,
		lane:          lane,
		memo:          newScanMemo(),
		prefetchSlots: make(chan struct{}, prefetchConcurrency),
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

// withPinnedBlocks records the blocks chain setup pinned and the chains it reads each account on.
// Chains that failed to pin are absent from both.
func withPinnedBlocks(
	ctx context.Context,
	pinned map[ChainID]BlockRef,
	accountChains map[string][]ChainID,
) context.Context {
	scope, ok := ctx.Value(scanScopeKey{}).(scanScope)
	if !ok {
		return ctx
	}
	scope.pinned = pinned
	scope.accountChains = accountChains
	return context.WithValue(ctx, scanScopeKey{}, scope)
}

// pinnedFor returns the pinned blocks of the chains the scan reads account on.
func (s scanScope) pinnedFor(account string) map[ChainID]BlockRef {
	if s.accountChains == nil {
		return s.pinned
	}
	pinned := make(map[ChainID]BlockRef, len(s.accountChains[account]))
	for _, chainID := range s.accountChains[account] {
		if block, exists := s.pinned[chainID]; exists {
			pinned[chainID] = block
		}
	}
	return pinned
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

// coverageSink collects the coverage notes a deployment's index reads record while it runs: an
// index read that answers short of its pin says so here rather than through the adapter, so the
// note reaches the scan however the adapter's read ends.
type coverageSink struct {
	mu    sync.Mutex
	notes []error
}

// withCoverage opens a sink for the notes recorded under the returned context. The engine opens one
// per deployment it reads.
func withCoverage(ctx context.Context) (context.Context, *coverageSink) {
	scope, _ := ctx.Value(scanScopeKey{}).(scanScope)
	scope.coverage = &coverageSink{}
	return context.WithValue(ctx, scanScopeKey{}, scope), scope.coverage
}

// noteCoverage records note in the sink ctx carries. Outside one, nothing reports it.
func noteCoverage(ctx context.Context, note error) {
	sink := scopeFrom(ctx).coverage
	if sink == nil {
		return
	}
	sink.mu.Lock()
	sink.notes = append(sink.notes, note)
	sink.mu.Unlock()
}

// take returns the notes recorded so far and empties the sink.
func (s *coverageSink) take() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := s.notes
	s.notes = nil
	return notes
}

// withPresence marks what runs under ctx as work for a presence probe.
func withPresence(ctx context.Context) context.Context {
	scope, ok := ctx.Value(scanScopeKey{}).(scanScope)
	if !ok {
		return ctx
	}
	scope.presence = true
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

// claim registers key and reports whether the caller now owns its computation. A caller that
// does not own it waits on the entry's done channel for whoever does.
func (m *scanMemo) claim(key string) (*memoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, exists := m.entries[key]
	if exists {
		return entry, false
	}
	entry = &memoEntry{done: make(chan struct{})}
	m.entries[key] = entry
	return entry, true
}

// once returns the memoized value for key, computing it with compute on the first call and
// waiting for whoever is computing it otherwise. The wait ends with ctx, returning nil. A nil
// memo computes every time, which is what a caller outside a scan gets.
func (m *scanMemo) once(ctx context.Context, key string, compute func() any) any {
	if m == nil {
		return compute()
	}
	entry, owner := m.claim(key)
	if !owner {
		select {
		case <-entry.done:
			return entry.value
		case <-ctx.Done():
			return nil
		}
	}
	defer close(entry.done)
	entry.value = compute()
	return entry.value
}

// start computes key in the background unless someone already claimed it, so a later once for
// the same key waits only for whatever is left of the work instead of doing it. A nil memo
// starts nothing.
func (m *scanMemo) start(key string, compute func() any) {
	if m == nil {
		return
	}
	entry, owner := m.claim(key)
	if !owner {
		return
	}
	go func() {
		defer close(entry.done)
		entry.value = compute()
	}()
}
