package portfolio

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestListaManifestAndRegistration(t *testing.T) {
	adapter := newListaAdapter().(*ListaAdapter)
	if got, want := len(adapter.moolah[BSC].Markets), 56; got != want {
		t.Fatalf("Lista BSC markets = %d, want %d", got, want)
	}
	if got, want := len(adapter.moolah[Ethereum].Markets), 6; got != want {
		t.Fatalf("Lista Ethereum markets = %d, want %d", got, want)
	}
	if got, want := len(adapter.moolah[BSC].Vaults), 7; got != want {
		t.Fatalf("Lista callable BSC vaults = %d, want %d", got, want)
	}
	if got, want := len(adapter.moolah[Ethereum].Vaults), 1; got != want {
		t.Fatalf("Lista callable Ethereum vaults = %d, want %d", got, want)
	}
	if got, want := len(adapter.cdp.CandidateTokens), 68; got != want {
		t.Fatalf("Lista CDP candidates = %d, want %d", got, want)
	}
}

func TestListaShareFractionUsesProtocolVirtualBalances(t *testing.T) {
	numerator, denominator := listaShareFraction(big.NewInt(7), big.NewInt(99), big.NewInt(1_000_000))
	if numerator.String() != "700" || denominator.String() != "2000000" {
		t.Fatalf("fraction = %s/%s, want 700/2000000", numerator, denominator)
	}
}

func TestListaExpectedMarketBalancesAccruesInterestAndFee(t *testing.T) {
	state := listaMoolahMarketState{
		TotalSupplyAssets: big.NewInt(2_000_000_000),
		TotalSupplyShares: big.NewInt(1_000_000_000),
		TotalBorrowAssets: big.NewInt(1_000_000_000),
		TotalBorrowShares: big.NewInt(500_000_000),
		LastUpdate:        big.NewInt(100),
		Fee:               big.NewInt(100_000_000_000_000_000),
	}
	got, feeShares := listaExpectedMarketBalances(state, big.NewInt(1_000_000_000_000), 1_000)
	// Taylor factor is 1_000_500_166_666_666, yielding 1_000_500 units of interest.
	if got.TotalBorrowAssets.String() != "1001000500" {
		t.Fatalf("total borrow assets = %s, want 1001000500", got.TotalBorrowAssets)
	}
	if got.TotalSupplyAssets.String() != "2001000500" {
		t.Fatalf("total supply assets = %s, want 2001000500", got.TotalSupplyAssets)
	}
	if got.TotalSupplyShares.Cmp(state.TotalSupplyShares) <= 0 {
		t.Fatalf("fee shares were not minted: got %s", got.TotalSupplyShares)
	}
	if feeShares.String() != "50052" {
		t.Fatalf("pending fee shares = %s, want 50052", feeShares)
	}
	if state.TotalBorrowAssets.String() != "1000000000" {
		t.Fatalf("input state was mutated: %s", state.TotalBorrowAssets)
	}
}

func TestListaMulDivUp(t *testing.T) {
	if got := listaMulDivUp(big.NewInt(10), big.NewInt(10), big.NewInt(6)); got.String() != "17" {
		t.Fatalf("mulDivUp = %s, want 17", got)
	}
}

func TestListaFeeRecipientLoadsFeeOnlyMarket(t *testing.T) {
	feeRecipient := common.HexToAddress("0x00000000000000000000000000000000000000f1")
	empty := listaMoolahHolding{
		supplyShares: new(big.Int),
		borrowShares: new(big.Int),
		collateral:   new(big.Int),
	}
	if !listaShouldLoadMoolahMarket(empty, feeRecipient, feeRecipient) {
		t.Fatal("fee recipient must load an all-zero stored position to discover pending fee shares")
	}
	other := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	if listaShouldLoadMoolahMarket(empty, other, feeRecipient) {
		t.Fatal("ordinary account must not load an all-zero stored position")
	}
}

func TestListaEffectiveSupplySharesCreditsPendingFeesOnlyToRecipient(t *testing.T) {
	feeRecipient := common.HexToAddress("0x00000000000000000000000000000000000000f1")
	stored := big.NewInt(7)
	pendingFees := big.NewInt(5)
	if got := listaEffectiveSupplyShares(stored, pendingFees, feeRecipient, feeRecipient); got.String() != "12" {
		t.Fatalf("fee recipient effective shares = %s, want 12", got)
	}
	other := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	if got := listaEffectiveSupplyShares(stored, pendingFees, other, feeRecipient); got.String() != "7" {
		t.Fatalf("ordinary account effective shares = %s, want 7", got)
	}
	if stored.String() != "7" {
		t.Fatalf("stored shares were mutated: %s", stored)
	}
}

func TestListaUncreatedMarketValidation(t *testing.T) {
	emptyHolding := listaMoolahHolding{
		id:           common.HexToHash("0x01"),
		supplyShares: new(big.Int),
		borrowShares: new(big.Int),
		collateral:   new(big.Int),
	}
	uncreated := listaMoolahHeldMarket{
		holding: emptyHolding,
		state:   listaMoolahMarketState{LastUpdate: new(big.Int)},
	}
	process, err := listaShouldProcessMoolahMarket(uncreated)
	if err != nil || process {
		t.Fatalf("all-zero uncreated market = process %t, error %v; want skip without error", process, err)
	}

	created := uncreated
	created.state.LastUpdate = big.NewInt(1)
	process, err = listaShouldProcessMoolahMarket(created)
	if err != nil || !process {
		t.Fatalf("created market = process %t, error %v; want process", process, err)
	}

	inconsistent := uncreated
	inconsistent.holding.supplyShares = big.NewInt(1)
	process, err = listaShouldProcessMoolahMarket(inconsistent)
	if err == nil || process {
		t.Fatalf("nonzero uncreated market = process %t, error %v; want explicit error", process, err)
	}
}
