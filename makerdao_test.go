package portfolio

import (
	"math/big"
	"testing"
)

func makerTestBigInt(t *testing.T, value string) *big.Int {
	t.Helper()
	result, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid integer %q", value)
	}
	return result
}

func TestMakerAccountingVectors(t *testing.T) {
	pie := makerTestBigInt(t, "11394850674709709513568864")
	persistedChi := makerTestBigInt(t, "1178603257706378349786654086")
	dsr := makerTestBigInt(t, "1000000000393915525145987602")
	effective, err := makerAccruedChi(persistedChi, dsr, 1_785_268_667, 1_785_282_311)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1178609592224933414086736439"; effective.String() != want {
		t.Fatalf("effective chi = %s, want %s", effective, want)
	}
	if got, want := makerSavingsRaw(pie, effective).String(), "13430080307183618113496924"; got != want {
		t.Fatalf("savings = %s, want %s", got, want)
	}
	if got := makerCollateralRaw(new(big.Int).Mul(big.NewInt(9), makerWad), 8); got.String() != "900000000" {
		t.Fatalf("collateral = %s, want 900000000", got)
	}
}

func TestEngineRegistersMakerDAO(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		if protocol.ID == "makerdao" {
			return
		}
	}
	t.Fatal("makerdao is not registered")
}
