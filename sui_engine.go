package portfolio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// SuiConfig supplies what a SuiEngine reads beyond the Sentio indexes of the Engine it is built
// from. Every field is optional: a missing source makes the reads that need it report that nothing
// was read, never that nothing is held.
type SuiConfig struct {
	// Node dials a Sui node. It is called on the first read that needs one, once for every read
	// waiting on it, on a timeout of its own rather than the context of whichever read happened
	// to start it; a failed dial is tried again by the next read. Nil serves the indexed protocols
	// alone. SuiGRPCNode is the production dialer.
	Node func(context.Context) (SuiReader, error)
	// Prices values coins. Nil leaves every component unpriced.
	Prices SuiPrices
	// WalletHistory reads what an account held at past checkpoints. Nil leaves the wallet unread
	// at every pin.
	WalletHistory SuiWalletHistory
	// Now is the clock the freshness of an indexed latest read is judged by. Nil is time.Now.
	Now func() time.Time
}

// SuiGRPCNode dials endpoint (grpc://host:port for plaintext, grpcs://host:port for TLS) and
// verifies that it serves Sui mainnet.
func SuiGRPCNode(endpoint string) func(context.Context) (SuiReader, error) {
	return func(ctx context.Context) (SuiReader, error) {
		client, err := dialSuiGRPC(ctx, endpoint, SuiMainnetChainIdentifier)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
}

// SuiEngine reads Sui accounts — direct wallet holdings and every protocol in suiRoutes — at the
// head or at a past checkpoint or instant, and values them. It is built from an Engine and shares
// its indexer lane, its observer and its Sentio index configuration, so a Sui read waits for the
// same lane slots as an EVM scan and reports to the same observer. It is safe for concurrent use.
type SuiEngine struct {
	engine  *Engine
	node    *suiNodeDialer
	nodes   suiNodeSource
	indexes map[string]suiIndexSource
	prices  SuiPrices
	wallet  SuiWalletHistory
	now     func() time.Time
	// metadata remembers usable coin metadata between wallet history reads: a coin's precision
	// never changes, and a backfill asks for the same coins run after run.
	metadata suiMetadataCache
}

// suiIndexSource is one protocol's version-pinned processor index. It answers for completed
// samples only, which is why it can serve a past checkpoint at all, and every answer carries the
// sample it came from.
type suiIndexSource interface {
	ReadLatest(context.Context, SuiAddress) (suiPositions, error)
	ReadAtCheckpoint(context.Context, SuiAddress, uint64) (suiPositions, error)
	ReadRange(context.Context, SuiAddress, time.Time, time.Time) ([]suiHistorySample, error)
}

// suiNodeSource reads a protocol's head object state through a node.
type suiNodeSource interface {
	ReadLatest(context.Context, string, SuiAddress, SuiReader) (suiPositions, error)
}

// suiIndexes are the protocols read from a processor index, by the constructor of their reader.
// Their configurations are the Engine's SentioIndexers under the same protocol ID.
var suiIndexes = map[string]func(SentioIndexerConfig) (*suiHistoryIndex, error){
	"suilend": newSuilendIndex,
	"navi":    newNaviIndex,
}

// NewSuiEngine builds the Sui engine on engine's indexer lane and observer, reading each indexed
// protocol from the index engine was configured with under that protocol's ID. An index that is
// configured but unusable — no explicit processor version, an unusable endpoint — is an error; one
// that is not configured leaves its protocol unread. Nothing is dialled here.
func NewSuiEngine(engine *Engine, config SuiConfig) (*SuiEngine, error) {
	if engine == nil {
		return nil, errors.New("a Sui engine is built from an engine")
	}
	e := &SuiEngine{
		engine:  engine,
		nodes:   newSuiNodeProtocols(),
		indexes: map[string]suiIndexSource{},
		prices:  config.Prices,
		wallet:  config.WalletHistory,
		now:     config.Now,
	}
	if config.Node != nil {
		e.node = &suiNodeDialer{dial: config.Node}
	}
	if e.now == nil {
		e.now = time.Now
	}
	for _, route := range suiRoutes {
		build := suiIndexes[route.id]
		indexConfig, configured := engine.sentioIndexers[route.id]
		if build == nil || !configured {
			continue
		}
		index, err := build(indexConfig)
		if err != nil {
			return nil, fmt.Errorf("%s index: %w", route.id, err)
		}
		e.indexes[route.id] = index.withEngine(engine)
	}
	return e, nil
}

// suiSource is where a route reads a protocol's state.
type suiSource uint8

const (
	// suiUnread is a state nothing can answer for: a latest-only protocol at a pin.
	suiUnread suiSource = iota
	// suiFromNode is head state through the node.
	suiFromNode
	// suiFromIndex is a completed sample of the protocol's processor index.
	suiFromIndex
	// suiFromWalletHistory is SuiConfig.WalletHistory at the checkpoint of the protocol samples
	// read beside it.
	suiFromWalletHistory
)

// suiRoute is where one protocol's latest and pinned state are read. A read uses its route's
// source alone: a source that fails is never replaced by another, because each answers for a
// different checkpoint, and a head answer under a past pin would be a wrong number nobody can see
// is wrong.
type suiRoute struct {
	id, name       string
	latest, pinned suiSource
}

// suiRoutes is every Sui protocol, in the order every response lists them. Suilend has no node
// path, so even its latest state is the newest completed sample. NAVI reads its head from the
// node, which is fresher than its newest sample, and its past from its index. The wallet's past
// comes from wallet history, read at the checkpoint the protocol samples beside it answer for.
var suiRoutes = []suiRoute{
	{id: "wallet", name: "Wallet", latest: suiFromNode, pinned: suiFromWalletHistory},
	{id: "navi", name: "NAVI", latest: suiFromNode, pinned: suiFromIndex},
	{id: "volo-vaults", name: "Volo Vaults", latest: suiFromNode, pinned: suiUnread},
	{id: "suilend", name: "Suilend", latest: suiFromIndex, pinned: suiFromIndex},
	{id: "cetus", name: "Cetus", latest: suiFromNode, pinned: suiUnread},
	{id: "bluefin", name: "Bluefin", latest: suiFromNode, pinned: suiUnread},
}

// SuiProtocolInfo describes one Sui protocol and whether this engine, as configured, can read it.
type SuiProtocolInfo struct {
	ID   string
	Name string
	// History reports that the protocol has a past at all: a pinned read can answer for it once
	// its sources are configured.
	History bool
	// Latest and Pinned report that every source a latest or a pinned read needs is configured.
	Latest bool
	Pinned bool
}

// Protocols lists every Sui protocol in response order.
func (e *SuiEngine) Protocols() []SuiProtocolInfo {
	out := make([]SuiProtocolInfo, 0, len(suiRoutes))
	for _, route := range suiRoutes {
		out = append(out, e.info(route))
	}
	return out
}

func (e *SuiEngine) info(route suiRoute) SuiProtocolInfo {
	return SuiProtocolInfo{
		ID:      route.id,
		Name:    route.name,
		History: route.pinned != suiUnread,
		Latest:  e.configured(route, route.latest) == nil,
		Pinned:  e.configured(route, route.pinned) == nil,
	}
}

// HistoryReady reports whether every protocol with a past can be read at a pin, which a history
// sample must cover: one taken without a protocol would answer for it with nothing.
func (e *SuiEngine) HistoryReady() error {
	var missing []string
	for _, route := range suiRoutes {
		if route.pinned == suiUnread {
			continue
		}
		if err := e.configured(route, route.pinned); err != nil {
			missing = append(missing, route.id+" ("+err.Error()+")")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("Sui history cannot read %s", strings.Join(missing, ", "))
	}
	return nil
}

var (
	errSuiNodeUnconfigured    = errors.New("Sui node reads are not configured; no positions were read")
	errSuiIndexUnconfigured   = errors.New("indexed Sui positions are not configured for this protocol; no positions were read")
	errSuiWalletUnconfigured  = errors.New("Sui wallet history is not configured; no balances were read")
	errSuiWalletNeedsNode     = errors.New("Sui wallet history reads coin metadata from a node, and none is configured; no balances were read")
	errSuiHistoryUnsupported  = errors.New("Sui history is unavailable for this protocol; only latest state is supported and no positions were read")
	errSuiIndexedLatestStale  = errors.New("Sui position index has not completed a recent sample")
	errSuiInvalidIndexedState = errors.New("indexed Sui positions returned an invalid sample")
)

// configured reports why route cannot be read from source, or nil when it can.
func (e *SuiEngine) configured(route suiRoute, source suiSource) error {
	switch source {
	case suiFromNode:
		if e.node == nil {
			return errSuiNodeUnconfigured
		}
	case suiFromIndex:
		if e.indexes[route.id] == nil {
			return errSuiIndexUnconfigured
		}
	case suiFromWalletHistory:
		if e.wallet == nil {
			return errSuiWalletUnconfigured
		}
		if e.node == nil {
			return errSuiWalletNeedsNode
		}
	default:
		return errSuiHistoryUnsupported
	}
	return nil
}

// SuiPin is the state a scan reads. The zero value is the latest.
type SuiPin struct {
	kind       suiPinKind
	checkpoint uint64
	at         time.Time
}

type suiPinKind uint8

const (
	suiPinLatest suiPinKind = iota
	suiPinCheckpoint
	suiPinTime
)

// SuiAtCheckpoint reads the newest completed samples at or before checkpoint sequence.
func SuiAtCheckpoint(sequence uint64) SuiPin {
	return SuiPin{kind: suiPinCheckpoint, checkpoint: sequence}
}

// SuiAtTime reads the samples covering at.
func SuiAtTime(at time.Time) SuiPin {
	return SuiPin{kind: suiPinTime, at: at}
}

// IsLatest reports whether the pin reads the latest state.
func (p SuiPin) IsLatest() bool { return p.kind == suiPinLatest }

// SuiScanOptions selects what a Sui scan reads.
type SuiScanOptions struct {
	// ProtocolIDs selects protocols by ID; empty selects every protocol. An ID no protocol has is
	// ignored, as Engine.ScanWithOptions ignores one.
	ProtocolIDs map[string]struct{}
	// Pin is the state to read; the zero value reads the latest.
	Pin SuiPin
	// SkipPrices values nothing and asks for no quote.
	SkipPrices bool
}

func (o SuiScanOptions) selected() []suiRoute {
	routes := make([]suiRoute, 0, len(suiRoutes))
	for _, route := range suiRoutes {
		if _, wanted := o.ProtocolIDs[route.id]; wanted || len(o.ProtocolIDs) == 0 {
			routes = append(routes, route)
		}
	}
	return routes
}

// SuiResponse is a Sui scan's answer, shaped as Response is for EVM accounts. Every selected
// protocol either is read or has a "protocol" error saying why it was not. A latest node read of a
// protocol that holds nothing has no snapshot; an indexed or pinned read always has one, so an
// absent snapshot is never what "nothing held at a pin" looks like.
type SuiResponse struct {
	Address            SuiAddress
	SupportedProtocols []SuiProtocolInfo
	Snapshots          []SuiSnapshot
	ProtocolSummaries  []ProtocolSummary
	// Errors carry ChainID zero and scope "protocol" (not read, or read in part), "pricing" or
	// "coverage".
	Errors []ScanError
	// Checkpoint is the newest checkpoint any read answered for: a head seen after a latest read,
	// or an index sample. It is zero when nothing was read.
	Checkpoint SuiCheckpoint
	// Prices holds one quote per coin a component was valued at, keyed "sui:<coin type>": the
	// first protocol's in response order when protocols value a coin at different instants, as a
	// latest scan does an indexed sample. A component's own PriceUSD is the price it was valued
	// at.
	Prices      map[string]float64
	CompletedAt time.Time
}

// SuiSnapshot is one protocol's positions at one checkpoint: the head seen after a latest node
// read, whose groups carry headBeforeRead and headAfterRead, or the sample an index or wallet
// history read was answered from.
type SuiSnapshot struct {
	ProtocolID   string
	ProtocolName string
	Account      SuiAddress
	Checkpoint   SuiCheckpoint
	Groups       []SuiGroup
	ValueUSD     *float64
}

// suiPriceKey is a coin's key in SuiResponse.Prices.
func suiPriceKey(coinType string) string { return "sui:" + coinType }

func (e *SuiEngine) newResponse(owner SuiAddress, routes []suiRoute) *SuiResponse {
	response := &SuiResponse{
		Address:            owner,
		SupportedProtocols: make([]SuiProtocolInfo, 0, len(routes)),
		Snapshots:          []SuiSnapshot{},
		ProtocolSummaries:  []ProtocolSummary{},
		Errors:             []ScanError{},
		Prices:             map[string]float64{},
	}
	for _, route := range routes {
		response.SupportedProtocols = append(response.SupportedProtocols, e.info(route))
	}
	return response
}

// suiProtocolResult is one protocol's part of a response: its snapshot, when it was read, and its
// errors in the order they arose.
type suiProtocolResult struct {
	snapshot   *SuiSnapshot
	summary    *ProtocolSummary
	checkpoint SuiCheckpoint
	prices     map[string]float64
	errors     []ScanError
}

func (r *suiProtocolResult) fail(route suiRoute, scope string, err error) {
	r.errors = append(r.errors, ScanError{Scope: scope, ProtocolID: route.id, ProtocolName: route.name, Message: PublicError(err)})
}

// add appends result to the response, raising its checkpoint to the result's.
func (response *SuiResponse) add(result *suiProtocolResult) {
	if result.snapshot != nil {
		response.Snapshots = append(response.Snapshots, *result.snapshot)
	}
	if result.summary != nil {
		response.ProtocolSummaries = append(response.ProtocolSummaries, *result.summary)
	}
	response.Errors = append(response.Errors, result.errors...)
	if result.checkpoint.Sequence > response.Checkpoint.Sequence {
		response.Checkpoint = result.checkpoint
	}
	for coin, price := range result.prices {
		if _, quoted := response.Prices[suiPriceKey(coin)]; !quoted {
			response.Prices[suiPriceKey(coin)] = price
		}
	}
}

// Scan reads every protocol's latest state and values it.
func (e *SuiEngine) Scan(ctx context.Context, owner SuiAddress) *SuiResponse {
	return e.ScanWithOptions(ctx, owner, SuiScanOptions{})
}

// ScanWithOptions reads the selected protocols at options.Pin. A pinned scan never reaches the
// node for state: it reads processor indexes and wallet history alone, the latest-only protocols
// report that they were not read, and it is answered exactly as ScanRange answers one instant —
// every protocol at one checkpoint when the wallet is read beside them.
func (e *SuiEngine) ScanWithOptions(ctx context.Context, owner SuiAddress, options SuiScanOptions) *SuiResponse {
	ctx = withScan(ctx, e.engine.observer, e.engine.indexerLane)
	routes := options.selected()
	switch options.Pin.kind {
	case suiPinCheckpoint:
		checkpoint := options.Pin.checkpoint
		return e.scanSamples(ctx, owner, routes, suiSamplePlan{instants: []time.Time{{}}, checkpoint: &checkpoint, skipPrices: options.SkipPrices})[0].Response
	case suiPinTime:
		return e.scanSamples(ctx, owner, routes, suiSamplePlan{instants: []time.Time{options.Pin.at}, skipPrices: options.SkipPrices})[0].Response
	}
	return e.scanLatest(ctx, owner, routes, options.SkipPrices)
}

// suiIndexedLatestMaxAge bounds how old the sample answering a latest indexed read may be.
// Materializing one completed sample takes the following callback, so a healthy index can expose a
// sample almost two sampling intervals old; the bound exists to keep a stalled index from answering
// "latest" with a stale sample.
const suiIndexedLatestMaxAge = 2*time.Hour + time.Minute

// suiLatestRead is what one protocol's latest read returned, before valuation.
type suiLatestRead struct {
	positions suiPositions
	// valuedAt is when its quotes are asked for: zero for head state, the sample's timestamp for an
	// indexed read, which answers for that sample rather than for the head.
	valuedAt time.Time
	result   suiProtocolResult
	ok       bool
}

// scanLatest reads every route at once, then prices what they hold together.
func (e *SuiEngine) scanLatest(ctx context.Context, owner SuiAddress, routes []suiRoute, skipPrices bool) *SuiResponse {
	response := e.newResponse(owner, routes)
	reads := make([]suiLatestRead, len(routes))
	var group sync.WaitGroup
	for i, route := range routes {
		group.Go(func() {
			reads[i] = e.readLatest(withDeployment(ctx, route.id, 0), owner, route)
		})
	}
	group.Wait()
	wanted := map[time.Time][]string{}
	for _, read := range reads {
		if read.ok {
			wanted[read.valuedAt] = mergeSortedStrings(wanted[read.valuedAt], suiPositionCoins(read.positions))
		}
	}
	quotes := suiQuotes{}
	if !skipPrices {
		quotes = e.quote(ctx, wanted)
	}
	for i, route := range routes {
		read := &reads[i]
		if read.ok {
			e.valueRead(owner, route, read, quotes, skipPrices)
		}
		response.add(&read.result)
	}
	response.CompletedAt = e.now()
	return response
}

// readLatest reads one route's latest state, recording why it was not read in the result.
func (e *SuiEngine) readLatest(ctx context.Context, owner SuiAddress, route suiRoute) suiLatestRead {
	read := suiLatestRead{}
	if err := e.configured(route, route.latest); err != nil {
		read.result.fail(route, "protocol", err)
		return read
	}
	if route.latest == suiFromIndex {
		positions, err := e.indexes[route.id].ReadLatest(ctx, owner)
		if err == nil {
			err = e.checkIndexedLatest(route, positions)
		}
		if err != nil {
			read.result.fail(route, "protocol", err)
			return read
		}
		read.positions, read.valuedAt, read.ok = positions, positions.Checkpoint.Timestamp, true
		return read
	}
	reader, err := e.node.get(ctx)
	if err != nil {
		read.result.fail(route, "protocol", fmt.Errorf("Sui node is unavailable: %w", err))
		return read
	}
	var positions suiPositions
	if route.id == "wallet" {
		positions, err = e.readWalletLatest(ctx, owner, reader)
	} else {
		positions, err = e.nodes.ReadLatest(ctx, route.id, owner, reader)
		if err == nil && positions.ProtocolID != route.id {
			err = errors.New("protocol result has an invalid identity")
		}
	}
	if err != nil {
		read.result.fail(route, "protocol", err)
		return read
	}
	read.positions, read.ok = positions, true
	return read
}

// checkIndexedLatest accepts an indexed latest read only when it names a real sample that is not
// in the future and is recent enough to stand for the latest state.
func (e *SuiEngine) checkIndexedLatest(route suiRoute, positions suiPositions) error {
	if err := checkSuiSample(route, positions, nil, nil); err != nil {
		return err
	}
	now := e.now()
	if sample := positions.Checkpoint.Timestamp; sample.After(now) || now.Sub(sample) > suiIndexedLatestMaxAge {
		return errSuiIndexedLatestStale
	}
	return nil
}

// checkSuiSample accepts an indexed answer only when it is the route's, names a whole checkpoint,
// and does not postdate the checkpoint or instant it was asked about.
func checkSuiSample(route suiRoute, positions suiPositions, checkpoint *uint64, at *time.Time) error {
	sample := positions.Checkpoint
	if positions.ProtocolID != route.id || sample.Sequence == 0 || sample.Timestamp.IsZero() ||
		sample.Digest == ([32]byte{}) ||
		(checkpoint != nil && sample.Sequence > *checkpoint) ||
		(at != nil && sample.Timestamp.After(*at)) {
		return errSuiInvalidIndexedState
	}
	return nil
}

// valueRead prices one latest read and turns it into the route's snapshot. A head read holding
// nothing has no snapshot; an indexed read always has one, at its sample.
func (e *SuiEngine) valueRead(owner SuiAddress, route suiRoute, read *suiLatestRead, quotes suiQuotes, skipPrices bool) {
	positions := read.positions
	read.result.checkpoint = positions.Checkpoint
	for _, err := range positions.Errors {
		read.result.fail(route, "protocol", err)
	}
	coins := suiPositionCoins(positions)
	var prices map[string]float64
	if !skipPrices {
		var failures []error
		prices, failures = quotes.at(read.valuedAt, coins)
		// The wallet holds unsolicited coins nothing quotes, which the host hides after valuation;
		// every other unpriced coin is a gap the response reports.
		if route.id != "wallet" {
			failures = append(failures, quotes.gaps(read.valuedAt, coins, prices)...)
		}
		for _, failure := range failures {
			read.result.fail(route, "pricing", failure)
		}
	}
	snapshot, summary, err := suiValuedSnapshot(owner, positions, prices)
	if err != nil {
		read.result.fail(route, "protocol", err)
		return
	}
	if snapshot == nil && route.latest == suiFromIndex {
		snapshot = &SuiSnapshot{ProtocolID: route.id, ProtocolName: route.name, Account: owner, Checkpoint: positions.Checkpoint}
	}
	read.result.snapshot, read.result.summary, read.result.prices = snapshot, summary, prices
}

// suiPositionCoins is every coin type positions hold, sorted.
func suiPositionCoins(positions suiPositions) []string {
	coins := []string{}
	for _, group := range positions.Groups {
		for _, component := range group.Components {
			coins = append(coins, component.Coin.CoinType)
		}
	}
	return mergeSortedStrings(nil, coins)
}

// suiDialTimeout bounds one dial of the node, apart from any request.
const suiDialTimeout = 20 * time.Second

// suiNodeDialer dials the node on first use and keeps the connection: a node that is down when the
// service starts must not take anything that does not need it down with it, and the transport
// reconnects a live connection by itself.
type suiNodeDialer struct {
	dial func(context.Context) (SuiReader, error)

	mu     sync.Mutex
	reader SuiReader
	// attempt is the dial in flight, if any. Reads that arrive during it wait on its done channel
	// rather than on the mutex, so a read can stop waiting when its own context ends.
	attempt *suiDialAttempt
}

// suiDialAttempt is one shared dial. reader and err are written before done is closed and read
// only after it.
type suiDialAttempt struct {
	done   chan struct{}
	reader SuiReader
	err    error
}

// get returns the shared reader, dialling it on first use. Only one dial runs at a time; every
// read that arrives while it is in flight waits for that same dial, and stops waiting as soon as
// its own context ends. The dial runs on its own timeout, detached from any one read, so the read
// that happened to start it cannot fail the ones queued behind it by going away.
func (d *suiNodeDialer) get(ctx context.Context) (SuiReader, error) {
	d.mu.Lock()
	if d.reader != nil {
		reader := d.reader
		d.mu.Unlock()
		return reader, nil
	}
	attempt := d.attempt
	if attempt == nil {
		attempt = &suiDialAttempt{done: make(chan struct{})}
		d.attempt = attempt
		go d.run(attempt)
	}
	d.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.reader, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run performs one shared dial and publishes its outcome. A failed dial leaves no reader, so the
// next read starts a fresh attempt rather than inheriting a stale failure.
func (d *suiNodeDialer) run(attempt *suiDialAttempt) {
	ctx, cancel := context.WithTimeout(context.Background(), suiDialTimeout)
	defer cancel()
	reader, err := d.dial(ctx)
	if err == nil && reader == nil {
		err = errors.New("Sui node dialer returned no reader")
	}
	d.mu.Lock()
	if err == nil {
		d.reader = reader
	}
	d.attempt = nil
	d.mu.Unlock()
	attempt.reader, attempt.err = reader, err
	close(attempt.done)
}
