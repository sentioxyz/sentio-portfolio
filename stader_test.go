package portfolio

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStaderBSCDeploymentAndRegistration(t *testing.T) {
	adapter := newStaderAdapter().(*StaderAdapter)
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, BSC}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Stader chains = %v, want %v", got, want)
	}
	if got, want := staderBSCDeployment.liquidToken.Address,
		common.HexToAddress("0x1bdd3cf7f79cfb8edbb955f20ad99211551ba275"); got != want {
		t.Fatalf("BNBx token = %s, want %s", got, want)
	}
	if got, want := staderBSCDeployment.manager,
		common.HexToAddress("0x3b961e83400D51e6E1AF5c450d3C7d7b80588d28"); got != want {
		t.Fatalf("BNBx manager = %s, want %s", got, want)
	}
	if staderBSCDeployment.tokenActivationBlock != 19_907_065 {
		t.Fatalf("BNBx token activation block = %d, want 19907065", staderBSCDeployment.tokenActivationBlock)
	}
	if staderBSCDeployment.managerActivationBlock != 40_990_394 {
		t.Fatalf("BNBx manager activation block = %d, want 40990394", staderBSCDeployment.managerActivationBlock)
	}
}

func TestStaderBSCWithdrawalAmount(t *testing.T) {
	amount, err := staderBSCWithdrawalAmount(
		big.NewInt(125),
		[]staderBSCProcessedWithdrawal{
			{amountInBNBx: big.NewInt(20), batchAmountInBNB: big.NewInt(33), batchAmountInBNBx: big.NewInt(30)},
			{amountInBNBx: big.NewInt(10), batchAmountInBNB: big.NewInt(33), batchAmountInBNBx: big.NewInt(30)},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// 125 is the already-converted total for unprocessed requests. The processed
	// requests receive 20/30 and 10/30 of a 33 BNB batch: 22 + 11 BNB.
	if got, want := amount, big.NewInt(158); got.Cmp(want) != 0 {
		t.Fatalf("withdrawal amount = %s, want %s", got, want)
	}
	if _, err := staderBSCWithdrawalAmount(
		big.NewInt(0),
		[]staderBSCProcessedWithdrawal{{
			amountInBNBx: big.NewInt(1), batchAmountInBNB: big.NewInt(1), batchAmountInBNBx: big.NewInt(0),
		}},
	); err == nil {
		t.Fatal("zero batch BNBx denominator did not fail")
	}
}
