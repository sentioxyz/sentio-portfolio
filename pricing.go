package portfolio

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// AssetID is the stable, strongly typed identity shared with price providers.
// Symbol and decimals are intentionally excluded because neither identifies an
// asset and both can differ between data sources.
type AssetID struct {
	ChainID ChainID
	Address common.Address
}

// PriceProvider is the only valuation dependency of the calculation kernel.
// Implementations may use CoinQuote or another service without coupling
// protocol adapters to that transport.
//
// A live scan is valued through USDPrices. A scan pinned to fixed blocks is
// valued through USDPricesAt with each pinned block's timestamp, so historical
// quantities are never multiplied by today's prices. A provider without
// historical data must report those tokens as failures rather than answer with
// the latest quote: an unpriced component is a gap the response shows, while a
// current price on a past balance is a wrong number nobody can see is wrong.
type PriceProvider interface {
	USDPrices(context.Context, []Token) PriceResult
	USDPricesAt(context.Context, []Token, time.Time) PriceResult
}

type PriceFailure struct {
	Asset   *AssetID
	Message string
}

type PriceResult struct {
	Prices   map[AssetID]float64
	Failures []PriceFailure
}

var chainPriceNames = map[ChainID]string{
	Ethereum:  "ethereum",
	BSC:       "bsc",
	Base:      "base",
	Arbitrum:  "arbitrum",
	Polygon:   "polygon",
	Monad:     "monad",
	Plasma:    "plasma",
	Avalanche: "avalanche",
	Optimism:  "optimism",
}

func AssetForToken(token Token) AssetID {
	return AssetID{ChainID: token.ChainID, Address: token.Address}
}

// PriceKey is the stable key used by Response.Prices.
func PriceKey(token Token) string {
	return fmt.Sprintf(
		"%s:%s",
		chainPriceNames[token.ChainID],
		strings.ToLower(token.Address.Hex()),
	)
}

// priceQuery is one provider call: the tokens valued at one instant. A zero At
// asks for the latest quote.
type priceQuery struct {
	At     time.Time
	Tokens []Token
}

// valuationInstant is the moment a snapshot's holdings are worth pricing at. A
// fixed block is valued at its own timestamp; the live settled head is a few
// blocks old at most, so it is valued at the present.
func valuationInstant(block BlockRef) time.Time {
	if !block.Fixed {
		return time.Time{}
	}
	return time.Unix(int64(block.Timestamp), 0).UTC()
}

// collectPriceQueries groups the tokens a scan must quote by the instant they
// are valued at. Every chain in a scan is pinned to one block, so an asset maps
// to exactly one instant. Queries are ordered latest first, then by ascending
// timestamp, and tokens within a query by PriceKey, so provider batches are
// deterministic.
func collectPriceQueries(snapshots []Snapshot) []priceQuery {
	seen := make(map[AssetID]struct{})
	byInstant := make(map[time.Time][]Token)
	for _, snapshot := range snapshots {
		at := valuationInstant(snapshot.Block)
		for _, group := range snapshot.Groups {
			for _, component := range group.Components {
				if component.AmountRaw == "0" {
					continue
				}
				// A component with a price basis is quoted through that token, so asking the
				// provider for its own would only add a guaranteed failure to every scan.
				token := component.priceToken()
				asset := AssetForToken(token)
				if _, exists := seen[asset]; exists {
					continue
				}
				seen[asset] = struct{}{}
				byInstant[at] = append(byInstant[at], token)
			}
		}
	}
	queries := make([]priceQuery, 0, len(byInstant))
	for at, tokens := range byInstant {
		sort.Slice(tokens, func(left, right int) bool {
			return PriceKey(tokens[left]) < PriceKey(tokens[right])
		})
		queries = append(queries, priceQuery{At: at, Tokens: tokens})
	}
	sort.Slice(queries, func(left, right int) bool {
		return queries[left].At.Before(queries[right].At)
	})
	return queries
}

func fetchPrices(
	ctx context.Context,
	provider PriceProvider,
	snapshots []Snapshot,
) (map[string]float64, []ScanError) {
	queries := collectPriceQueries(snapshots)
	prices := make(map[string]float64)
	if len(queries) == 0 {
		return prices, nil
	}
	if provider == nil {
		return prices, []ScanError{{
			Scope: "pricing", Message: "price provider is not configured",
		}}
	}
	priceErrors := make([]ScanError, 0)
	for _, query := range queries {
		var result PriceResult
		if query.At.IsZero() {
			result = provider.USDPrices(ctx, query.Tokens)
		} else {
			result = provider.USDPricesAt(ctx, query.Tokens, query.At)
		}
		for _, token := range query.Tokens {
			if price, exists := result.Prices[AssetForToken(token)]; exists {
				prices[PriceKey(token)] = price
			}
		}
		for _, failure := range result.Failures {
			scanError := ScanError{
				Scope: "pricing", Message: PublicError(errors.New(failure.Message)),
			}
			if failure.Asset != nil {
				scanError.ChainID = failure.Asset.ChainID
			}
			priceErrors = append(priceErrors, scanError)
		}
	}
	return prices, priceErrors
}
