package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"time"
)

// SuiPrices quotes Sui coins in USD. It is a seam of its own rather than a PriceProvider because a
// Sui coin is a Move type, not a ChainID and an address.
type SuiPrices interface {
	// SuiUSDPrices answers every query: Prices[i] holds the quotes for queries[i], keyed by
	// normalized coin type. A query's At is the instant its coins are valued at; the zero At asks
	// for the latest quote. Answer each instant with that instant's quote and never another: a
	// current price on a past balance is a wrong number nobody can see is wrong. A coin absent from
	// its query's prices is unpriced, and a SuiPriceFailure may say why.
	SuiUSDPrices(ctx context.Context, queries []SuiPriceQuery) SuiPriceResult
	// AdmitSuiCoins returns the coin types the source can resolve. Wallet history drops the rest
	// before it reads their metadata or asks for their prices: a wallet is mostly unsolicited coins,
	// and each would cost a quote per sample only to come back unpriced.
	AdmitSuiCoins(coinTypes []string) []string
}

// SuiPriceQuery asks for the USD price of each coin at At, or the latest when At is zero.
type SuiPriceQuery struct {
	At        time.Time
	CoinTypes []string
}

// SuiPriceResult answers a SuiUSDPrices call; Prices[i] answers query i.
type SuiPriceResult struct {
	Prices   []map[string]float64
	Failures []SuiPriceFailure
}

// SuiPriceFailure is why a query, or one coin of it, was not priced.
type SuiPriceFailure struct {
	// Query indexes the query the failure is about.
	Query int
	// CoinType names the coin the failure is about; empty is every coin of the query left unpriced.
	CoinType string
	Message  string
	// Unavailable marks a failure of the price service itself — an outage or a timeout rather than
	// an answer such as a missing quote — so asking again later may price the coin.
	Unavailable bool
}

// suiQuotes is what the price source answered, keyed by valuation instant: the zero instant for
// latest quotes, otherwise the UTC timestamp of the sample a balance was read at.
type suiQuotes struct {
	prices   map[time.Time]map[string]float64
	failures map[time.Time][]SuiPriceFailure
}

// suiQuoteKey is the map key of a valuation instant: UTC, without a monotonic reading, so the same
// instant always finds the same entry.
func suiQuoteKey(at time.Time) time.Time { return at.UTC().Round(0) }

// quote asks the price source for every coin of wanted at its instant, in one call.
func (e *SuiEngine) quote(ctx context.Context, wanted map[time.Time][]string) suiQuotes {
	quotes := suiQuotes{prices: map[time.Time]map[string]float64{}, failures: map[time.Time][]SuiPriceFailure{}}
	keys := make([]time.Time, 0, len(wanted))
	merged := map[time.Time][]string{}
	for at, coins := range wanted {
		if len(coins) == 0 {
			continue
		}
		key := suiQuoteKey(at)
		if _, seen := merged[key]; !seen {
			keys = append(keys, key)
		}
		merged[key] = mergeSortedStrings(merged[key], coins)
	}
	if len(keys) == 0 {
		return quotes
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Before(keys[j]) })
	if e.prices == nil {
		for _, key := range keys {
			quotes.failures[key] = []SuiPriceFailure{{Message: "Sui prices are not configured"}}
		}
		return quotes
	}
	queries := make([]SuiPriceQuery, len(keys))
	for i, key := range keys {
		queries[i] = SuiPriceQuery{At: key, CoinTypes: merged[key]}
	}
	result := e.prices.SuiUSDPrices(ctx, queries)
	for i, key := range keys {
		if i < len(result.Prices) && result.Prices[i] != nil {
			quotes.prices[key] = result.Prices[i]
		} else {
			quotes.failures[key] = append(quotes.failures[key], SuiPriceFailure{Query: i, Message: "the price source did not answer", Unavailable: true})
		}
	}
	for _, failure := range result.Failures {
		if failure.Query >= 0 && failure.Query < len(keys) {
			key := keys[failure.Query]
			quotes.failures[key] = append(quotes.failures[key], failure)
		}
	}
	return quotes
}

// at returns the quotes for coins at instant, and the failures about any of them. A route that
// holds nothing is told of no failure.
func (q suiQuotes) at(instant time.Time, coins []string) (map[string]float64, []error) {
	key := suiQuoteKey(instant)
	prices := map[string]float64{}
	if len(coins) == 0 {
		return prices, nil
	}
	for _, coin := range coins {
		if price, ok := q.prices[key][coin]; ok {
			prices[coin] = price
		}
	}
	var failures []error
	for _, failure := range q.failures[key] {
		if failure.CoinType == "" || slices.Contains(coins, failure.CoinType) {
			failures = append(failures, errors.New(failure.Message))
		}
	}
	return prices, failures
}

// gaps reports each of coins left unpriced that no failure already accounts for.
func (q suiQuotes) gaps(instant time.Time, coins []string, prices map[string]float64) []error {
	key := suiQuoteKey(instant)
	explained := map[string]bool{}
	for _, failure := range q.failures[key] {
		if failure.CoinType == "" {
			return nil
		}
		explained[failure.CoinType] = true
	}
	var gaps []error
	for _, coin := range coins {
		if _, priced := prices[coin]; priced || explained[coin] {
			continue
		}
		if key.IsZero() {
			gaps = append(gaps, fmt.Errorf("no admissible USD price for %s", coin))
		} else {
			gaps = append(gaps, fmt.Errorf("no admissible USD price at the sample for %s", coin))
		}
	}
	return gaps
}

// unavailable returns the first failure of the price service itself at instant: the reason asking
// again later may price what is unpriced.
func (q suiQuotes) unavailable(instant time.Time) (string, bool) {
	for _, failure := range q.failures[suiQuoteKey(instant)] {
		if failure.Unavailable {
			return failure.Message, true
		}
	}
	return "", false
}

// suiValuedSnapshot values positions at prices (normalized coin type to USD) and returns the
// snapshot and its summary, or nil for both when nothing is held. A group's value nets its priced
// components, debts subtracted; an unpriced component counts toward the summary's components and
// toward no value.
func suiValuedSnapshot(owner SuiAddress, positions suiPositions, prices map[string]float64) (*SuiSnapshot, *ProtocolSummary, error) {
	if len(positions.Groups) == 0 {
		return nil, nil, nil
	}
	snapshot := &SuiSnapshot{
		ProtocolID:   positions.ProtocolID,
		ProtocolName: positions.ProtocolName,
		Account:      owner,
		Checkpoint:   positions.Checkpoint,
		Groups:       make([]SuiGroup, 0, len(positions.Groups)),
	}
	summary := &ProtocolSummary{ProtocolID: positions.ProtocolID, ProtocolName: positions.ProtocolName}
	for _, source := range positions.Groups {
		group := source
		group.ValueUSD = nil
		group.Components = make([]SuiComponent, 0, len(source.Components))
		var total float64
		priced := false
		for _, component := range source.Components {
			amount, ok := new(big.Int).SetString(component.AmountRaw, 10)
			if !ok || amount.Sign() < 0 {
				return nil, nil, errors.New("invalid Sui component quantity")
			}
			if component.AmountDenominatorRaw != "" {
				return nil, nil, errors.New("unsupported Sui quantity denominator")
			}
			summary.ComponentCount++
			component.PriceUSD, component.ValueUSD = nil, nil
			if price, ok := prices[component.Coin.CoinType]; ok {
				value := suiComponentValue(amount, component.Coin.Decimals, price)
				component.PriceUSD, component.ValueUSD = &price, &value
				switch component.Kind {
				case "debt":
					summary.DebtUSD += value
					total -= value
				case "reward":
					summary.RewardUSD += value
					total += value
				default:
					summary.AssetUSD += value
					total += value
				}
				summary.PricedComponents++
				priced = true
			}
			group.Components = append(group.Components, component)
		}
		if priced {
			group.ValueUSD = &total
			summary.TotalUSD += total
		}
		snapshot.Groups = append(snapshot.Groups, group)
	}
	if summary.PricedComponents > 0 {
		total := summary.TotalUSD
		snapshot.ValueUSD = &total
	}
	return snapshot, summary, nil
}

// suiComponentValue is amount / 10^decimals * price, computed in big.Float so a large balance of a
// many-decimal coin does not lose precision before the final conversion.
func suiComponentValue(amount *big.Int, decimals uint8, price float64) float64 {
	scale := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil))
	quantity := new(big.Float).Quo(new(big.Float).SetInt(amount), scale)
	value, _ := new(big.Float).Mul(quantity, big.NewFloat(price)).Float64()
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return 0
	}
	return value
}

// mergeSortedStrings is the sorted union of a and b, without duplicates.
func mergeSortedStrings(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	sort.Strings(out)
	return slices.Compact(out)
}
