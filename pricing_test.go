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

// A component with a price basis must never be quoted by its own token. Asking a provider for a
// PT is a guaranteed failure, and one that lands in the response as a pricing error on every
// scan of every account holding one.
func TestFetchPricesAsksForTheBasisTokenInsteadOfTheComponentToken(t *testing.T) {
	asset := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xdC035D45d973E3EC169d2276DDab16f1e407384F"),
		Symbol:   "USDS",
		Decimals: 18,
	}
	principal := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xdc169abe56461a2e0c034da431ac2a3ebf596094"),
		Symbol:   "PT-sUSDS-26NOV2026",
		Decimals: 18,
	}
	snapshots := []Snapshot{{Groups: []Group{{Components: []Component{{
		Kind:       "asset",
		Token:      principal,
		AmountRaw:  "1000000000000000000",
		PriceBasis: &PriceBasis{Token: asset, RatioRaw: "989030473175155466"},
	}}}}}}
	provider := &recordingPriceProvider{}

	prices, priceErrors := fetchPrices(context.Background(), provider, snapshots)

	if len(priceErrors) != 0 {
		t.Fatalf("price errors = %+v", priceErrors)
	}
	if len(provider.tokens) != 1 || AssetForToken(provider.tokens[0]) != AssetForToken(asset) {
		t.Fatalf("provider tokens = %+v, want only the basis asset", provider.tokens)
	}
	if _, quoted := prices[PriceKey(principal)]; quoted {
		t.Fatal("the unquotable component token must not appear in the price map")
	}
}

// The derived price belongs to the component's own token, not to the basis: a caller reading
// PriceUSD is reading what one PT is worth, and the value follows from that alone.
func TestComponentPriceDerivesTheComponentTokenPrice(t *testing.T) {
	asset := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xdC035D45d973E3EC169d2276DDab16f1e407384F"),
		Symbol:   "USDS",
		Decimals: 18,
	}
	component := Component{
		Kind:       "asset",
		Token:      Token{ChainID: Ethereum, Symbol: "PT", Decimals: 18},
		AmountRaw:  "2000000000000000000",
		PriceBasis: &PriceBasis{Token: asset, RatioRaw: "500000000000000000"},
	}
	prices := map[string]float64{PriceKey(asset): 3}

	price, priced, err := componentPrice(component, prices)
	if err != nil || !priced {
		t.Fatalf("componentPrice = (%v, %v, %v)", price, priced, err)
	}
	if price != 1.5 {
		t.Fatalf("derived price = %v, want 1.5", price)
	}
	value, err := componentValueUSD(component, price)
	if err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatalf("value = %v, want 3", value)
	}
}

// An unquoted basis leaves the component unpriced rather than valuing it at the ratio alone.
func TestComponentPriceWithoutABasisQuoteStaysUnpriced(t *testing.T) {
	component := Component{
		Kind:      "asset",
		Token:     Token{ChainID: Ethereum, Symbol: "PT", Decimals: 18},
		AmountRaw: "1",
		PriceBasis: &PriceBasis{
			Token:    Token{ChainID: Ethereum, Symbol: "USDS", Decimals: 18},
			RatioRaw: "989030473175155466",
		},
	}

	price, priced, err := componentPrice(component, map[string]float64{})
	if err != nil {
		t.Fatal(err)
	}
	if priced || price != 0 {
		t.Fatalf("componentPrice = (%v, %v), want unpriced", price, priced)
	}
}

// A malformed ratio is a bug in the adapter that produced it, so it fails the scan rather than
// silently valuing the component as if no basis had been set.
func TestComponentPriceRejectsAnUnusableRatio(t *testing.T) {
	asset := Token{ChainID: Ethereum, Symbol: "USDS", Decimals: 18}
	for _, ratio := range []string{"", "0", "-1", "not-a-number"} {
		component := Component{
			Kind:       "asset",
			Token:      Token{ChainID: Ethereum, Symbol: "PT", Decimals: 18},
			AmountRaw:  "1",
			PriceBasis: &PriceBasis{Token: asset, RatioRaw: ratio},
		}
		if _, _, err := componentPrice(
			component, map[string]float64{PriceKey(asset): 1},
		); err == nil {
			t.Fatalf("ratio %q was accepted", ratio)
		}
	}
}
