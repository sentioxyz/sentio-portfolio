package portfolio

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

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
type PriceProvider interface {
	USDPrices(context.Context, []Token) PriceResult
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
	Ethereum: "ethereum",
	BSC:      "bsc",
	Base:     "base",
	Arbitrum: "arbitrum",
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

func collectPriceTokens(snapshots []Snapshot) []Token {
	unique := make(map[AssetID]Token)
	for _, snapshot := range snapshots {
		for _, group := range snapshot.Groups {
			for _, component := range group.Components {
				if component.AmountRaw == "0" {
					continue
				}
				asset := AssetForToken(component.Token)
				if _, exists := unique[asset]; !exists {
					unique[asset] = component.Token
				}
			}
		}
	}
	tokens := make([]Token, 0, len(unique))
	for _, token := range unique {
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(left, right int) bool {
		return PriceKey(tokens[left]) < PriceKey(tokens[right])
	})
	return tokens
}

func fetchPrices(
	ctx context.Context,
	provider PriceProvider,
	snapshots []Snapshot,
) (map[string]float64, []ScanError) {
	tokens := collectPriceTokens(snapshots)
	prices := make(map[string]float64, len(tokens))
	if len(tokens) == 0 {
		return prices, nil
	}
	if provider == nil {
		return prices, []ScanError{{
			Scope: "pricing", Message: "price provider is not configured",
		}}
	}
	result := provider.USDPrices(ctx, tokens)
	for _, token := range tokens {
		if price, exists := result.Prices[AssetForToken(token)]; exists {
			prices[PriceKey(token)] = price
		}
	}
	priceErrors := make([]ScanError, 0, len(result.Failures))
	for _, failure := range result.Failures {
		scanError := ScanError{
			Scope: "pricing", Message: PublicError(errors.New(failure.Message)),
		}
		if failure.Asset != nil {
			scanError.ChainID = failure.Asset.ChainID
		}
		priceErrors = append(priceErrors, scanError)
	}
	return prices, priceErrors
}
