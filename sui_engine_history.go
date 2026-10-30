package portfolio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SuiRangeScan is ScanRange's answer for one instant.
type SuiRangeScan struct {
	Response *SuiResponse
	// Retry is set when Response is incomplete because a source did not answer — an index read
	// failed, wallet history had not reached the sample, the price service was unavailable — so
	// asking again later may complete it. An index that answered, even with a sample it could not
	// certify, is not a reason to retry. Retry wraps ErrSuiRangeUnread when the scan ran out of
	// time before reading the instant.
	Retry error
}

// ErrSuiRangeUnread is an instant a range scan ran out of time before reading.
var ErrSuiRangeUnread = errors.New("the range scan ran out of time before reading this instant")

// SuiRangePhaseObservation is one phase of a Sui range scan: "index_read" once per protocol index
// (ProtocolID set), then "align", "wallet_read" and "pricing".
type SuiRangePhaseObservation struct {
	Phase      string
	ProtocolID string
	Duration   time.Duration
	Err        error
}

// SuiRangePhaseObserver is an Observer that also wants the phases of Sui range scans. The engine
// reports them to its observer when the observer implements this interface.
type SuiRangePhaseObserver interface {
	ObserveSuiRangePhase(SuiRangePhaseObservation)
}

// ScanRange answers ScanWithOptions(SuiAtTime(instant)) for every protocol with a past, at every
// instant at once: each protocol's index is read once over the instants' span, each sample is
// priced once whatever number of instants it answers, and all of them are priced together. Scan i
// answers instants[i]; the instants need not be sorted.
//
// The wallet has to be read at the same state as the protocols, or an asset moving between them is
// counted twice or not at all, so every protocol of an instant is first answered at one checkpoint
// and the wallet is read there. All instants share one balance read, and wallet coins are priced at
// that checkpoint's timestamp beside the protocols' prices. Part of ctx's deadline is kept for
// pricing.
func (e *SuiEngine) ScanRange(ctx context.Context, owner SuiAddress, instants []time.Time) []SuiRangeScan {
	ctx = withScan(ctx, e.engine.observer, e.engine.indexerLane)
	routes := make([]suiRoute, 0, len(suiRoutes))
	for _, route := range suiRoutes {
		if route.pinned != suiUnread {
			routes = append(routes, route)
		}
	}
	plan := suiSamplePlan{instants: instants}
	if observer, ok := e.engine.observer.(SuiRangePhaseObserver); ok {
		plan.observe = observer.ObserveSuiRangePhase
	}
	return e.scanSamples(ctx, owner, routes, plan)
}

// suiSamplePlan is what a sampled scan answers: the instants of a range or of one pinned time, or
// one checkpoint.
type suiSamplePlan struct {
	instants []time.Time
	// checkpoint, when set, pins the scan's single instant to a checkpoint rather than a time.
	checkpoint *uint64
	observe    func(SuiRangePhaseObservation)
}

func (p suiSamplePlan) phase(name, protocolID string, startedAt time.Time, err error) {
	if p.observe != nil {
		p.observe(SuiRangePhaseObservation{Phase: name, ProtocolID: protocolID, Duration: time.Since(startedAt), Err: err})
	}
}

// suiProtocolRange is what one protocol's range read returned: its samples in checkpoint order,
// and the error that ended it early, if one did.
type suiProtocolRange struct {
	samples []suiHistorySample
	err     error
}

// suiAnsweredSample is one protocol's answer at one instant of a sampled scan.
type suiAnsweredSample struct {
	positions suiPositions
	err       error
}

// scanSamples reads routes from processor indexes and wallet history at each instant of plan,
// never from the node's state.
func (e *SuiEngine) scanSamples(ctx context.Context, owner SuiAddress, routes []suiRoute, plan suiSamplePlan) []SuiRangeScan {
	if len(plan.instants) == 0 {
		return nil
	}
	readCtx, cancelReads := withSuiPriceReserve(ctx)
	defer cancelReads()
	unread := func(err error) error {
		if err != nil && errors.Is(err, context.DeadlineExceeded) && readCtx.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%w: %w", ErrSuiRangeUnread, err)
		}
		return err
	}
	var read []suiRoute
	var wallet *suiRoute
	for _, route := range routes {
		switch {
		case route.pinned == suiFromIndex && e.indexes[route.id] != nil:
			read = append(read, route)
		case route.pinned == suiFromWalletHistory:
			wallet = &route
		}
	}
	answers := make([]map[string]suiAnsweredSample, len(plan.instants))
	retries := make([]error, len(plan.instants))
	for i := range answers {
		answers[i] = map[string]suiAnsweredSample{}
	}
	var ranges map[string]*suiProtocolRange
	if plan.checkpoint != nil {
		e.readCheckpoint(readCtx, owner, read, *plan.checkpoint, answers[0], unread, plan)
		for _, route := range read {
			if err := answers[0][route.id].err; err != nil && retries[0] == nil {
				retries[0] = fmt.Errorf("%s index: %w", route.id, err)
			}
		}
	} else {
		ranges = e.readRanges(readCtx, owner, read, plan, unread)
		for i, at := range plan.instants {
			for _, route := range read {
				sample, unanswered, err := ranges[route.id].answer(at)
				if unanswered && retries[i] == nil {
					retries[i] = fmt.Errorf("%s index: %w", route.id, err)
				}
				positions := suiPositions{ProtocolID: route.id, ProtocolName: route.name}
				if err == nil {
					positions, err = sample.At(at)
				}
				answers[i][route.id] = suiAnsweredSample{positions, err}
			}
		}
	}
	var wallets []suiWalletRead
	var walletErr error
	if wallet != nil {
		if walletErr = e.configured(*wallet, suiFromWalletHistory); walletErr == nil {
			aligned := time.Now()
			e.alignSamples(readCtx, owner, plan, read, ranges, answers, retries, unread)
			plan.phase("align", "", aligned, nil)
			pins := make([]SuiCheckpoint, len(plan.instants))
			unpinned := make([]error, len(plan.instants))
			for i := range plan.instants {
				pins[i], unpinned[i] = suiWalletPin(read, answers[i], plan, i)
			}
			walletRead := time.Now()
			wallets = e.readWalletHistory(ctx, readCtx, owner, pins, unpinned)
			plan.phase("wallet_read", "", walletRead, nil)
		}
	}
	// Wallet coins are priced apart from the protocols' so each keeps its own failures, and at the
	// same time, so neither waits on the other.
	wanted := map[time.Time][]string{}
	for i := range plan.instants {
		for _, route := range read {
			if answer := answers[i][route.id]; answer.err == nil {
				key := suiQuoteKey(answer.positions.Checkpoint.Timestamp)
				wanted[key] = mergeSortedStrings(wanted[key], suiPositionCoins(answer.positions))
			}
		}
	}
	priced := time.Now()
	var quotes, walletQuotes suiQuotes
	var pricing sync.WaitGroup
	pricing.Go(func() { quotes = e.quote(ctx, wanted) })
	pricing.Go(func() { walletQuotes = e.quote(ctx, suiWalletQuotes(wallets)) })
	pricing.Wait()
	plan.phase("pricing", "", priced, nil)
	out := make([]SuiRangeScan, len(plan.instants))
	for i := range plan.instants {
		response := e.newResponse(owner, routes)
		for _, route := range routes {
			result := suiProtocolResult{}
			var retry error
			switch {
			case route.pinned == suiFromWalletHistory && walletErr != nil:
				result.fail(route, "protocol", walletErr)
			case route.pinned == suiFromWalletHistory:
				retry = e.walletResult(&result, owner, route, wallets[i], walletQuotes)
			case route.pinned == suiUnread:
				result.fail(route, "protocol", errSuiHistoryUnsupported)
			case e.indexes[route.id] == nil:
				result.fail(route, "protocol", errSuiIndexUnconfigured)
			case answers[i][route.id].err != nil:
				result.fail(route, "protocol", answers[i][route.id].err)
			default:
				retry = e.sampleResult(&result, owner, route, answers[i][route.id].positions, plan, i, quotes)
			}
			if retry != nil && retries[i] == nil {
				retries[i] = retry
			}
			response.add(&result)
		}
		response.CompletedAt = e.now()
		out[i] = SuiRangeScan{Response: response, Retry: retries[i]}
	}
	return out
}

// readRanges reads each index once over the span of plan's instants, all at once.
func (e *SuiEngine) readRanges(readCtx context.Context, owner SuiAddress, read []suiRoute, plan suiSamplePlan, unread func(error) error) map[string]*suiProtocolRange {
	lo, hi := plan.instants[0], plan.instants[0]
	for _, at := range plan.instants[1:] {
		lo, hi = minTime(lo, at), maxTime(hi, at)
	}
	ranges := map[string]*suiProtocolRange{}
	var group sync.WaitGroup
	for _, route := range read {
		r := &suiProtocolRange{}
		ranges[route.id] = r
		index := e.indexes[route.id]
		group.Go(func() {
			started := time.Now()
			r.samples, r.err = index.ReadRange(withDeployment(readCtx, route.id, 0), owner, lo, hi)
			r.err = unread(r.err)
			plan.phase("index_read", route.id, started, r.err)
		})
	}
	group.Wait()
	return ranges
}

// readCheckpoint reads each index at checkpoint, all at once.
func (e *SuiEngine) readCheckpoint(readCtx context.Context, owner SuiAddress, read []suiRoute, checkpoint uint64, answers map[string]suiAnsweredSample, unread func(error) error, plan suiSamplePlan) {
	results := make([]suiAnsweredSample, len(read))
	var group sync.WaitGroup
	for i, route := range read {
		index := e.indexes[route.id]
		group.Go(func() {
			started := time.Now()
			positions, err := index.ReadAtCheckpoint(withDeployment(readCtx, route.id, 0), owner, checkpoint)
			results[i] = suiAnsweredSample{positions, unread(err)}
			plan.phase("index_read", route.id, started, results[i].err)
		})
	}
	group.Wait()
	for i, route := range read {
		answers[route.id] = results[i]
	}
}

// answer is the sample answering at: the newest covering it, as a single read selects. An instant
// no sample covers has none, unless the range failed before reaching it; only that failure is the
// index not answering.
func (r *suiProtocolRange) answer(at time.Time) (sample suiHistorySample, unanswered bool, err error) {
	if found, ok := suiSampleAt(r.samples, at); ok {
		return found, false, nil
	}
	if r.err != nil && (len(r.samples) == 0 || !at.Before(r.samples[len(r.samples)-1].Until)) {
		return suiHistorySample{}, true, r.err
	}
	return suiHistorySample{}, false, errors.New("Sui index has no completed sample for the requested hour")
}

// suiSampleAt is the newest of samples covering at.
func suiSampleAt(samples []suiHistorySample, at time.Time) (suiHistorySample, bool) {
	var found suiHistorySample
	ok := false
	for _, sample := range samples {
		if !at.Before(sample.Start) && at.Before(sample.Until) && (!ok || sample.Start.After(found.Start)) {
			found, ok = sample, true
		}
	}
	return found, ok
}

// sampleResult adds one protocol's sample at instant i to its response, priced at the sample's own
// timestamp, and returns why the instant should be asked again, if the price service did not
// answer.
func (e *SuiEngine) sampleResult(result *suiProtocolResult, owner SuiAddress, route suiRoute, positions suiPositions, plan suiSamplePlan, i int, quotes suiQuotes) error {
	var at *time.Time
	if plan.checkpoint == nil {
		at = &plan.instants[i]
	}
	if err := checkSuiSample(route, positions, plan.checkpoint, at); err != nil {
		result.fail(route, "protocol", err)
		return nil
	}
	sample := positions.Checkpoint
	var retry error
	if failure, ok := quotes.unavailable(sample.Timestamp); ok {
		retry = fmt.Errorf("%s prices at %s: %s", route.id, suiQuoteKey(sample.Timestamp).Format(time.RFC3339), failure)
	}
	coins := suiPositionCoins(positions)
	prices, failures := quotes.at(sample.Timestamp, coins)
	failures = append(failures, quotes.gaps(sample.Timestamp, coins, prices)...)
	snapshot, summary, err := suiValuedSnapshot(owner, positions, prices)
	if err != nil {
		result.fail(route, "protocol", err)
		return retry
	}
	for _, readErr := range positions.Errors {
		result.fail(route, "protocol", readErr)
	}
	for _, failure := range failures {
		result.fail(route, "pricing", failure)
	}
	if snapshot == nil {
		snapshot = &SuiSnapshot{ProtocolID: route.id, ProtocolName: route.name, Account: owner, Checkpoint: sample}
	}
	result.snapshot, result.summary, result.prices, result.checkpoint = snapshot, summary, prices, sample
	return retry
}

// suiWalletPin is the checkpoint the wallet is read at for instant i: the one checkpoint every
// protocol read answers at (alignSamples), so the wallet and the protocols describe one state and
// an asset moving between them is counted once. Without an answer from every protocol read, all at
// that checkpoint, there is no state to pin to, and the instant fails on that protocol anyway.
func suiWalletPin(read []suiRoute, answers map[string]suiAnsweredSample, plan suiSamplePlan, i int) (SuiCheckpoint, error) {
	var pin SuiCheckpoint
	if len(read) == 0 {
		return pin, errors.New("Sui wallet history is read at a protocol sample's checkpoint, and no protocol index was read")
	}
	var at *time.Time
	if plan.checkpoint == nil {
		at = &plan.instants[i]
	}
	for _, route := range read {
		answer := answers[route.id]
		sample := answer.positions.Checkpoint
		if answer.err != nil || checkSuiSample(route, answer.positions, plan.checkpoint, at) != nil {
			return SuiCheckpoint{}, fmt.Errorf("wallet balances were not read: %s has no sample to pin them to", route.name)
		}
		if pin.Sequence != 0 && sample.Sequence != pin.Sequence {
			return SuiCheckpoint{}, errors.New("wallet balances were not read: the protocol samples are at different checkpoints")
		}
		pin = sample
	}
	return pin, nil
}

// alignSamples answers every protocol of each instant at one checkpoint: the oldest of the
// checkpoints its samples are at. Indexes sample on fixed UTC boundaries, a coarser index's on a
// subset of a finer one's (a day's midnight is also an hour), and each lands on the first
// checkpoint at or after a boundary, so every index has a sample at exactly the oldest one: where
// one index is daily and another hourly for an instant, the instant is answered at the day's
// sample. A newer protocol's sample at that checkpoint may precede what the range read, and is
// then read. An instant whose protocols still cannot be answered at one checkpoint fails on the
// protocol that has no sample there, as an instant no sample covers does. An instant any protocol
// did not answer is left as it is: it fails on that protocol already.
func (e *SuiEngine) alignSamples(
	readCtx context.Context,
	owner SuiAddress,
	plan suiSamplePlan,
	read []suiRoute,
	ranges map[string]*suiProtocolRange,
	answers []map[string]suiAnsweredSample,
	retries []error,
	unread func(error) error,
) {
	type target struct {
		instant    int
		checkpoint SuiCheckpoint
	}
	targets := map[string][]target{}
	for i := range plan.instants {
		var oldest SuiCheckpoint
		for _, route := range read {
			answer := answers[i][route.id]
			if answer.err != nil || answer.positions.Checkpoint.Sequence == 0 {
				oldest = SuiCheckpoint{}
				break
			}
			if oldest.Sequence == 0 || answer.positions.Checkpoint.Sequence < oldest.Sequence {
				oldest = answer.positions.Checkpoint
			}
		}
		if oldest.Sequence == 0 {
			continue
		}
		for _, route := range read {
			if answers[i][route.id].positions.Checkpoint.Sequence != oldest.Sequence {
				targets[route.id] = append(targets[route.id], target{i, oldest})
			}
		}
	}
	// Read what the first reads did not reach: the samples at the targets' checkpoints.
	extra := map[string]*suiProtocolRange{}
	var reads sync.WaitGroup
	for _, route := range read {
		list := targets[route.id]
		if len(list) == 0 {
			continue
		}
		index := e.indexes[route.id]
		r := &suiProtocolRange{}
		if plan.checkpoint != nil {
			extra[route.id] = r
			sequence := list[0].checkpoint.Sequence
			reads.Go(func() {
				positions, err := index.ReadAtCheckpoint(withDeployment(readCtx, route.id, 0), owner, sequence)
				r.err = unread(err)
				if err == nil {
					// A checkpoint read answers the newest sample at or before it, whose interval
					// begins at its own checkpoint's timestamp.
					r.samples = []suiHistorySample{{Start: positions.Checkpoint.Timestamp, Until: positions.Checkpoint.Timestamp.Add(time.Nanosecond), Positions: positions}}
				}
			})
			continue
		}
		var lo, hi time.Time
		for _, t := range list {
			if _, ok := suiSampleAt(ranges[route.id].samples, t.checkpoint.Timestamp); ok {
				continue
			}
			if lo.IsZero() || t.checkpoint.Timestamp.Before(lo) {
				lo = t.checkpoint.Timestamp
			}
			hi = maxTime(hi, t.checkpoint.Timestamp)
		}
		if lo.IsZero() {
			continue
		}
		extra[route.id] = r
		reads.Go(func() {
			r.samples, r.err = index.ReadRange(withDeployment(readCtx, route.id, 0), owner, lo, hi)
			r.err = unread(r.err)
		})
	}
	reads.Wait()
	for _, route := range read {
		var samples []suiHistorySample
		if ranges != nil {
			samples = ranges[route.id].samples
		}
		if r := extra[route.id]; r != nil {
			samples = append(append([]suiHistorySample{}, r.samples...), samples...)
		}
		for _, t := range targets[route.id] {
			sample, ok := suiSampleAt(samples, t.checkpoint.Timestamp)
			instant := t.checkpoint.Timestamp
			if plan.checkpoint == nil {
				instant = plan.instants[t.instant]
			}
			switch {
			case !ok && extra[route.id] != nil && extra[route.id].err != nil:
				answers[t.instant][route.id] = suiAnsweredSample{positions: suiPositions{ProtocolID: route.id, ProtocolName: route.name}, err: extra[route.id].err}
				if retries[t.instant] == nil {
					retries[t.instant] = fmt.Errorf("%s index: %w", route.id, extra[route.id].err)
				}
			case ok && sample.Err != nil:
				// A sample the index could not certify fails the instant as it would at its own time.
				positions, err := sample.At(t.checkpoint.Timestamp)
				answers[t.instant][route.id] = suiAnsweredSample{positions, err}
			case !ok || sample.Positions.Checkpoint.Sequence != t.checkpoint.Sequence:
				answers[t.instant][route.id] = suiAnsweredSample{positions: suiPositions{ProtocolID: route.id, ProtocolName: route.name},
					err: fmt.Errorf("%s has no sample at checkpoint %d, where this instant's other protocol samples are, so the portfolio cannot be read at one state", route.name, t.checkpoint.Sequence)}
			case plan.checkpoint != nil:
				answers[t.instant][route.id] = suiAnsweredSample{positions: sample.Positions}
			default:
				positions, err := sample.At(t.checkpoint.Timestamp)
				// The answer is still the instant's, whichever sample it came from.
				for _, group := range positions.Groups {
					if group.Metadata != nil {
						group.Metadata["requestedTimestamp"] = instant.UTC().Format(time.RFC3339Nano)
					}
				}
				answers[t.instant][route.id] = suiAnsweredSample{positions, err}
			}
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
