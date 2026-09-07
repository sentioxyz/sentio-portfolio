package portfolio

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// recordingPriceProvider quotes every token at 1 and records what it was asked: tokens holds the
// latest-price requests, historical holds each fixed-instant request in call order.
type recordingPriceProvider struct {
	tokens     []Token
	historical []priceQuery
}

func (p *recordingPriceProvider) USDPricesAt(
	_ context.Context,
	tokens []Token,
	at time.Time,
) PriceResult {
	p.historical = append(p.historical, priceQuery{At: at, Tokens: append([]Token(nil), tokens...)})
	prices := make(map[AssetID]float64, len(tokens))
	for _, token := range tokens {
		prices[AssetForToken(token)] = 1
	}
	return PriceResult{Prices: prices}
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

// A fixed-block snapshot holds what the account had then, so it must be valued at what things
// cost then. Each pinned block's timestamp becomes one historical query; live snapshots keep
// the latest quote; the same asset is never asked twice.
func TestFetchPricesValuesFixedBlockSnapshotsAtTheBlockTimestamp(t *testing.T) {
	usdc := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xA0b86991c6218b36c1d19d4a2e9eb0cE3606eB48"),
		Symbol:   "USDC",
		Decimals: 6,
	}
	cake := Token{
		ChainID:  BSC,
		Address:  common.HexToAddress("0x0E09FaBB73Bd3Ade0a17ECC321fD13a19e81cE82"),
		Symbol:   "CAKE",
		Decimals: 18,
	}
	weth := Token{
		ChainID:  Base,
		Address:  common.HexToAddress("0x4200000000000000000000000000000000000006"),
		Symbol:   "WETH",
		Decimals: 18,
	}
	const ethereumTimestamp, bscTimestamp = uint64(1_700_000_000), uint64(1_700_000_007)
	snapshots := []Snapshot{
		{
			ChainID: Ethereum,
			Block:   BlockRef{ChainID: Ethereum, Number: 900, Timestamp: ethereumTimestamp, Fixed: true},
			Groups: []Group{{Components: []Component{
				{Kind: "asset", Token: usdc, AmountRaw: "1"},
				{Kind: "asset", Token: usdc, AmountRaw: "2"},
			}}},
		},
		{
			ChainID: BSC,
			Block:   BlockRef{ChainID: BSC, Number: 100, Timestamp: bscTimestamp, Fixed: true},
			Groups:  []Group{{Components: []Component{{Kind: "asset", Token: cake, AmountRaw: "3"}}}},
		},
		{
			ChainID: Base,
			Block:   BlockRef{ChainID: Base, Number: 500, Timestamp: 1_700_000_100},
			Groups:  []Group{{Components: []Component{{Kind: "asset", Token: weth, AmountRaw: "4"}}}},
		},
	}
	provider := &recordingPriceProvider{}

	prices, priceErrors := fetchPrices(context.Background(), provider, snapshots)

	if len(priceErrors) != 0 {
		t.Fatalf("price errors = %+v", priceErrors)
	}
	if len(provider.tokens) != 1 || AssetForToken(provider.tokens[0]) != AssetForToken(weth) {
		t.Fatalf("latest tokens = %+v, want only the live-block WETH", provider.tokens)
	}
	if len(provider.historical) != 2 {
		t.Fatalf("historical queries = %+v, want one per pinned block", provider.historical)
	}
	first, second := provider.historical[0], provider.historical[1]
	if !first.At.Equal(time.Unix(int64(ethereumTimestamp), 0)) ||
		len(first.Tokens) != 1 || AssetForToken(first.Tokens[0]) != AssetForToken(usdc) {
		t.Fatalf("first historical query = %+v, want USDC at %d", first, ethereumTimestamp)
	}
	if !second.At.Equal(time.Unix(int64(bscTimestamp), 0)) ||
		len(second.Tokens) != 1 || AssetForToken(second.Tokens[0]) != AssetForToken(cake) {
		t.Fatalf("second historical query = %+v, want CAKE at %d", second, bscTimestamp)
	}
	for _, token := range []Token{usdc, cake, weth} {
		if prices[PriceKey(token)] != 1 {
			t.Fatalf("prices = %+v, want %s priced", prices, token.Symbol)
		}
	}
}

// Historical failures reach the response like any other pricing failure, attributed to the chain.
func TestFetchPricesReportsHistoricalFailures(t *testing.T) {
	usdc := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xA0b86991c6218b36c1d19d4a2e9eb0cE3606eB48"),
		Symbol:   "USDC",
		Decimals: 6,
	}
	snapshots := []Snapshot{{
		ChainID: Ethereum,
		Block:   BlockRef{ChainID: Ethereum, Number: 900, Timestamp: 1_700_000_000, Fixed: true},
		Groups:  []Group{{Components: []Component{{Kind: "asset", Token: usdc, AmountRaw: "1"}}}},
	}}
	asset := AssetForToken(usdc)
	provider := &failingHistoricalPriceProvider{failure: PriceFailure{
		Asset: &asset, Message: "PRICE_GAP: no sample within the allowed gap",
	}}

	prices, priceErrors := fetchPrices(context.Background(), provider, snapshots)

	if len(prices) != 0 {
		t.Fatalf("prices = %+v, want none", prices)
	}
	if len(priceErrors) != 1 || priceErrors[0].Scope != "pricing" || priceErrors[0].ChainID != Ethereum ||
		priceErrors[0].Message != "PRICE_GAP: no sample within the allowed gap" {
		t.Fatalf("price errors = %+v", priceErrors)
	}
}

type failingHistoricalPriceProvider struct {
	failure PriceFailure
}

func (p *failingHistoricalPriceProvider) USDPrices(context.Context, []Token) PriceResult {
	panic("a fixed-block scan must not ask for latest prices")
}

func (p *failingHistoricalPriceProvider) USDPricesAt(context.Context, []Token, time.Time) PriceResult {
	return PriceResult{Failures: []PriceFailure{p.failure}}
}

// End to end: a scan pinned to a past block prices the wallet through the historical path, at
// that block's own timestamp, and never touches the latest quote.
func TestEngineFixedBlockScanValuesHoldingsAtTheBlockTimestamp(t *testing.T) {
	const pinnedBlock, pinnedTimestamp = uint64(900), uint64(1_700_000_000)
	pool := &laggingPool{t: t, announcedHead: 1_000, servedHead: 1_000}
	pool.blockTimestamp = func(number uint64) uint64 {
		if number == pinnedBlock {
			return pinnedTimestamp
		}
		return 1
	}
	server := httptest.NewServer(pool)
	t.Cleanup(server.Close)
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	wallet := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Chains: []WalletBalanceChain{{
			Block: BlockRef{
				ChainID:   Ethereum,
				Number:    pinnedBlock,
				Hash:      common.BytesToHash([]byte{byte(pinnedBlock & 0xff)}),
				Timestamp: pinnedTimestamp,
			},
			Accounts: []WalletBalanceAccount{{
				Account: owner,
				Balances: []WalletBalance{{
					Token:     Token{ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6},
					AmountRaw: "250000000", MetadataComplete: true,
				}},
			}},
		}},
	}}
	prices := &recordingPriceProvider{}
	engine := NewEngineWithConfig(
		map[ChainID]string{Ethereum: server.URL},
		prices,
		EngineConfig{WalletBalanceProvider: wallet},
	)

	response := engine.ScanWithOptions(context.Background(), owner, ScanOptions{
		ProtocolIDs: map[string]struct{}{walletProtocolID: {}},
		ChainIDs:    map[ChainID]struct{}{Ethereum: {}},
		BlockNumber: map[ChainID]uint64{Ethereum: pinnedBlock},
	})

	if response.ChainBlocks[Ethereum] != pinnedBlock {
		t.Fatalf("response block = %d, want the pinned block %d", response.ChainBlocks[Ethereum], pinnedBlock)
	}
	if len(prices.tokens) != 0 {
		t.Fatalf("latest prices were requested for a fixed-block scan: %+v", prices.tokens)
	}
	if len(prices.historical) != 1 || !prices.historical[0].At.Equal(time.Unix(int64(pinnedTimestamp), 0)) {
		t.Fatalf("historical queries = %+v, want one at %d", prices.historical, pinnedTimestamp)
	}
	if len(prices.historical[0].Tokens) != 1 || prices.historical[0].Tokens[0].Address != walletTestUSDC {
		t.Fatalf("historical tokens = %+v, want USDC", prices.historical[0].Tokens)
	}
	group, exists := groupByID(response.Snapshots[0].Groups, walletTokenGroupID(walletTestUSDC))
	if !exists || group.Components[0].PriceUSD == nil || *group.Components[0].PriceUSD != 1 {
		t.Fatalf("USDC component was not priced from the historical quote: %+v", response.Snapshots)
	}
	if len(response.Errors) != 0 {
		t.Fatalf("scan errors = %+v", response.Errors)
	}
}
