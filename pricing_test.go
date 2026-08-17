package portfolio

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type recordingPriceProvider struct {
	tokens []Token
}

func (p *recordingPriceProvider) USDPrices(
	_ context.Context,
	tokens []Token,
) PriceResult {
	p.tokens = append([]Token(nil), tokens...)
	prices := make(map[AssetID]float64, len(tokens))
	for _, token := range tokens {
		prices[AssetForToken(token)] = 1
	}
	return PriceResult{Prices: prices}
}

func TestFetchPricesUsesTypedProviderBoundary(t *testing.T) {
	usdc := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xA0b86991c6218b36c1d19d4a2e9eb0cE3606eB48"),
		Symbol:   "USDC",
		Decimals: 6,
	}
	zero := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Symbol:   "ZERO",
		Decimals: 18,
	}
	snapshots := []Snapshot{{Groups: []Group{{Components: []Component{
		{Kind: "asset", Token: usdc, AmountRaw: "1"},
		{Kind: "asset", Token: usdc, AmountRaw: "2"},
		{Kind: "asset", Token: zero, AmountRaw: "0"},
	}}}}}
	provider := &recordingPriceProvider{}

	prices, priceErrors := fetchPrices(context.Background(), provider, snapshots)

	if len(priceErrors) != 0 {
		t.Fatalf("price errors = %+v", priceErrors)
	}
	if len(provider.tokens) != 1 || AssetForToken(provider.tokens[0]) != AssetForToken(usdc) {
		t.Fatalf("provider tokens = %+v, want one USDC", provider.tokens)
	}
	if prices[PriceKey(usdc)] != 1 || len(prices) != 1 {
		t.Fatalf("prices = %+v", prices)
	}
}

func TestFetchPricesReportsMissingProvider(t *testing.T) {
	snapshots := []Snapshot{{Groups: []Group{{Components: []Component{{
		Kind: "asset", Token: Token{ChainID: Ethereum, Address: common.HexToAddress("0x1")}, AmountRaw: "1",
	}}}}}}

	prices, priceErrors := fetchPrices(context.Background(), nil, snapshots)

	if len(prices) != 0 || len(priceErrors) != 1 || priceErrors[0].Message != "price provider is not configured" {
		t.Fatalf("prices=%+v errors=%+v", prices, priceErrors)
	}
}
