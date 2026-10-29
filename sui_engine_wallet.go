package portfolio

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// SuiWalletHistory reads what an account held at past checkpoints. The kernel speaks no Sui
// transport that can, so the host supplies one over whatever balance history it keeps.
type SuiWalletHistory interface {
	// BalancesAt returns, for each checkpoint in pins (sorted, distinct and nonzero), every coin
	// type owner held a nonzero balance of once that checkpoint was executed, with coin types
	// normalized and in coin-type order. Unless the source holds every change through the newest
	// pin, it must fail rather than answer with an earlier state under the pin's name, and the
	// failure should wrap ErrSuiWalletNotIngested. A failure wrapping ErrSuiWalletNotCovered is an
	// answer — the account is beyond what the source covers — and is not asked again; any other
	// failure is a source that did not answer, and is.
	BalancesAt(ctx context.Context, owner SuiAddress, pins []uint64) (map[uint64][]SuiCoinBalance, error)
}

var (
	// ErrSuiWalletNotIngested is a pin the wallet history source has not ingested every change
	// through yet.
	ErrSuiWalletNotIngested = errors.New("Sui balance history has not reached the sample checkpoint yet")
	// ErrSuiWalletNotCovered is an account the wallet history source does not cover.
	ErrSuiWalletNotCovered = errors.New("the account is beyond what Sui wallet history covers")
)

const (
	// suiWalletGroupPrefix mirrors the EVM wallet's group identifiers ("wallet:native",
	// "wallet:token:<address>"), so one coin type is one group on every chain.
	suiWalletGroupPrefix = "wallet:coin:"
	// suiOmittedCoinsNamed is how many omitted coin types an error message lists before it
	// summarises the rest as a count.
	suiOmittedCoinsNamed = 5
	// suiWalletMaxValuedCoins bounds the coin types one range scan values after admission: each
	// costs a quote per sample time.
	suiWalletMaxValuedCoins = 500
	// suiWalletMetadataCacheSize bounds the coin metadata an engine remembers between scans.
	suiWalletMetadataCacheSize = 20_000
)

// readWalletLatest reads the account's coins at the head, with each coin's on-chain metadata.
// Coins without usable metadata are omitted and reported, because a balance without decimals
// cannot be shown as an amount.
func (e *SuiEngine) readWalletLatest(ctx context.Context, owner SuiAddress, reader SuiReader) (suiPositions, error) {
	holdings, err := reader.Holdings(ctx, owner, nil)
	if err != nil {
		return suiPositions{}, fmt.Errorf("failed to read Sui holdings: %w", err)
	}
	coins := make([]string, 0, len(holdings.Balances))
	for _, balance := range holdings.Balances {
		coins = append(coins, balance.CoinType)
	}
	metadata, unusable, err := e.coinMetadata(ctx, reader, coins)
	if err != nil {
		return suiPositions{}, fmt.Errorf("failed to read Sui coin metadata: %w", err)
	}
	positions := suiWalletPositions(holdings.Checkpoint, holdings.Balances, metadata, map[string]any{
		"holding":        "coin",
		"headBeforeRead": holdings.HeadBeforeRead,
	})
	positions.HeadBeforeRead = holdings.HeadBeforeRead
	if len(unusable) > 0 {
		positions.Errors = append(positions.Errors, suiOmittedCoins(unusable))
	}
	return positions, nil
}

// suiWalletPositions is one group per coin of balances that has metadata, in coin-type order, each
// carrying a copy of groupMetadata.
func suiWalletPositions(checkpoint SuiCheckpoint, balances []SuiCoinBalance, metadata map[string]SuiCoinMetadata, groupMetadata map[string]any) suiPositions {
	positions := suiPositions{ProtocolID: "wallet", ProtocolName: "Wallet", Checkpoint: checkpoint}
	for _, balance := range balances {
		coin, ok := metadata[balance.CoinType]
		if !ok {
			continue
		}
		positions.Groups = append(positions.Groups, SuiGroup{
			ID:         suiWalletGroupPrefix + balance.CoinType,
			Label:      coin.Symbol,
			Components: []SuiComponent{{Kind: "asset", Coin: coin, AmountRaw: balance.Amount.String()}},
			Metadata:   maps.Clone(groupMetadata),
		})
	}
	return positions
}

// suiOmittedCoins reports the coins left out for want of usable metadata.
func suiOmittedCoins(unusable map[string]error) error {
	coinTypes := slices.Sorted(maps.Keys(unusable))
	named := coinTypes
	if len(named) > suiOmittedCoinsNamed {
		named = named[:suiOmittedCoinsNamed]
	}
	message := fmt.Sprintf("%d coin type(s) without usable on-chain metadata were omitted: %s", len(coinTypes), strings.Join(named, ", "))
	if len(coinTypes) > len(named) {
		message += fmt.Sprintf(" and %d more", len(coinTypes)-len(named))
	}
	return errors.New(message)
}

// suiWalletRead is the wallet's answer at one instant of a sampled scan: the coins it held at the
// sample checkpoint the answer is pinned to, or why it was not read. retry is set when a source did
// not answer, so asking later may.
type suiWalletRead struct {
	pin      SuiCheckpoint
	balances []SuiCoinBalance
	metadata map[string]SuiCoinMetadata
	notes    []string
	err      error
	retry    error
}

// readWalletHistory reads the wallet at each instant's pin with one balance read for all of them,
// keeps the coins the price source can resolve, and looks up their metadata. unpinned[i] is why
// instant i has no pin. Reads use readCtx, which ends before ctx does so what was read can still be
// priced; running out of it marks the instants unread rather than failed.
func (e *SuiEngine) readWalletHistory(ctx, readCtx context.Context, owner SuiAddress, pins []SuiCheckpoint, unpinned []error) []suiWalletRead {
	reads := make([]suiWalletRead, len(pins))
	sequences := []uint64{}
	for i := range pins {
		if unpinned[i] != nil {
			reads[i].err = unpinned[i]
			continue
		}
		reads[i].pin = pins[i]
		sequences = append(sequences, pins[i].Sequence)
	}
	if len(sequences) == 0 {
		return reads
	}
	sequences = slices.Compact(slices.Sorted(slices.Values(sequences)))
	fail := func(what string, err error, answered bool) []suiWalletRead {
		err = fmt.Errorf("%s: %w", what, err)
		var retry error
		switch {
		case errors.Is(err, context.DeadlineExceeded) && readCtx.Err() != nil && ctx.Err() == nil:
			err = fmt.Errorf("%w: %w", ErrSuiRangeUnread, err)
			retry = err
		case !answered:
			retry = err
		}
		for i := range reads {
			if unpinned[i] == nil {
				reads[i].err, reads[i].retry = err, retry
			}
		}
		return reads
	}
	held, err := e.wallet.BalancesAt(readCtx, owner, sequences)
	if err != nil {
		return fail("wallet balances", err, errors.Is(err, ErrSuiWalletNotCovered))
	}
	coins := []string{}
	for _, balances := range held {
		for _, balance := range balances {
			coins = append(coins, balance.CoinType)
		}
	}
	coins = mergeSortedStrings(nil, coins)
	admitted := coins
	if e.prices != nil {
		admitted = e.prices.AdmitSuiCoins(coins)
	}
	if len(admitted) > suiWalletMaxValuedCoins {
		return fail("wallet coins", fmt.Errorf("the account holds %d coin types the price source can resolve, more than the %d wallet history values", len(admitted), suiWalletMaxValuedCoins), true)
	}
	metadata, unusable, err := e.walletCoinMetadata(readCtx, admitted)
	if err != nil {
		return fail("wallet coin metadata", err, false)
	}
	valued := map[string]bool{}
	for _, coin := range admitted {
		valued[coin] = true
	}
	for i := range reads {
		if unpinned[i] != nil {
			continue
		}
		skipped, omitted := 0, map[string]error{}
		for _, balance := range held[pins[i].Sequence] {
			if !valued[balance.CoinType] {
				skipped++
				continue
			}
			if _, ok := metadata[balance.CoinType]; ok {
				reads[i].balances = append(reads[i].balances, balance)
				continue
			}
			reason := unusable[balance.CoinType]
			if reason == nil {
				reason = errors.New("no metadata was returned")
			}
			omitted[balance.CoinType] = reason
		}
		reads[i].metadata = metadata
		if skipped > 0 {
			reads[i].notes = append(reads[i].notes, fmt.Sprintf("%d coin type(s) the price source cannot resolve were omitted", skipped))
		}
		if len(omitted) > 0 {
			reads[i].notes = append(reads[i].notes, suiOmittedCoins(omitted).Error())
		}
	}
	return reads
}

// suiMetadataCache remembers usable coin metadata, up to suiWalletMetadataCacheSize coins. A coin
// whose metadata was unusable is asked again: the chain may yet publish it.
type suiMetadataCache struct {
	mu      sync.Mutex
	entries map[string]SuiCoinMetadata
}

// walletCoinMetadata is coinMetadata through the node, dialled only when the cache lacks a coin.
func (e *SuiEngine) walletCoinMetadata(ctx context.Context, coins []string) (map[string]SuiCoinMetadata, map[string]error, error) {
	return e.coinMetadata(ctx, nil, coins)
}

// coinMetadata is CoinMetadata through reader, or through the node when reader is nil,
// remembering usable metadata between scans.
func (e *SuiEngine) coinMetadata(ctx context.Context, reader SuiReader, coins []string) (map[string]SuiCoinMetadata, map[string]error, error) {
	metadata := map[string]SuiCoinMetadata{}
	missing := []string{}
	e.metadata.mu.Lock()
	for _, coin := range coins {
		if entry, ok := e.metadata.entries[coin]; ok {
			metadata[coin] = entry
		} else {
			missing = append(missing, coin)
		}
	}
	e.metadata.mu.Unlock()
	if len(missing) == 0 {
		return metadata, map[string]error{}, nil
	}
	if reader == nil {
		var err error
		if reader, err = e.node.get(ctx); err != nil {
			return nil, nil, fmt.Errorf("Sui node is unavailable: %w", err)
		}
	}
	read, unusable, err := reader.CoinMetadata(ctx, missing)
	if err != nil {
		return nil, nil, err
	}
	e.metadata.mu.Lock()
	if e.metadata.entries == nil || len(e.metadata.entries)+len(read) > suiWalletMetadataCacheSize {
		e.metadata.entries = make(map[string]SuiCoinMetadata, len(read))
	}
	for coin, entry := range read {
		e.metadata.entries[coin] = entry
		metadata[coin] = entry
	}
	e.metadata.mu.Unlock()
	return metadata, unusable, nil
}

// walletResult adds one instant's wallet to its response, valued at its pin's timestamp, and
// returns why the instant should be asked again, if a source did not answer.
func (e *SuiEngine) walletResult(result *suiProtocolResult, owner SuiAddress, route suiRoute, read suiWalletRead, quotes suiQuotes, skipPrices bool) error {
	if read.err != nil {
		result.fail(route, "protocol", read.err)
		return read.retry
	}
	var retry error
	var prices map[string]float64
	var failures []error
	if !skipPrices {
		if failure, ok := quotes.unavailable(read.pin.Timestamp); ok {
			retry = fmt.Errorf("wallet prices at %s: %s", suiQuoteKey(read.pin.Timestamp).Format(time.RFC3339), failure)
		}
		coins := make([]string, len(read.balances))
		for i, balance := range read.balances {
			coins[i] = balance.CoinType
		}
		prices, failures = quotes.at(read.pin.Timestamp, coins)
	}
	positions := suiWalletPositions(read.pin, read.balances, read.metadata, map[string]any{"holding": "coin"})
	snapshot, summary, err := suiValuedSnapshot(owner, positions, prices)
	if err != nil {
		result.fail(route, "protocol", err)
		return retry
	}
	for _, note := range read.notes {
		result.fail(route, "coverage", errors.New(note))
	}
	for i, failure := range failures {
		if i == suiOmittedCoinsNamed {
			result.fail(route, "pricing", fmt.Errorf("%d more wallet price failure(s) at the sample", len(failures)-i))
			break
		}
		result.fail(route, "pricing", failure)
	}
	// An empty wallet is still a wallet read at the sample, as an indexed protocol's empty sample is.
	if snapshot == nil {
		snapshot = &SuiSnapshot{ProtocolID: route.id, ProtocolName: route.name, Account: owner, Checkpoint: read.pin}
	}
	result.snapshot, result.summary, result.prices, result.checkpoint = snapshot, summary, prices, read.pin
	return retry
}

// suiWalletQuotes are the coin types to value at each pin's timestamp.
func suiWalletQuotes(reads []suiWalletRead) map[time.Time][]string {
	wanted := map[time.Time][]string{}
	for _, read := range reads {
		if read.err != nil || len(read.balances) == 0 {
			continue
		}
		coins := make([]string, len(read.balances))
		for i, balance := range read.balances {
			coins[i] = balance.CoinType
		}
		key := suiQuoteKey(read.pin.Timestamp)
		wanted[key] = mergeSortedStrings(wanted[key], coins)
	}
	return wanted
}
