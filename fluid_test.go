package portfolio

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestFluidDeploymentsCoverSupportedChains(t *testing.T) {
	for _, chainID := range SupportedChainIDs {
		deployment, exists := fluidDeployments[chainID]
		if !exists {
			t.Fatalf("Fluid deployment is absent for chain %d", chainID)
		}
		for name, address := range map[string]common.Address{
			"lending resolver":         deployment.LendingResolver,
			"vault factory":            deployment.VaultFactory,
			"vault resolver":           deployment.VaultResolver,
			"vault positions resolver": deployment.VaultPositionsResolver,
			"DEX resolver":             deployment.DexResolver,
		} {
			if address == (common.Address{}) {
				t.Errorf("chain %d %s is zero", chainID, name)
			}
		}
		if deployment.LendingWindow.ActivationBlock == 0 ||
			deployment.VaultWindow.ActivationBlock == 0 || deployment.DexWindow.ActivationBlock == 0 {
			t.Errorf("chain %d has an empty Fluid activation block", chainID)
		}
		for _, vault := range deployment.LiteVaults {
			if vault.Address == (common.Address{}) || vault.Window.ActivationBlock == 0 {
				t.Errorf("chain %d has an invalid Fluid Lite vault: %+v", chainID, vault)
			}
		}
		for _, rewards := range deployment.StakingRewards {
			if rewards.Address == (common.Address{}) || rewards.Window.ActivationBlock == 0 {
				t.Errorf("chain %d has invalid Fluid staking rewards: %+v", chainID, rewards)
			}
		}
	}
}

func TestFluidSmartShareAmountsRemainExactRationals(t *testing.T) {
	shares := new(big.Int).SetUint64(123_456_789)
	ratio := new(big.Int).SetUint64(987_654_321)
	numerator := new(big.Int).Mul(shares, ratio)
	component := NewComponent("asset", Token{}, numerator, Source{})
	component.AmountDenominatorRaw = fluidShareScale.String()
	actual := new(big.Rat).SetFrac(
		new(big.Int).SetBytes(numerator.Bytes()),
		new(big.Int).SetBytes(fluidShareScale.Bytes()),
	)
	expected := new(big.Rat).SetFrac(
		new(big.Int).Mul(shares, ratio),
		new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
	)
	if actual.Cmp(expected) != 0 || component.AmountDenominatorRaw != "1000000000000000000" {
		t.Fatalf("smart share conversion = %s, want %s", actual, expected)
	}
}

func TestFluidNativeSentinelMapsToWrappedNative(t *testing.T) {
	for _, chainID := range SupportedChainIDs {
		address, native, err := fluidUnderlyingAddress(chainID, fluidNativeSentinel)
		if err != nil {
			t.Fatal(err)
		}
		if !native || address != fluidWrappedNative[chainID] {
			t.Fatalf("chain %d native mapping = %s, %t", chainID, address, native)
		}
	}
}
