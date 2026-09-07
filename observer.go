package portfolio

import (
	"context"
	"sync"
	"time"
)

// Observer receives timing and outcome facts about a scan while it runs. The kernel exports no
// telemetry of its own — it has no opinion about Prometheus, OpenTelemetry or structured logs —
// so a host installs an Observer through EngineConfig and maps these calls onto whatever it
// runs. A host that installs nothing pays for nothing: every hook resolves to a no-op.
//
// Methods are called concurrently from the scan's worker goroutines and sit on its critical
// path, so implementations must be safe for concurrent use and must return quickly; anything
// slow belongs in a buffer the observer drains elsewhere.
//
// Nothing an Observer receives names an endpoint or credential. Errors are the redacted values a
// Response would show, and no observation carries the scanned address, so a host can turn any
// field into a metric label without leaking either.
type Observer interface {
	// ObserveScan fires once at the end of every Scan or ScanWithOptions.
	ObserveScan(ScanObservation)
	// ObserveProtocol fires once per (protocol, chain) deployment the scan ran.
	ObserveProtocol(ProtocolObservation)
	// ObserveRPC fires once per JSON-RPC round trip, including every retry.
	ObserveRPC(RPCObservation)
	// ObserveIndexer fires once per Sentio indexer request, including every retry, and once per
	// wait for the shared indexer lane.
	ObserveIndexer(IndexerObservation)
}

// ScanObservation summarizes one scan. The phase durations are wall-clock and sequential, so they
// add up to roughly Duration; the counts describe what the scan actually did rather than what it
// was asked for.
type ScanObservation struct {
	Options  ScanOptions
	Duration time.Duration

	// ChainSetup dials every selected chain, pins its block and resolves attributed accounts.
	ChainSetup time.Duration
	// WalletDiscovery is the WalletBalanceProvider call. Zero when the wallet protocol was
	// excluded.
	WalletDiscovery time.Duration
	// Protocols is the adapter fan-out: every (protocol, chain) deployment on the worker pool.
	Protocols time.Duration
	// Pricing is the PriceProvider round trip plus valuation. Zero when prices were skipped.
	Pricing time.Duration

	// Chains is how many chains were pinned successfully and therefore scanned.
	Chains int
	// Deployments is how many (protocol, chain) pairs were dispatched to the worker pool.
	Deployments int
	Snapshots   int
	// Errors is the scan's error list as the Response reports it. It is shared with the
	// Response, so observers must treat it as read-only.
	Errors []ScanError
	// Canceled reports that the context ended before the scan finished; a canceled scan's
	// durations describe how far it got, not how long the work takes.
	Canceled bool
}

// ProtocolObservation is one adapter run on one chain: the work between the worker picking the
// deployment up and handing its groups back.
type ProtocolObservation struct {
	ProtocolID string
	ChainID    ChainID
	Duration   time.Duration
	// Groups counts the position groups the adapter returned, across every attributed account.
	Groups int
	// Err is the adapter's failure, redacted, or nil.
	Err error
}

// RPCObservation is one JSON-RPC HTTP round trip. Retries report separately with an increasing
// Attempt, so a host can tell a slow endpoint from a flaky one.
type RPCObservation struct {
	ChainID ChainID
	// ProtocolID names the adapter the call served, or is empty for chain setup and wallet reads
	// that run outside any adapter.
	ProtocolID string
	// Method is the JSON-RPC method. A batch reports the method its elements share, or "batch"
	// when they differ.
	Method string
	// BatchSize is the number of elements in a batch call, or zero for a single call.
	BatchSize int
	// Attempt is 1-based.
	Attempt  int
	Duration time.Duration
	Err      error
}

// IndexerObservation is one Sentio indexer request, or one wait for the indexer lane.
type IndexerObservation struct {
	ProtocolID string
	// ChainID is the chain the request served, or zero for a status request, which covers every
	// chain of a processor at once.
	ChainID ChainID
	// Kind is IndexerStatus for a processor status read, IndexerGraphQL for a position query, or
	// IndexerLane for the time spent waiting to enter the shared indexer lane.
	Kind string
	// Attempt is 1-based for requests and zero for a lane wait.
	Attempt  int
	Duration time.Duration
	Err      error
}

// Kinds reported in IndexerObservation.Kind.
const (
	IndexerStatus  = "status"
	IndexerGraphQL = "graphql"
	IndexerLane    = "lane"
)

type noopObserver struct{}

func (noopObserver) ObserveScan(ScanObservation)         {}
func (noopObserver) ObserveProtocol(ProtocolObservation) {}
func (noopObserver) ObserveRPC(RPCObservation)           {}
func (noopObserver) ObserveIndexer(IndexerObservation)   {}

// scanScope is what a scan attaches to its context so that the RPC client and the indexer
// clients, which adapters call with signatures the kernel does not want to widen, can still
// report to the scan's observer under the right protocol and chain.
type scanScope struct {
	observer   Observer
	protocolID string
	chainID    ChainID
}

type scanScopeKey struct{}

func withObserver(ctx context.Context, observer Observer) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, scanScopeKey{}, scanScope{observer: observer})
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
	if !ok || scope.observer == nil {
		return scanScope{observer: noopObserver{}}
	}
	return scope
}

// observerFrom never returns nil, so call sites report unconditionally.
func observerFrom(ctx context.Context) Observer {
	return scopeFrom(ctx).observer
}

// Sentio applies a queue limit per API key. All index-backed portfolio adapters therefore share
// one lane across status checks, pagination, and retries instead of limiting concurrency inside
// each protocol independently.
var sentioQueryMu sync.Mutex

// lockSentioLane takes the shared indexer lane and reports how long the wait was. The lane is
// the one place a scan serializes across protocols, chains and concurrent requests, which makes
// the wait the first number to look at when index-backed protocols dominate a scan's duration.
func lockSentioLane(ctx context.Context) {
	startedAt := time.Now()
	sentioQueryMu.Lock()
	scope := scopeFrom(ctx)
	scope.observer.ObserveIndexer(IndexerObservation{
		ProtocolID: scope.protocolID,
		ChainID:    scope.chainID,
		Kind:       IndexerLane,
		Duration:   time.Since(startedAt),
	})
}

func unlockSentioLane() {
	sentioQueryMu.Unlock()
}
