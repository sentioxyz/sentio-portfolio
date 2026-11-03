package portfolio

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// earnETHStub answers every read readLidoEarnETH makes for holdsTestAccount, which holds four
// earnETH shares, with report as the oracle's wstETH report. At a price of 0.8 shares per wstETH
// the shares are five wstETH, which the stub converts to 6.1 stETH.
func earnETHStub(t *testing.T, report lidoEarnReport) *contractStub {
	oracle := common.HexToAddress("0x0000000000000000000000000000000000000c02")
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(lidoEarnETHAddress, lidoEarnShareManagerABI, "vault", nil, lidoEarnETHVaultAddress)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "shareManager", nil, lidoEarnETHAddress)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "oracle", nil, oracle)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "getAssetCount", nil, big.NewInt(1))
	stub.balance(lidoEarnETHAddress, big.NewInt(4_000_000_000_000_000_000))
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "assetAt", []any{big.NewInt(0)}, lidoWstETHAddress)
	stub.answer(oracle, lidoEarnOracleABI, "getReport", []any{lidoWstETHAddress}, report)
	stub.answer(lidoWstETHAddress, lidoWstETHABI, "getStETHByWstETH", []any{big.NewInt(5_000_000_000_000_000_000)},
		big.NewInt(6_100_000_000_000_000_000))
	return stub
}

// A report the oracle flagged as suspicious stays flagged at that block for good, so the position
// is valued at its price and carries a coverage note rather than failing every scan pinned there.
func TestLidoValuesEarnETHAtASuspiciousReportWithACoverageNote(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 24_500_000, Fixed: true}
	stub := earnETHStub(t, lidoEarnReport{
		PriceD18:     big.NewInt(800_000_000_000_000_000),
		Timestamp:    1_752_000_000,
		IsSuspicious: true,
	})
	ctx, coverage := withCoverage(context.Background())

	group, err := readLidoEarnETH(ctx, stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if group == nil || len(group.Components) != 1 || group.Components[0].AmountRaw != "6100000000000000000" {
		t.Fatalf("group = %+v, want 6.1 stETH", group)
	}
	notes := coverage.take()
	if len(notes) != 1 {
		t.Fatalf("coverage notes = %v, want one", notes)
	}
	want := "earnETH valued at a wstETH oracle report still awaiting acceptance (reported 2025-07-08T18:40:00Z)"
	if notes[0].Error() != want {
		t.Fatalf("coverage note = %q, want %q", notes[0], want)
	}
}

func TestLidoValuesEarnETHAtAnAcceptedReportWithoutANote(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 24_500_000, Fixed: true}
	stub := earnETHStub(t, lidoEarnReport{PriceD18: big.NewInt(800_000_000_000_000_000), Timestamp: 1_752_000_000})
	ctx, coverage := withCoverage(context.Background())

	group, err := readLidoEarnETH(ctx, stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if group == nil {
		t.Fatal("the earnETH position is missing")
	}
	if notes := coverage.take(); len(notes) != 0 {
		t.Fatalf("coverage notes = %v, want none", notes)
	}
}

// A report without a price cannot value the shares at all, flagged or not.
func TestLidoRejectsAnEarnETHReportWithoutAPrice(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 24_500_000, Fixed: true}
	for _, suspicious := range []bool{false, true} {
		stub := earnETHStub(t, lidoEarnReport{PriceD18: new(big.Int), Timestamp: 1_752_000_000, IsSuspicious: suspicious})
		ctx, coverage := withCoverage(context.Background())

		_, err := readLidoEarnETH(ctx, stub.dial(), block, holdsTestAccount)
		if err == nil || !strings.Contains(err.Error(), "has no price") {
			t.Fatalf("suspicious=%v: err = %v, want the missing price", suspicious, err)
		}
		if notes := coverage.take(); len(notes) != 0 {
			t.Fatalf("suspicious=%v: coverage notes = %v, want none", suspicious, notes)
		}
	}
}
