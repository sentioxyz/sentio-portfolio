package portfolio

import (
	"math"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestApplyValuationsUsesExactRawAmountsAndNetPolicies(t *testing.T) {
	usdc := Token{
		ChainID:  Ethereum,
		Address:  common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		Symbol:   "USDC",
		Decimals: 6,
	}
	snapshots := []Snapshot{{
		ProtocolID:     "test",
		ProtocolName:   "Test",
		ChainID:        Ethereum,
		NetValuePolicy: "floor-zero",
		Groups: []Group{{
			ID:             "lending",
			Label:          "Lending",
			NetValuePolicy: "floor-zero",
			Components: []Component{
				NewComponent("asset", usdc, mustBigInt(t, "100000000"), Source{}),
				NewComponent("debt", usdc, mustBigInt(t, "150000000"), Source{}),
			},
		}},
	}}

	summaries, err := applyValuations(
		snapshots,
		map[string]float64{PriceKey(usdc): 1},
		[]ProtocolInfo{{ID: "test", Name: "Test", Chains: []ChainID{Ethereum}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summary count = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if math.Abs(summary.AssetUSD-100) > 1e-9 ||
		math.Abs(summary.DebtUSD-150) > 1e-9 ||
		math.Abs(summary.TotalUSD) > 1e-9 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.PricedComponents != 2 || summary.ComponentCount != 2 {
		t.Fatalf("unexpected component coverage: %+v", summary)
	}
	if snapshots[0].Groups[0].ValueUSD == nil ||
		*snapshots[0].Groups[0].ValueUSD != 0 ||
		snapshots[0].ValueUSD == nil ||
		*snapshots[0].ValueUSD != 0 {
		t.Fatalf("floor-zero values were not applied: %+v", snapshots[0])
	}
}

func mustBigInt(t *testing.T, value string) *big.Int {
	t.Helper()
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid test big integer %q", value)
	}
	return parsed
}
