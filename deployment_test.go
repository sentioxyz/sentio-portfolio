package portfolio

import (
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestDeploymentWindowBoundaries(t *testing.T) {
	window := deploymentWindow{ActivationBlock: 100, DeactivationBlock: 199}
	for _, test := range []struct {
		block uint64
		want  bool
	}{
		{block: 99, want: false},
		{block: 100, want: true},
		{block: 199, want: true},
		{block: 200, want: false},
	} {
		if got := window.ActiveAt(test.block); got != test.want {
			t.Fatalf("ActiveAt(%d) = %v, want %v", test.block, got, test.want)
		}
	}
	if !(deploymentWindow{ActivationBlock: 100}).ActiveAt(1_000_000) {
		t.Fatal("open-ended deployment is not active after activation")
	}
}

func requireAvailabilityBoundary(t *testing.T, name string, window availabilityWindow) {
	t.Helper()
	if !window.configured {
		t.Fatalf("%s availability is not configured", name)
	}
	start := window.deploymentWindow.ActivationBlock
	if start == 0 {
		t.Fatalf("%s unexpectedly starts at genesis", name)
	}
	if window.ActiveAt(start - 1) {
		t.Fatalf("%s is active before block %d", name, start)
	}
	if !window.ActiveAt(start) {
		t.Fatalf("%s is inactive at block %d", name, start)
	}
}

func TestCompoundV2ComponentAvailabilityIsExplicit(t *testing.T) {
	for _, candidate := range compoundV2Adapters() {
		adapter := candidate.(*CompoundV2Adapter)
		for chainID, deployment := range adapter.deployments {
			prefix := fmt.Sprintf("%s chain %d", adapter.Info().ID, chainID)
			requireAvailabilityBoundary(t, prefix+" comptroller", deployment.ComptrollerWindow)
			if deployment.RewardLens != (common.Address{}) {
				requireAvailabilityBoundary(t, prefix+" reward lens", deployment.RewardLensWindow)
			}
			if deployment.MultiRewardDistributor != (common.Address{}) {
				requireAvailabilityBoundary(t, prefix+" multi reward distributor", deployment.MultiRewardWindow)
			}
			for index, module := range deployment.StakingModules {
				requireAvailabilityBoundary(
					t,
					fmt.Sprintf("%s staking module %d", prefix, index),
					module.Window,
				)
			}
			for index, module := range deployment.DistributorStaking {
				requireAvailabilityBoundary(
					t,
					fmt.Sprintf("%s distributor staking module %d", prefix, index),
					module.Window,
				)
			}
		}
	}
}

func TestVenusComponentAvailabilityIsExplicit(t *testing.T) {
	adapter := newVenusAdapter().(*VenusAdapter)
	for chainID, deployment := range adapter.deployments {
		prefix := fmt.Sprintf("Venus chain %d", chainID)
		requireAvailabilityBoundary(t, prefix+" pool registry", deployment.PoolRegistryWindow)
		requireAvailabilityBoundary(t, prefix+" pool lens", deployment.PoolLensWindow)
		if deployment.XVSVault != (common.Address{}) {
			requireAvailabilityBoundary(t, prefix+" XVS vault", deployment.XVSVaultWindow)
		}
		if deployment.Core != nil {
			requireAvailabilityBoundary(t, prefix+" core", deployment.Core.ComptrollerWindow)
		}
		if deployment.CoreRewardsLens != (common.Address{}) {
			requireAvailabilityBoundary(t, prefix+" core rewards", deployment.CoreRewardsWindow)
		}
	}
}

func TestNewlyGatedProtocolComponentBoundaries(t *testing.T) {
	for name, window := range map[string]availabilityWindow{
		"Fraxlend registry":  fraxlendRegistryWindow,
		"Maker savings":      makerSavingsWindow,
		"Maker vaults":       makerVaultWindow,
		"Rocket Pool tokens": rocketTokenWindow,
		"Rocket Pool nodes":  rocketNodeWindow,
	} {
		requireAvailabilityBoundary(t, name, window)
	}

	adapter := lstAdapters()[0].(*ConvertedBalanceAdapter)
	position := adapter.positions[Ethereum][0]
	if position.ActivationBlock != 15_676_402 {
		t.Fatalf(
			"Liquid Collective activation block = %d, want 15676402",
			position.ActivationBlock,
		)
	}
}

func TestAaveHistoricalMarketVersions(t *testing.T) {
	var adapter *AaveAdapter
	for _, candidate := range aaveAdapters() {
		if candidate.Info().ID == "aave-v3" {
			adapter = candidate.(*AaveAdapter)
			break
		}
	}
	if adapter == nil {
		t.Fatal("Aave v3 adapter is not registered")
	}

	markets, err := adapter.activeMarkets(Ethereum, 17_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) != 1 {
		t.Fatalf("active market count = %d, want 1", len(markets))
	}
	if got, want := markets[0].Label, "Core"; got != want {
		t.Fatalf("market label = %q, want %q", got, want)
	}
	if got, want := markets[0].DataProvider,
		common.HexToAddress("0x7B4EB56E7CD4b454BA8ff71E4518426369a138a3"); got != want {
		t.Fatalf("historical provider = %s, want %s", got, want)
	}
}

func TestCompoundHistoricalMarketActivation(t *testing.T) {
	adapter := newCompoundV3Adapter().(*CompoundV3Adapter)
	markets := adapter.activeMarkets(Ethereum, 17_000_000)
	if len(markets) != 2 {
		t.Fatalf("active market count = %d, want 2", len(markets))
	}
	if markets[0].Label != "USDC" || markets[1].Label != "WETH" {
		t.Fatalf("active markets = %q, %q; want USDC, WETH", markets[0].Label, markets[1].Label)
	}
}

func TestLidoHistoricalSidecarActivation(t *testing.T) {
	if !lidoWithdrawalDeployment.ActiveAt(20_000_000) {
		t.Fatal("withdrawal queue should be active at block 20,000,000")
	}
	if lidoEarnETHDeployment.ActiveAt(20_000_000) {
		t.Fatal("earnETH should not be active at block 20,000,000")
	}
}

func TestERC4626VaultsHaveActivationBlocks(t *testing.T) {
	for _, candidate := range erc4626Adapters() {
		adapter := candidate.(*ERC4626Adapter)
		for chainID, vaults := range adapter.vaults {
			for _, vault := range vaults {
				if vault.ActivationBlock == 0 {
					t.Errorf(
						"%s vault %s on chain %d has no activation block",
						adapter.Info().ID,
						vault.Address,
						chainID,
					)
				}
			}
		}
	}
}

func TestEthenaHistoricalVaultActivation(t *testing.T) {
	var adapter *ERC4626Adapter
	for _, candidate := range erc4626Adapters() {
		if candidate.Info().ID == "ethena" {
			adapter = candidate.(*ERC4626Adapter)
			break
		}
	}
	if adapter == nil {
		t.Fatal("Ethena adapter is not registered")
	}

	for _, test := range []struct {
		block uint64
		ids   []string
	}{
		{block: 18_571_358},
		{block: 18_571_359, ids: []string{"susde:staked"}},
		{block: 20_713_441, ids: []string{"susde:staked"}},
		{block: 20_713_442, ids: []string{"susde:staked", "sena:staked"}},
	} {
		active := activeVaultsAt(adapter.vaults[Ethereum], test.block)
		if len(active) != len(test.ids) {
			t.Fatalf(
				"active vault count at block %d = %d, want %d",
				test.block,
				len(active),
				len(test.ids),
			)
		}
		for index, want := range test.ids {
			if got := active[index].ID; got != want {
				t.Fatalf(
					"active vault %d at block %d = %q, want %q",
					index,
					test.block,
					got,
					want,
				)
			}
		}
	}
}

func TestLiquityV1IsRegistered(t *testing.T) {
	engine := NewEngine(nil, nil)
	for _, protocol := range engine.Protocols() {
		if protocol.ID == "liquity-v1" {
			if len(protocol.Chains) != 1 || protocol.Chains[0] != Ethereum {
				t.Fatalf("liquity-v1 chains = %v, want [Ethereum]", protocol.Chains)
			}
			return
		}
	}
	t.Fatal("liquity-v1 is not registered")
}

func TestLiquityV1ComponentDeploymentBoundaries(t *testing.T) {
	for name, deployment := range map[string]deploymentWindow{
		"trove manager":  liquityTroveManagerDeployment,
		"stability pool": liquityStabilityPoolDeployment,
		"pair":           liquityPairDeployment,
		"Unipool":        liquityUniPoolDeployment,
		"LQTY staking":   liquityLQTYStakingDeployment,
	} {
		if deployment.ActiveAt(deployment.ActivationBlock - 1) {
			t.Errorf("%s is active before deployment block %d", name, deployment.ActivationBlock)
		}
		if !deployment.ActiveAt(deployment.ActivationBlock) {
			t.Errorf("%s is inactive at deployment block %d", name, deployment.ActivationBlock)
		}
	}

	account := common.HexToAddress("0x1234")
	for _, test := range []struct {
		name  string
		block uint64
		want  map[common.Address]int
	}{
		{name: "before trove manager", block: 12_178_556, want: map[common.Address]int{}},
		{name: "at trove manager", block: 12_178_557, want: map[common.Address]int{liquityTroveManager: 1}},
		{name: "before stability pool", block: 12_178_564, want: map[common.Address]int{liquityTroveManager: 1}},
		{name: "at stability pool", block: 12_178_565, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3}},
		{name: "before pair", block: 12_178_598, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3}},
		{name: "at pair", block: 12_178_599, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3}},
		{name: "before Unipool", block: 12_178_601, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3}},
		{name: "at Unipool", block: 12_178_602, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3, liquityUniPool: 2}},
		{name: "before LQTY staking", block: 12_178_606, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3, liquityUniPool: 2}},
		{name: "at LQTY staking", block: 12_178_607, want: map[common.Address]int{liquityTroveManager: 1, liquityStabilityPool: 3, liquityUniPool: 2, liquityLQTYStaking: 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := newLiquityV1CallPlan(test.block, account)
			got := make(map[common.Address]int)
			for _, call := range plan.calls {
				got[call.Contract]++
			}
			if len(got) != len(test.want) {
				t.Fatalf("active contracts at block %d = %v, want %v", test.block, got, test.want)
			}
			for contract, count := range test.want {
				if got[contract] != count {
					t.Errorf("calls to %s at block %d = %d, want %d", contract, test.block, got[contract], count)
				}
			}
		})
	}
}
