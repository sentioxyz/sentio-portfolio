package portfolio

import (
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestFiveChainCanonicalIdentity(t *testing.T) {
	want := map[ChainID]struct {
		priceName string
		symbol    string
		wrapped   common.Address
	}{
		Polygon:   {"polygon", "POL", common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270")},
		Monad:     {"monad", "MON", common.HexToAddress("0x3bd359C1119dA7Da1D913D1C4D2B7c461115433A")},
		Plasma:    {"plasma", "XPL", common.HexToAddress("0x6100E367285b01F48D07953803A2d8dCA5D19873")},
		Avalanche: {"avalanche", "AVAX", common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")},
		Optimism:  {"optimism", "ETH", common.HexToAddress("0x4200000000000000000000000000000000000006")},
	}
	for chainID, expected := range want {
		if got := chainPriceNames[chainID]; got != expected.priceName {
			t.Errorf("chain %d price name = %q, want %q", chainID, got, expected.priceName)
		}
		coin, exists := walletNativeCoin[chainID]
		if !exists {
			t.Errorf("chain %d native coin is missing", chainID)
			continue
		}
		if coin.Symbol != expected.symbol || coin.Wrapped != expected.wrapped {
			t.Errorf("chain %d native coin = %+v, want %s/%s", chainID, coin, expected.symbol, expected.wrapped)
		}
	}
}

// TestFiveChainAdapterMatrix is the fail-closed inventory for the five-chain
// expansion. An omitted protocol/chain pair is intentionally unsupported: the
// kernel only advertises deployments whose current adapter semantics and
// historical boundaries were verified, rather than inferring support from a
// protocol brand appearing on an aggregator.
func TestFiveChainAdapterMatrix(t *testing.T) {
	targets := []ChainID{Polygon, Monad, Plasma, Avalanche, Optimism}
	want := map[string][]ChainID{
		"aave-v2":       {Polygon, Avalanche},
		"aave-v3":       {Polygon, Monad, Plasma, Avalanche, Optimism},
		"aave-v4":       {Avalanche},
		"beefy":         {Polygon, Monad, Plasma, Avalanche, Optimism},
		"compound-v3":   {Polygon, Optimism},
		"curve-lending": {Optimism},
		"etherfi":       {Optimism},
		"euler-v2":      {Polygon, Monad, Plasma, Avalanche},
		"fluid":         {Polygon, Plasma},
		"moonwell":      {Optimism},
		"morpho-blue":   {Polygon, Monad, Plasma, Avalanche, Optimism},
		"pendle":        {Monad, Plasma, Optimism},
		"sonne":         {Optimism},
		"spark":         {Avalanche, Optimism},
		"stader":        {Polygon},
		"uniswap-v3":    {Polygon, Monad, Plasma, Avalanche, Optimism},
		"uniswap-v4":    {Polygon, Monad, Avalanche, Optimism},
		"venus":         {Optimism},
		"vesper":        {Avalanche, Optimism},
		"wallet":        {Polygon, Monad, Plasma, Avalanche, Optimism},
		"yearn-v3":      {Polygon},
	}
	protocols := NewEngine(nil, nil).Protocols()
	if got, expected := len(protocols), 48; got != expected {
		t.Fatalf("protocol count = %d, want %d", got, expected)
	}
	for _, protocol := range protocols {
		var got []ChainID
		for _, chainID := range targets {
			if supportsChain(protocol.Chains, chainID) {
				got = append(got, chainID)
			}
		}
		if !reflect.DeepEqual(got, want[protocol.ID]) {
			t.Errorf("protocol %q target chains = %v, want %v", protocol.ID, got, want[protocol.ID])
		}
	}
}

func TestFiveChainBrandOnlyCandidatesStayUnsupported(t *testing.T) {
	// These labels are deliberate evidence notes for commonly conflated products.
	// The assertion keeps a future broad constructor refactor from silently
	// advertising them without adding the adapter-specific implementation.
	excluded := []struct {
		protocolID string
		chainID    ChainID
		reason     string
	}{
		{"lido", Optimism, "bridged Lido tokens are wallet holdings, not an Optimism Lido staking deployment"},
		{"frax-ether", Optimism, "bridged Frax LSTs do not expose the Ethereum staking adapter topology"},
		{"vesper", Polygon, "the official Vesper pool metadata has no Polygon deployment"},
		{"yearn-v3", Optimism, "no official current Yearn V3 vault manifest was verified"},
		{"yearn-v3", Avalanche, "no official current Yearn V3 vault manifest was verified"},
		{"pendle", Polygon, "no official Pendle V2 factory deployment was verified"},
		{"pendle", Avalanche, "the deployment file lacks the market and yield factories required by this adapter"},
		{"uniswap-v4", Plasma, "no official Uniswap V4 deployment was verified"},
		{"euler-v2", Optimism, "no official Euler V2 deployment was verified"},
		{"aave-v4", Polygon, "no official compatible Aave V4 deployment was verified"},
		{"aave-v4", Monad, "no official compatible Aave V4 deployment was verified"},
		{"aave-v4", Plasma, "no official compatible Aave V4 deployment was verified"},
		{"aave-v4", Optimism, "no official compatible Aave V4 deployment was verified"},
	}
	registrations := NewEngine(nil, nil).registrations
	for _, candidate := range excluded {
		if candidate.reason == "" {
			t.Fatalf("%s/%d has no exclusion reason", candidate.protocolID, candidate.chainID)
		}
		registration, exists := registrations[candidate.protocolID]
		if !exists {
			t.Fatalf("protocol %q is not registered", candidate.protocolID)
		}
		if supportsChain(registration.Info().Chains, candidate.chainID) {
			t.Errorf(
				"protocol %q unexpectedly advertises chain %d: %s",
				candidate.protocolID,
				candidate.chainID,
				candidate.reason,
			)
		}
	}
}
