package portfolio

import "time"

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
	// ObserveIndexer fires once per Sentio indexer request, including every retry, once per wait
	// for the shared indexer lane, and once per release of a lane slot.
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
	// Incomplete counts what the scan stopped before reading — chains it never set up,
	// deployments it never started or stopped midway — each also an "incomplete" error. It is
	// nonzero only when the collection deadline or the context ended the scan early.
	Incomplete int
	// Canceled reports that the context ended before the scan finished; a canceled scan's
	// durations describe how far it got, not how long the work takes. A scan stopped by its own
	// collection deadline is not canceled.
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

// IndexerObservation is one Sentio indexer request, one wait for the indexer lane, or one hold of
// a lane slot.
type IndexerObservation struct {
	ProtocolID string
	// ChainID is the chain the request served, or zero for a status request, which covers every
	// chain of a processor at once, and for a presence probe started ahead of the workers.
	ChainID ChainID
	// Kind is IndexerStatus for a processor status read, IndexerGraphQL for a position query,
	// IndexerSQL for a SQL statement from submission to result, IndexerLane for the time spent
	// waiting to enter the shared indexer lane, or IndexerLaneHold for the time from entering
	// the lane to leaving it.
	Kind string
	// Presence marks requests, lane waits and lane holds made for a presence probe, which asks
	// every chain of an indexer at once, apart from the per-chain pages.
	Presence bool
	// Attempt is 1-based for requests and zero for a lane wait or hold.
	Attempt  int
	Duration time.Duration
	// Err is the request's failure, redacted, or nil. A lane wait reports the context's error
	// when the caller gave up before a slot freed up.
	Err error
}

// Kinds reported in IndexerObservation.Kind.
const (
	IndexerStatus   = "status"
	IndexerGraphQL  = "graphql"
	IndexerSQL      = "sql"
	IndexerLane     = "lane"
	IndexerLaneHold = "lane_hold"
)

type noopObserver struct{}

func (noopObserver) ObserveScan(ScanObservation)         {}
func (noopObserver) ObserveProtocol(ProtocolObservation) {}
func (noopObserver) ObserveRPC(RPCObservation)           {}
func (noopObserver) ObserveIndexer(IndexerObservation)   {}
