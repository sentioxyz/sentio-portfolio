package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/protobuf/proto"
)

const (
	suiEngineTestUSDC = "0x00000000000000000000000000000000000000000000000000000000000000aa::usdc::USDC"
	suiEngineTestGIFT = "0x00000000000000000000000000000000000000000000000000000000000000bb::gift::GIFT"
)

var suiEngineTestBase = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

func suiEngineCheckpoint(sequence uint64, at time.Time) SuiCheckpoint {
	return SuiCheckpoint{Sequence: sequence, Digest: [32]byte{1, 2, 3}, Timestamp: at}
}

func suiEngineCoin(coinType string) SuiCoinMetadata {
	switch coinType {
	case suiLongType:
		return SuiCoinMetadata{CoinType: coinType, Symbol: "SUI", Name: "Sui", Decimals: 9}
	case suiEngineTestUSDC:
		return SuiCoinMetadata{CoinType: coinType, Symbol: "USDC", Name: "USD Coin", Decimals: 6}
	}
	return SuiCoinMetadata{CoinType: coinType, Symbol: "GIFT", Name: "Gift", Decimals: 6}
}

func suiEngineProtocolName(id string) string {
	for _, route := range suiRoutes {
		if route.id == id {
			return route.name
		}
	}
	return id
}

// suiEnginePositions is one lending group holding amount of each coin as an asset.
func suiEnginePositions(protocol string, checkpoint SuiCheckpoint, amounts map[string]string) suiPositions {
	group := SuiGroup{ID: protocol + ":position", MarketID: "0", Label: "Lending", Metadata: map[string]any{"stateMode": "historical"}}
	for _, coin := range slices.Sorted(func(yield func(string) bool) {
		for coin := range amounts {
			if !yield(coin) {
				return
			}
		}
	}) {
		group.Components = append(group.Components, SuiComponent{Kind: "asset", Coin: suiEngineCoin(coin), AmountRaw: amounts[coin]})
	}
	return suiPositions{ProtocolID: protocol, ProtocolName: suiEngineProtocolName(protocol), Checkpoint: checkpoint, HeadBeforeRead: checkpoint.Sequence, Groups: []SuiGroup{group}}
}

func suiEngineSample(protocol string, sequence uint64, start, until time.Time, amount string) suiHistorySample {
	return suiHistorySample{Start: start, Until: until, Positions: suiEnginePositions(protocol, suiEngineCheckpoint(sequence, start), map[string]string{suiLongType: amount})}
}

// hourlySamples are count hourly samples from base, the first at checkpoint first.
func hourlySamples(protocol string, first uint64, count int) []suiHistorySample {
	out := make([]suiHistorySample, count)
	for h := range count {
		start := suiEngineTestBase.Add(time.Duration(h) * time.Hour)
		out[h] = suiEngineSample(protocol, first+uint64(h), start, start.Add(time.Hour), fmt.Sprint(1_000_000_000*(h+1)))
	}
	return out
}

// fakeSuiIndex serves samples the way a processor index does: a checkpoint read gets the newest
// sample at or before it, a range read every sample meeting the range, a latest read latest.
type fakeSuiIndex struct {
	id      string
	samples []suiHistorySample
	latest  suiPositions
	// rangeErr fails range reads; stall makes them wait for their context, as a statement still
	// running does.
	rangeErr error
	stall    bool

	mu                                       sync.Mutex
	latestReads, checkpointReads, rangeReads int
	ranges                                   [][2]time.Time
}

func (f *fakeSuiIndex) ReadLatest(context.Context, SuiAddress) (suiPositions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestReads++
	if f.latest.ProtocolID == "" {
		return suiPositions{ProtocolID: f.id}, errors.New("index has no latest sample")
	}
	return f.latest, nil
}

func (f *fakeSuiIndex) ReadAtCheckpoint(_ context.Context, _ SuiAddress, sequence uint64) (suiPositions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpointReads++
	for i := len(f.samples) - 1; i >= 0; i-- {
		if sample := f.samples[i]; sample.Positions.Checkpoint.Sequence <= sequence {
			return sample.Positions, sample.Err
		}
	}
	return suiPositions{ProtocolID: f.id}, errors.New("Sui index has no completed sample at the requested checkpoint")
}

func (f *fakeSuiIndex) ReadRange(ctx context.Context, _ SuiAddress, from, to time.Time) ([]suiHistorySample, error) {
	f.mu.Lock()
	f.rangeReads++
	f.ranges = append(f.ranges, [2]time.Time{from, to})
	f.mu.Unlock()
	if f.stall {
		<-ctx.Done()
		return nil, fmt.Errorf("statement failed: %w", ctx.Err())
	}
	if f.rangeErr != nil {
		return nil, f.rangeErr
	}
	var out []suiHistorySample
	for _, sample := range f.samples {
		if !sample.Start.After(to) && sample.Until.After(from) {
			out = append(out, sample)
		}
	}
	return out, nil
}

func (f *fakeSuiIndex) reads() (latest, checkpoint, ranges int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.latestReads, f.checkpointReads, f.rangeReads
}

// fakeSuiNode is a node at head with balances. State reads (heads and holdings) and metadata reads
// are counted apart: a pinned scan may look up a coin's immutable metadata, never state.
type fakeSuiNode struct {
	head     SuiCheckpoint
	balances []SuiCoinBalance
	metadata map[string]SuiCoinMetadata

	mu                        sync.Mutex
	stateReads, metadataReads int
}

func (f *fakeSuiNode) LatestCheckpoint(context.Context) (SuiCheckpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReads++
	return f.head, nil
}

func (f *fakeSuiNode) CheckpointBySequence(_ context.Context, sequence uint64) (SuiCheckpoint, error) {
	return suiEngineCheckpoint(sequence, f.head.Timestamp), nil
}

func (f *fakeSuiNode) Holdings(context.Context, SuiAddress, *SuiCheckpoint) (SuiHoldings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateReads++
	return SuiHoldings{Balances: f.balances, Checkpoint: f.head, HeadBeforeRead: f.head.Sequence - 2}, nil
}

func (f *fakeSuiNode) CoinMetadata(_ context.Context, coins []string) (map[string]SuiCoinMetadata, map[string]error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metadataReads++
	found, unusable := map[string]SuiCoinMetadata{}, map[string]error{}
	for _, coin := range coins {
		if metadata, ok := f.metadata[coin]; ok {
			found[coin] = metadata
		} else {
			unusable[coin] = errors.New("coin metadata declares no decimals")
		}
	}
	return found, unusable, nil
}

func (f *fakeSuiNode) Close() {}

func (f *fakeSuiNode) counts() (state, metadata int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateReads, f.metadataReads
}

// fakeSuiNodeProtocols answers node protocol reads with positions, or err. A read of stall waits
// for its context, as object reads against a slow node do.
type fakeSuiNodeProtocols struct {
	positions map[string]suiPositions
	err       error
	stall     string

	mu    sync.Mutex
	reads []string
}

func (f *fakeSuiNodeProtocols) ReadLatest(ctx context.Context, id string, _ SuiAddress, reader SuiReader) (suiPositions, error) {
	f.mu.Lock()
	f.reads = append(f.reads, id)
	f.mu.Unlock()
	if id == f.stall {
		<-ctx.Done()
		return suiPositions{ProtocolID: id}, fmt.Errorf("object read failed: %w", ctx.Err())
	}
	if f.err != nil {
		return suiPositions{ProtocolID: id}, f.err
	}
	if positions, ok := f.positions[id]; ok {
		return positions, nil
	}
	head, err := reader.LatestCheckpoint(context.Background())
	return suiPositions{ProtocolID: id, ProtocolName: suiEngineProtocolName(id), Checkpoint: head}, err
}

// fakeSuiPrices prices SUI by the instant asked about, so a quote at the wrong instant shows, and
// USDC at one dollar. It answers nothing at an instant in unavailable, or once its context is done,
// as a price service that is down does.
type fakeSuiPrices struct {
	unavailable map[time.Time]bool
	admit       func([]string) []string

	mu      sync.Mutex
	queries []SuiPriceQuery
}

func suiEngineTestPrice(at time.Time) float64 {
	if at.IsZero() {
		return 2
	}
	return 1 + float64(at.Unix()%10_000)/100
}

func (f *fakeSuiPrices) SuiUSDPrices(ctx context.Context, queries []SuiPriceQuery) SuiPriceResult {
	f.mu.Lock()
	f.queries = append(f.queries, queries...)
	f.mu.Unlock()
	result := SuiPriceResult{Prices: make([]map[string]float64, len(queries))}
	for i, query := range queries {
		if f.unavailable[query.At] || ctx.Err() != nil {
			result.Failures = append(result.Failures, SuiPriceFailure{Query: i, Message: "price service is down", Unavailable: true})
			continue
		}
		result.Prices[i] = map[string]float64{}
		for _, coin := range query.CoinTypes {
			switch coin {
			case suiLongType:
				result.Prices[i][coin] = suiEngineTestPrice(query.At)
			case suiEngineTestUSDC:
				result.Prices[i][coin] = 1
			}
		}
	}
	return result
}

func (f *fakeSuiPrices) AdmitSuiCoins(coins []string) []string {
	if f.admit != nil {
		return f.admit(coins)
	}
	return coins
}

func (f *fakeSuiPrices) instants() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Time, len(f.queries))
	for i, query := range f.queries {
		out[i] = query.At
	}
	return out
}

// fakeSuiWallet holds balances by checkpoint.
type fakeSuiWallet struct {
	balances map[uint64][]SuiCoinBalance
	err      error

	mu    sync.Mutex
	calls [][]uint64
}

func (f *fakeSuiWallet) BalancesAt(_ context.Context, _ SuiAddress, pins []uint64) (map[uint64][]SuiCoinBalance, error) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(pins))
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := map[uint64][]SuiCoinBalance{}
	for _, pin := range pins {
		out[pin] = f.balances[pin]
	}
	return out, nil
}

// suiEngineTest wires a Sui engine to fakes. A nil node fails the test if anything dials it.
type suiEngineTest struct {
	t       *testing.T
	node    *fakeSuiNode
	nodeErr error
	// dialDelay holds each dial open, so reads that arrive meanwhile share it.
	dialDelay time.Duration
	dials     atomic.Int32
	nodes     *fakeSuiNodeProtocols
	prices    *fakeSuiPrices
	wallet    *fakeSuiWallet
	indexes   []*fakeSuiIndex
	now       time.Time
	observer  Observer
}

func (s *suiEngineTest) engine() *SuiEngine {
	s.t.Helper()
	config := SuiConfig{Now: func() time.Time { return s.now }}
	if s.node != nil || s.nodeErr != nil {
		config.Node = func(context.Context) (SuiReader, error) {
			s.dials.Add(1)
			time.Sleep(s.dialDelay)
			if s.nodeErr != nil {
				return nil, s.nodeErr
			}
			return s.node, nil
		}
	}
	if s.prices != nil {
		config.Prices = s.prices
	}
	if s.wallet != nil {
		config.WalletHistory = s.wallet
	}
	e, err := NewSuiEngine(NewEngineWithConfig(nil, nil, EngineConfig{Observer: s.observer}), config)
	if err != nil {
		s.t.Fatal(err)
	}
	if s.nodes != nil {
		e.nodes = s.nodes
	}
	for _, index := range s.indexes {
		e.indexes[index.id] = index
	}
	return e
}

func suiEngineOwner(t *testing.T) SuiAddress {
	owner, err := ParseSuiAddress("0x77")
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func suiSnapshotOf(response *SuiResponse, protocol string) *SuiSnapshot {
	for i := range response.Snapshots {
		if response.Snapshots[i].ProtocolID == protocol {
			return &response.Snapshots[i]
		}
	}
	return nil
}

func suiErrorsOf(response *SuiResponse, protocol, scope string) []string {
	var out []string
	for _, scanError := range response.Errors {
		if scanError.ProtocolID == protocol && scanError.Scope == scope {
			out = append(out, scanError.Message)
		}
	}
	return out
}

func suiHasError(response *SuiResponse, protocol, scope, fragment string) bool {
	return slices.ContainsFunc(suiErrorsOf(response, protocol, scope), func(message string) bool { return strings.Contains(message, fragment) })
}

// The rule the Sui engine exists to keep: a pinned read never asks the node for state. The indexed
// protocols answer from their samples, the wallet from wallet history at their checkpoint, and the
// latest-only protocols say they were not read rather than answering for the head.
func TestSuiPinnedScanNeverReadsNodeState(t *testing.T) {
	owner := suiEngineOwner(t)
	for _, pin := range []SuiPin{SuiAtCheckpoint(1005), SuiAtTime(suiEngineTestBase.Add(5*time.Hour + 30*time.Minute))} {
		node := &fakeSuiNode{head: suiEngineCheckpoint(9_000, suiEngineTestBase.Add(48*time.Hour)), metadata: map[string]SuiCoinMetadata{suiLongType: suiEngineCoin(suiLongType)}}
		nodes := &fakeSuiNodeProtocols{}
		navi := &fakeSuiIndex{id: "navi", samples: hourlySamples("navi", 1000, 24)}
		suilend := &fakeSuiIndex{id: "suilend", samples: hourlySamples("suilend", 1000, 24)}
		wallet := &fakeSuiWallet{balances: map[uint64][]SuiCoinBalance{1005: {{CoinType: suiLongType, Amount: big.NewInt(7)}}}}
		test := &suiEngineTest{t: t, node: node, nodes: nodes, prices: &fakeSuiPrices{}, wallet: wallet, indexes: []*fakeSuiIndex{navi, suilend}}
		response := test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: pin})

		if state, _ := node.counts(); state != 0 || len(nodes.reads) != 0 {
			t.Fatalf("pinned scan read node state: %d state reads, protocol reads %v", state, nodes.reads)
		}
		for _, id := range []string{"volo-vaults", "cetus", "bluefin"} {
			if !suiHasError(response, id, "protocol", "only latest state is supported") || suiSnapshotOf(response, id) != nil {
				t.Fatalf("%s at a pin: %+v", id, response.Errors)
			}
		}
		for _, id := range []string{"wallet", "navi", "suilend"} {
			if snapshot := suiSnapshotOf(response, id); snapshot == nil || snapshot.Checkpoint.Sequence != 1005 {
				t.Fatalf("%s not answered at the sample: %+v %+v", id, snapshot, response.Errors)
			}
		}
		if slices.Contains(test.prices.instants(), time.Time{}) {
			t.Fatal("a pinned scan asked for a latest quote")
		}
	}

	// Without the wallet, a pinned scan does not even dial.
	test := &suiEngineTest{t: t, nodeErr: errors.New("must not dial"), indexes: []*fakeSuiIndex{{id: "suilend", samples: hourlySamples("suilend", 1000, 3)}}}
	response := test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: SuiAtCheckpoint(1001), ProtocolIDs: map[string]struct{}{"suilend": {}, "cetus": {}}})
	if test.dials.Load() != 0 || suiSnapshotOf(response, "suilend") == nil {
		t.Fatalf("dials %d, response %+v", test.dials.Load(), response)
	}
}

// Suilend has no node path and NAVI's index answers only pins: each latest read uses its own
// source alone, and a request for indexed protocols needs no node.
func TestSuiLatestReadsEachProtocolFromItsOwnSource(t *testing.T) {
	owner := suiEngineOwner(t)
	sample := suiEngineCheckpoint(1000, suiEngineTestBase)
	suilend := &fakeSuiIndex{id: "suilend", latest: suiEnginePositions("suilend", sample, map[string]string{suiLongType: "1000000000"})}
	navi := &fakeSuiIndex{id: "navi", samples: hourlySamples("navi", 1000, 2)}

	// Indexed alone, with no node configured at all.
	test := &suiEngineTest{t: t, indexes: []*fakeSuiIndex{suilend, navi}, now: suiEngineTestBase.Add(time.Hour)}
	response := test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"suilend": {}}})
	if snapshot := suiSnapshotOf(response, "suilend"); snapshot == nil || snapshot.Checkpoint != sample || response.Checkpoint != sample {
		t.Fatalf("indexed latest: %+v", response)
	}

	// A failed NAVI node read stays a failure: its index answers for a different checkpoint.
	test = &suiEngineTest{t: t, node: &fakeSuiNode{head: suiEngineCheckpoint(2000, suiEngineTestBase)}, nodes: &fakeSuiNodeProtocols{err: errors.New("object read failed")}, indexes: []*fakeSuiIndex{suilend, navi}, now: suiEngineTestBase.Add(time.Hour)}
	response = test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"navi": {}}})
	if latest, checkpoint, ranges := navi.reads(); latest+checkpoint+ranges != 0 {
		t.Fatalf("NAVI's index was read for a latest request: %d %d %d", latest, checkpoint, ranges)
	}
	if !suiHasError(response, "navi", "protocol", "object read failed") || suiSnapshotOf(response, "navi") != nil {
		t.Fatalf("NAVI node failure: %+v", response)
	}

	// Without an index, Suilend is not read, and the node is not asked in its place.
	test = &suiEngineTest{t: t, nodeErr: errors.New("must not dial")}
	response = test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"suilend": {}}})
	if test.dials.Load() != 0 || !suiHasError(response, "suilend", "protocol", "not configured") {
		t.Fatalf("unconfigured Suilend: dials %d, %+v", test.dials.Load(), response.Errors)
	}
}

// A sample old enough to suggest a stalled index does not stand for the latest state.
func TestSuiIndexedLatestIsGatedByFreshness(t *testing.T) {
	owner := suiEngineOwner(t)
	sample := suiEngineCheckpoint(1000, suiEngineTestBase)
	for _, test := range []struct {
		age   time.Duration
		fresh bool
	}{{2 * time.Hour, true}, {2*time.Hour + 2*time.Minute, false}, {-time.Minute, false}} {
		suilend := &fakeSuiIndex{id: "suilend", latest: suiEnginePositions("suilend", sample, map[string]string{suiLongType: "1"})}
		engine := (&suiEngineTest{t: t, indexes: []*fakeSuiIndex{suilend}, now: suiEngineTestBase.Add(test.age)}).engine()
		response := engine.ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"suilend": {}}})
		if fresh := suiSnapshotOf(response, "suilend") != nil; fresh != test.fresh {
			t.Fatalf("age %s: read %v, errors %+v", test.age, fresh, response.Errors)
		}
		if !test.fresh && !suiHasError(response, "suilend", "protocol", "has not completed a recent sample") {
			t.Fatalf("age %s: %+v", test.age, response.Errors)
		}
	}
}

// A node outage costs the protocols that need the node, not the ones that do not.
func TestSuiNodeOutageLeavesIndexedProtocolsAnswered(t *testing.T) {
	owner := suiEngineOwner(t)
	sample := suiEngineCheckpoint(1000, suiEngineTestBase)
	suilend := &fakeSuiIndex{id: "suilend", latest: suiEnginePositions("suilend", sample, map[string]string{suiLongType: "1"})}
	test := &suiEngineTest{t: t, nodeErr: errors.New("connection refused"), dialDelay: 50 * time.Millisecond, indexes: []*fakeSuiIndex{suilend}, now: suiEngineTestBase.Add(time.Hour)}
	response := test.engine().Scan(context.Background(), owner)
	if suiSnapshotOf(response, "suilend") == nil {
		t.Fatalf("Suilend lost to a node outage: %+v", response.Errors)
	}
	for _, id := range []string{"wallet", "navi", "volo-vaults", "cetus", "bluefin"} {
		if !suiHasError(response, id, "protocol", "Sui node is unavailable") {
			t.Fatalf("%s: %+v", id, response.Errors)
		}
	}
	if test.dials.Load() != 1 {
		t.Fatalf("concurrent reads dialled %d times, want one shared dial", test.dials.Load())
	}
}

// An indexed answer must name a whole sample at or before what was asked.
func TestSuiPinnedScanRejectsSamplesPastThePin(t *testing.T) {
	owner := suiEngineOwner(t)
	late := suiEnginePositions("suilend", suiEngineCheckpoint(1010, suiEngineTestBase), map[string]string{suiLongType: "1"})
	noDigest := suiEnginePositions("suilend", suiEngineCheckpoint(1000, suiEngineTestBase), map[string]string{suiLongType: "1"})
	noDigest.Checkpoint.Digest = [32]byte{}
	for name, positions := range map[string]suiPositions{"after the pin": late, "without a digest": noDigest} {
		engine := (&suiEngineTest{t: t}).engine()
		engine.indexes["suilend"] = suiIndexAnswering{positions}
		response := engine.ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: SuiAtCheckpoint(1005), ProtocolIDs: map[string]struct{}{"suilend": {}}})
		if suiSnapshotOf(response, "suilend") != nil || !suiHasError(response, "suilend", "protocol", "invalid sample") {
			t.Fatalf("%s: %+v", name, response)
		}
	}
}

// suiIndexAnswering answers every read with positions.
type suiIndexAnswering struct{ positions suiPositions }

func (s suiIndexAnswering) ReadLatest(context.Context, SuiAddress) (suiPositions, error) {
	return s.positions, nil
}
func (s suiIndexAnswering) ReadAtCheckpoint(context.Context, SuiAddress, uint64) (suiPositions, error) {
	return s.positions, nil
}
func (s suiIndexAnswering) ReadRange(context.Context, SuiAddress, time.Time, time.Time) ([]suiHistorySample, error) {
	return nil, nil
}

// Head state is valued at the latest quote and a sample at its own timestamp; a debt nets out of
// its group; an unquoted protocol coin is a reported gap, an unquoted wallet coin is not.
func TestSuiLatestValuation(t *testing.T) {
	owner := suiEngineOwner(t)
	head := suiEngineCheckpoint(2000, suiEngineTestBase.Add(3*time.Hour))
	sample := suiEngineCheckpoint(1000, suiEngineTestBase)
	navi := suiPositions{ProtocolID: "navi", ProtocolName: "NAVI", Checkpoint: head, HeadBeforeRead: 1998, Groups: []SuiGroup{{
		ID: "navi:account", Label: "Lending", Components: []SuiComponent{
			{Kind: "asset", Coin: suiEngineCoin(suiLongType), AmountRaw: "3000000000"},
			{Kind: "debt", Coin: suiEngineCoin(suiEngineTestUSDC), AmountRaw: "1500000"},
			{Kind: "reward", Coin: suiEngineCoin(suiEngineTestGIFT), AmountRaw: "9"},
		}}}}
	node := &fakeSuiNode{head: head,
		balances: []SuiCoinBalance{{CoinType: suiLongType, Amount: big.NewInt(500_000_000)}, {CoinType: suiEngineTestGIFT, Amount: big.NewInt(4)}, {CoinType: suiEngineTestUSDC, Amount: big.NewInt(2_000_000)}},
		metadata: map[string]SuiCoinMetadata{suiLongType: suiEngineCoin(suiLongType), suiEngineTestGIFT: suiEngineCoin(suiEngineTestGIFT)}}
	suilend := &fakeSuiIndex{id: "suilend", latest: suiEnginePositions("suilend", sample, map[string]string{suiLongType: "1000000000"})}
	prices := &fakeSuiPrices{}
	test := &suiEngineTest{t: t, node: node, nodes: &fakeSuiNodeProtocols{positions: map[string]suiPositions{"navi": navi}}, prices: prices, indexes: []*fakeSuiIndex{suilend}, now: suiEngineTestBase.Add(time.Hour)}
	response := test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"wallet": {}, "navi": {}, "suilend": {}}})

	naviSnapshot := suiSnapshotOf(response, "navi")
	if naviSnapshot == nil || naviSnapshot.Checkpoint != head {
		t.Fatalf("navi: %+v", response)
	}
	group := naviSnapshot.Groups[0]
	if *group.Components[0].PriceUSD != 2 || *group.Components[0].ValueUSD != 6 || *group.Components[1].ValueUSD != 1.5 || group.Components[2].PriceUSD != nil || *group.ValueUSD != 4.5 {
		t.Fatalf("navi group valuation: %+v", group)
	}
	summary := response.ProtocolSummaries[slices.IndexFunc(response.ProtocolSummaries, func(s ProtocolSummary) bool { return s.ProtocolID == "navi" })]
	if summary.AssetUSD != 6 || summary.DebtUSD != 1.5 || summary.TotalUSD != 4.5 || summary.ComponentCount != 3 || summary.PricedComponents != 2 {
		t.Fatalf("navi summary: %+v", summary)
	}
	if !suiHasError(response, "navi", "pricing", "no admissible USD price for "+suiEngineTestGIFT) {
		t.Fatalf("unquoted protocol coin is not reported: %+v", response.Errors)
	}
	if got := *suiSnapshotOf(response, "suilend").Groups[0].Components[0].PriceUSD; got != suiEngineTestPrice(sample.Timestamp) {
		t.Fatalf("Suilend valued at %v, want the quote at its sample", got)
	}

	wallet := suiSnapshotOf(response, "wallet")
	if wallet == nil || len(wallet.Groups) != 2 || wallet.Checkpoint != head || wallet.Groups[0].Metadata["headBeforeRead"] != uint64(1998) {
		t.Fatalf("wallet: %+v", wallet)
	}
	if len(suiErrorsOf(response, "wallet", "pricing")) != 0 {
		t.Fatalf("an unquoted wallet coin was reported: %+v", response.Errors)
	}
	if !suiHasError(response, "wallet", "protocol", "1 coin type(s) without usable on-chain metadata were omitted: "+suiEngineTestUSDC) {
		t.Fatalf("wallet coin without metadata: %+v", response.Errors)
	}
	if response.Checkpoint != head || response.Prices["sui:"+suiEngineTestUSDC] != 1 {
		t.Fatalf("response checkpoint %+v prices %v", response.Checkpoint, response.Prices)
	}
}

// A protocol that runs out of time on a live scan does not cost the others their valuation: reads
// stop short of the deadline, and what they read is still priced.
func TestSuiLatestOutOfTimeStillPricesWhatWasRead(t *testing.T) {
	owner := suiEngineOwner(t)
	node := &fakeSuiNode{head: suiEngineCheckpoint(2000, suiEngineTestBase),
		balances: []SuiCoinBalance{{CoinType: suiLongType, Amount: big.NewInt(500_000_000)}},
		metadata: map[string]SuiCoinMetadata{suiLongType: suiEngineCoin(suiLongType)}}
	test := &suiEngineTest{t: t, node: node, nodes: &fakeSuiNodeProtocols{stall: "navi"}, prices: &fakeSuiPrices{}, now: suiEngineTestBase}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	response := test.engine().ScanWithOptions(ctx, owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"wallet": {}, "navi": {}}})
	wallet := suiSnapshotOf(response, "wallet")
	if wallet == nil || wallet.Groups[0].Components[0].PriceUSD == nil || len(suiErrorsOf(response, "wallet", "pricing")) != 0 {
		t.Fatalf("the wallet read was not priced: %+v %+v", wallet, response.Errors)
	}
	if !suiHasError(response, "navi", "protocol", context.DeadlineExceeded.Error()) {
		t.Fatalf("NAVI ran out of time unreported: %+v", response.Errors)
	}
}

// suiRangeFixture is NAVI sampled hourly and Suilend daily from the test base, so an instant late
// in the day finds them at different checkpoints, with the day's sample the older.
func suiRangeFixture(t *testing.T) (*suiEngineTest, *fakeSuiIndex, *fakeSuiIndex) {
	navi := &fakeSuiIndex{id: "navi", samples: hourlySamples("navi", 1000, 24)}
	day := suiEngineSample("suilend", 1000, suiEngineTestBase, suiEngineTestBase.Add(24*time.Hour), "5000000000")
	suilend := &fakeSuiIndex{id: "suilend", samples: []suiHistorySample{day}}
	node := &fakeSuiNode{metadata: map[string]SuiCoinMetadata{suiLongType: suiEngineCoin(suiLongType)}}
	wallet := &fakeSuiWallet{balances: map[uint64][]SuiCoinBalance{1000: {{CoinType: suiLongType, Amount: big.NewInt(2_000_000_000)}}}}
	return &suiEngineTest{t: t, node: node, prices: &fakeSuiPrices{}, wallet: wallet, indexes: []*fakeSuiIndex{navi, suilend}}, navi, suilend
}

// A range reads each index once over its span, answers every protocol of an instant at one
// checkpoint, and reads the wallet there once for every instant.
func TestSuiRangeAlignsEveryInstantToOneCheckpoint(t *testing.T) {
	owner := suiEngineOwner(t)
	test, navi, suilend := suiRangeFixture(t)
	instants := []time.Time{suiEngineTestBase.Add(5*time.Hour + 30*time.Minute), suiEngineTestBase.Add(6*time.Hour + 30*time.Minute)}
	scans := test.engine().ScanRange(context.Background(), owner, instants)

	if _, _, ranges := suilend.reads(); ranges != 1 {
		t.Fatalf("Suilend range reads %d", ranges)
	}
	// One read over the instants, one for the day's checkpoint they align to.
	if _, _, ranges := navi.reads(); ranges != 2 {
		t.Fatalf("NAVI range reads %d", ranges)
	}
	if len(test.wallet.calls) != 1 || !slices.Equal(test.wallet.calls[0], []uint64{1000}) {
		t.Fatalf("wallet reads %v", test.wallet.calls)
	}
	for i, scan := range scans {
		if scan.Retry != nil {
			t.Fatalf("instant %d: retry %v", i, scan.Retry)
		}
		for _, id := range []string{"wallet", "navi", "suilend"} {
			snapshot := suiSnapshotOf(scan.Response, id)
			if snapshot == nil || snapshot.Checkpoint.Sequence != 1000 {
				t.Fatalf("instant %d %s: %+v %+v", i, id, snapshot, scan.Response.Errors)
			}
		}
		if got := suiSnapshotOf(scan.Response, "navi").Groups[0].Metadata["requestedTimestamp"]; got != instants[i].Format(time.RFC3339Nano) {
			t.Fatalf("instant %d answered as %v", i, got)
		}
		wallet := suiSnapshotOf(scan.Response, "wallet")
		if *wallet.Groups[0].Components[0].PriceUSD != suiEngineTestPrice(suiEngineTestBase) {
			t.Fatalf("wallet priced at %v", *wallet.Groups[0].Components[0].PriceUSD)
		}
	}
}

// A pinned time scan is a range of one instant.
func TestSuiPinnedTimeScanAnswersAsARangeInstant(t *testing.T) {
	owner := suiEngineOwner(t)
	at := suiEngineTestBase.Add(7*time.Hour + 10*time.Minute)
	test, _, _ := suiRangeFixture(t)
	engine := test.engine()
	ranged := engine.ScanRange(context.Background(), owner, []time.Time{at})[0].Response
	single := engine.ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: SuiAtTime(at), ProtocolIDs: map[string]struct{}{"wallet": {}, "navi": {}, "suilend": {}}})
	ranged.CompletedAt, single.CompletedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(ranged, single) {
		t.Fatalf("range instant and pinned scan differ:\n%+v\n%+v", ranged, single)
	}

	// The same holds when the pin is a checkpoint: 1007 finds NAVI's hour and Suilend's day, and
	// both are answered at the day's checkpoint with the wallet.
	pinned := engine.ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: SuiAtCheckpoint(1007)})
	for _, id := range []string{"wallet", "navi", "suilend"} {
		if snapshot := suiSnapshotOf(pinned, id); snapshot == nil || snapshot.Checkpoint.Sequence != 1000 {
			t.Fatalf("checkpoint pin %s: %+v %+v", id, snapshot, pinned.Errors)
		}
	}
}

// Retry marks an instant a source did not answer; an answer, even an unusable one, is final.
func TestSuiRangeRetriesOnlyWhatASourceDidNotAnswer(t *testing.T) {
	owner := suiEngineOwner(t)
	at := []time.Time{suiEngineTestBase.Add(2*time.Hour + 10*time.Minute)}
	for _, test := range []struct {
		name  string
		edit  func(*suiEngineTest, *fakeSuiIndex, *fakeSuiIndex)
		retry string
	}{
		{"answered", func(*suiEngineTest, *fakeSuiIndex, *fakeSuiIndex) {}, ""},
		{"index failed", func(_ *suiEngineTest, _, suilend *fakeSuiIndex) { suilend.rangeErr = errors.New("statement failed") }, "suilend index"},
		{"uncertified sample", func(_ *suiEngineTest, navi, _ *fakeSuiIndex) {
			navi.samples[2].Err = errors.New("certificate counts fewer rows than visible")
		}, ""},
		{"wallet not ingested", func(s *suiEngineTest, _, _ *fakeSuiIndex) {
			s.wallet.err = fmt.Errorf("checkpoint 1000: %w", ErrSuiWalletNotIngested)
		}, "wallet balances"},
		{"wallet not covered", func(s *suiEngineTest, _, _ *fakeSuiIndex) {
			s.wallet.err = fmt.Errorf("too many coins: %w", ErrSuiWalletNotCovered)
		}, ""},
		{"prices unavailable", func(s *suiEngineTest, _, _ *fakeSuiIndex) {
			s.prices.unavailable = map[time.Time]bool{suiEngineTestBase: true}
		}, "prices at"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, navi, suilend := suiRangeFixture(t)
			test.edit(fixture, navi, suilend)
			scan := fixture.engine().ScanRange(context.Background(), owner, at)[0]
			if (scan.Retry == nil) != (test.retry == "") || (scan.Retry != nil && !strings.Contains(scan.Retry.Error(), test.retry)) {
				t.Fatalf("retry %v, want %q; errors %+v", scan.Retry, test.retry, scan.Response.Errors)
			}
		})
	}

	// A coin with no quote is a gap, not a reason to ask again. The instant is answered at the
	// day's checkpoint, so NAVI's first hour is the sample it reads.
	fixture, navi, _ := suiRangeFixture(t)
	navi.samples[0].Positions.Groups[0].Components[0].Coin = suiEngineCoin(suiEngineTestGIFT)
	scan := fixture.engine().ScanRange(context.Background(), owner, at)[0]
	if scan.Retry != nil || !suiHasError(scan.Response, "navi", "pricing", "no admissible USD price at the sample for "+suiEngineTestGIFT) {
		t.Fatalf("gap: retry %v errors %+v", scan.Retry, scan.Response.Errors)
	}
}

// Reads that run out of time end early so what they read is still priced, and the instants they
// did not reach are marked unread.
func TestSuiRangeOutOfTimeIsUnreadAndStillPriced(t *testing.T) {
	owner := suiEngineOwner(t)
	fixture, _, suilend := suiRangeFixture(t)
	suilend.stall = true
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	scan := fixture.engine().ScanRange(ctx, owner, []time.Time{suiEngineTestBase.Add(3 * time.Hour)})[0]
	if !errors.Is(scan.Retry, ErrSuiRangeUnread) {
		t.Fatalf("retry %v", scan.Retry)
	}
	navi := suiSnapshotOf(scan.Response, "navi")
	if navi == nil || navi.Groups[0].Components[0].PriceUSD == nil {
		t.Fatalf("what was read was not priced: %+v %+v", navi, scan.Response.Errors)
	}
}

// Without wallet history the wallet is not read at a pin, rather than shown empty.
func TestSuiPinnedWalletWithoutHistoryIsNotRead(t *testing.T) {
	owner := suiEngineOwner(t)
	fixture, _, _ := suiRangeFixture(t)
	fixture.wallet = nil
	response := fixture.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{Pin: SuiAtTime(suiEngineTestBase.Add(time.Hour))})
	if suiSnapshotOf(response, "wallet") != nil || !suiHasError(response, "wallet", "protocol", "wallet history is not configured") {
		t.Fatalf("%+v", response)
	}
	if suiSnapshotOf(response, "suilend") == nil || suiSnapshotOf(response, "navi") == nil {
		t.Fatalf("indexed protocols: %+v", response.Errors)
	}
}

func TestSuiProtocolsReportWhatIsConfigured(t *testing.T) {
	test := &suiEngineTest{t: t, nodeErr: errors.New("unused"), indexes: []*fakeSuiIndex{{id: "suilend"}}}
	engine := test.engine()
	got := map[string]SuiProtocolInfo{}
	for _, info := range engine.Protocols() {
		got[info.ID] = info
	}
	want := map[string]SuiProtocolInfo{
		"wallet":      {ID: "wallet", Name: "Wallet", History: true, Latest: true},
		"navi":        {ID: "navi", Name: "NAVI", History: true, Latest: true},
		"volo-vaults": {ID: "volo-vaults", Name: "Volo Vaults", Latest: true},
		"suilend":     {ID: "suilend", Name: "Suilend", History: true, Latest: true, Pinned: true},
		"cetus":       {ID: "cetus", Name: "Cetus", Latest: true},
		"bluefin":     {ID: "bluefin", Name: "Bluefin", Latest: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("protocols %+v", got)
	}
	if err := engine.HistoryReady(); err == nil || !strings.Contains(err.Error(), "wallet") || !strings.Contains(err.Error(), "navi") {
		t.Fatalf("history ready: %v", err)
	}
	test.wallet, test.indexes = &fakeSuiWallet{}, []*fakeSuiIndex{{id: "suilend"}, {id: "navi"}}
	if err := test.engine().HistoryReady(); err != nil {
		t.Fatal(err)
	}
}

// The engine takes each index from the configuration its Engine was built with, and reports the
// node's requests to that Engine's observer under the protocol they served.
func TestSuiEngineWiresIndexesAndObserverFromItsEngine(t *testing.T) {
	base, owner := suilendFixture()
	fixture, _ := newSQLPortfolioFixture(t, "suilend", base)
	server := newSuiGRPCTestServer(t)
	server.balances = []*rpcv2.Balance{suiTestBalance(suiLongType, 5)}
	server.metadata = map[string]*rpcv2.CoinMetadata{suiLongType: {Decimals: proto.Uint32(9), Name: proto.String("Sui"), Symbol: proto.String("SUI")}}
	observer := &recordingObserver{}
	engine := NewEngineWithConfig(nil, nil, EngineConfig{Observer: observer, SentioIndexers: map[string]SentioIndexerConfig{"suilend": {SQLURL: fixture.url, ProcessorVersion: "1"}}})
	sui, err := NewSuiEngine(engine, SuiConfig{Node: SuiGRPCNode(server.endpoint()), Now: func() time.Time { return fixture.pin.Timestamp.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	fastSQL(sui.indexes["suilend"].(*suiHistoryIndex).api)
	response := sui.ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"wallet": {}, "suilend": {}}})
	if snapshot := suiSnapshotOf(response, "suilend"); snapshot == nil || snapshot.Checkpoint != fixture.pin || fixture.calls != 1 {
		t.Fatalf("suilend through the configured index: %+v %+v", snapshot, response.Errors)
	}
	if suiSnapshotOf(response, "wallet") == nil {
		t.Fatalf("wallet: %+v", response.Errors)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if !slices.ContainsFunc(observer.rpcs, func(o RPCObservation) bool {
		return o.ProtocolID == "wallet" && strings.Contains(o.Method, "ListBalances")
	}) {
		t.Fatalf("node requests were not observed under their protocol: %+v", observer.rpcs)
	}

	if _, err := NewSuiEngine(NewEngineWithConfig(nil, nil, EngineConfig{SentioIndexers: map[string]SentioIndexerConfig{"navi": {SQLURL: fixture.url}}}), SuiConfig{}); err == nil {
		t.Fatal("an index without a processor version was accepted")
	}
}

// A price failure is about the coins it names, or every coin of its instant; a protocol that holds
// nothing has nothing it failed to price.
func TestSuiPriceFailureReachesOnlyProtocolsHoldingCoins(t *testing.T) {
	owner := suiEngineOwner(t)
	head := suiEngineCheckpoint(2000, suiEngineTestBase)
	navi := suiEnginePositions("navi", head, map[string]string{suiLongType: "1"})
	test := &suiEngineTest{t: t, node: &fakeSuiNode{head: head}, nodes: &fakeSuiNodeProtocols{positions: map[string]suiPositions{"navi": navi}}}
	response := test.engine().ScanWithOptions(context.Background(), owner, SuiScanOptions{ProtocolIDs: map[string]struct{}{"navi": {}, "cetus": {}}})
	if !suiHasError(response, "navi", "pricing", "Sui prices are not configured") {
		t.Fatalf("navi: %+v", response.Errors)
	}
	if got := suiErrorsOf(response, "cetus", "pricing"); len(got) != 0 {
		t.Fatalf("cetus holds nothing yet failed pricing: %v", got)
	}
}
