package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

var liquityV1ABI = MustABI(`[
  {"type":"function","name":"getEntireDebtAndColl","stateMutability":"view","inputs":[{"name":"borrower","type":"address"}],"outputs":[{"name":"debt","type":"uint256"},{"name":"coll","type":"uint256"},{"name":"pendingLUSDDebtReward","type":"uint256"},{"name":"pendingETHReward","type":"uint256"}]},
  {"type":"function","name":"getCompoundedLUSDDeposit","stateMutability":"view","inputs":[{"name":"depositor","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getDepositorETHGain","stateMutability":"view","inputs":[{"name":"depositor","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getDepositorLQTYGain","stateMutability":"view","inputs":[{"name":"depositor","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakes","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getPendingETHGain","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getPendingLUSDGain","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"earned","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

var liquityV1PairABI = MustABI(`[
  {"type":"function","name":"token0","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"token1","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getReserves","stateMutability":"view","inputs":[],"outputs":[{"type":"uint112"},{"type":"uint112"},{"type":"uint32"}]},
  {"type":"function","name":"totalSupply","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

var (
	liquityTroveManager  = common.HexToAddress("0xA39739EF8b0231DbFA0DcdA07d7e29faAbCf4bb2")
	liquityStabilityPool = common.HexToAddress("0x66017D22b0f8556afDd19FC67041899Eb65a21bb")
	liquityLQTYStaking   = common.HexToAddress("0x4f9Fbb3f1E99B56e0Fe2892e623Ed36A76Fc605d")
	liquityUniPool       = common.HexToAddress("0xd37a77E71ddF3373a79BE2eBB76B6c4808bDF0d5")
	liquityPair          = common.HexToAddress("0xF20EF17b889b437C151eB5bA15A47bFc62bfF469")
	liquityETH           = token(Ethereum, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "ETH", 18)
	liquityLUSD          = token(Ethereum, "0x5f98805A4E8be255a32880FDeC7F6728C6568bA0", "LUSD", 18)
	liquityLQTY          = token(Ethereum, "0x6DEA81C8171D0bA574754EF6F8b412F2Ed88c54D", "LQTY", 18)
)

var (
	liquityTroveManagerDeployment  = deploymentWindow{ActivationBlock: 12_178_557}
	liquityStabilityPoolDeployment = deploymentWindow{ActivationBlock: 12_178_565}
	liquityPairDeployment          = deploymentWindow{ActivationBlock: 12_178_599}
	liquityUniPoolDeployment       = deploymentWindow{ActivationBlock: 12_178_602}
	liquityLQTYStakingDeployment   = deploymentWindow{ActivationBlock: 12_178_607}
)

type liquityV1Adapter struct{ adapterBase }

func newLiquityV1Adapter() Adapter {
	return &liquityV1Adapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "liquity-v1", Name: "Liquity V1", Chains: []ChainID{Ethereum},
	}}}
}

type liquityV1CallPlan struct {
	calls           []ContractCall
	troveOffset     int
	stabilityOffset int
	stakingOffset   int
	unipoolOffset   int
}

func newLiquityV1CallPlan(block uint64, account common.Address) liquityV1CallPlan {
	plan := liquityV1CallPlan{
		troveOffset: -1, stabilityOffset: -1, stakingOffset: -1, unipoolOffset: -1,
	}
	if liquityTroveManagerDeployment.ActiveAt(block) {
		plan.troveOffset = len(plan.calls)
		plan.calls = append(plan.calls, ContractCall{
			Contract: liquityTroveManager,
			ABI:      liquityV1ABI,
			Method:   "getEntireDebtAndColl",
			Args:     []any{account},
		})
	}
	if liquityStabilityPoolDeployment.ActiveAt(block) {
		plan.stabilityOffset = len(plan.calls)
		plan.calls = append(plan.calls,
			ContractCall{Contract: liquityStabilityPool, ABI: liquityV1ABI, Method: "getCompoundedLUSDDeposit", Args: []any{account}},
			ContractCall{Contract: liquityStabilityPool, ABI: liquityV1ABI, Method: "getDepositorETHGain", Args: []any{account}},
			ContractCall{Contract: liquityStabilityPool, ABI: liquityV1ABI, Method: "getDepositorLQTYGain", Args: []any{account}},
		)
	}
	if liquityLQTYStakingDeployment.ActiveAt(block) {
		plan.stakingOffset = len(plan.calls)
		plan.calls = append(plan.calls,
			ContractCall{Contract: liquityLQTYStaking, ABI: liquityV1ABI, Method: "stakes", Args: []any{account}},
			ContractCall{Contract: liquityLQTYStaking, ABI: liquityV1ABI, Method: "getPendingETHGain", Args: []any{account}},
			ContractCall{Contract: liquityLQTYStaking, ABI: liquityV1ABI, Method: "getPendingLUSDGain", Args: []any{account}},
		)
	}
	if liquityUniPoolDeployment.ActiveAt(block) {
		plan.unipoolOffset = len(plan.calls)
		plan.calls = append(plan.calls,
			ContractCall{Contract: liquityUniPool, ABI: liquityV1ABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: liquityUniPool, ABI: liquityV1ABI, Method: "earned", Args: []any{account}},
		)
	}
	return plan
}

func appendLiquityComponent(
	components []Component,
	kind string,
	token Token,
	amount *big.Int,
	contract common.Address,
	method string,
) []Component {
	if amount.Sign() <= 0 {
		return components
	}
	return append(components, NewComponent(kind, token, amount, Source{Contract: contract, Method: method}))
}

func appendLiquityGroup(groups []Group, id, label string, components []Component) []Group {
	if len(components) == 0 {
		return groups
	}
	return append(groups, Group{ID: id, MarketID: id, Label: label, Components: components})
}

func (a *liquityV1Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	plan := newLiquityV1CallPlan(block.Number, account)
	if len(plan.calls) == 0 {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, plan.calls)
	if err != nil {
		return nil, fmt.Errorf("Liquity v1 account state: %w", err)
	}

	groups := make([]Group, 0, 4)
	if plan.troveOffset >= 0 {
		debt, decodeErr := BigIntAt(rows[plan.troveOffset], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("trove debt: %w", decodeErr)
		}
		collateral, decodeErr := BigIntAt(rows[plan.troveOffset], 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("trove collateral: %w", decodeErr)
		}
		trove := make([]Component, 0, 2)
		trove = appendLiquityComponent(trove, "asset", liquityETH, collateral, liquityTroveManager, "getEntireDebtAndColl")
		trove = appendLiquityComponent(trove, "debt", liquityLUSD, debt, liquityTroveManager, "getEntireDebtAndColl")
		groups = appendLiquityGroup(groups, "trove", "Trove", trove)
	}

	if plan.stabilityOffset >= 0 {
		values := make([]*big.Int, 3)
		for index := range values {
			values[index], err = BigIntAt(rows[plan.stabilityOffset+index], 0)
			if err != nil {
				return nil, fmt.Errorf("stability pool result %d: %w", index, err)
			}
		}
		stability := make([]Component, 0, 3)
		stability = appendLiquityComponent(stability, "asset", liquityLUSD, values[0], liquityStabilityPool, "getCompoundedLUSDDeposit")
		stability = appendLiquityComponent(stability, "reward", liquityETH, values[1], liquityStabilityPool, "getDepositorETHGain")
		stability = appendLiquityComponent(stability, "reward", liquityLQTY, values[2], liquityStabilityPool, "getDepositorLQTYGain")
		groups = appendLiquityGroup(groups, "stability-pool", "Stability Pool", stability)
	}

	if plan.stakingOffset >= 0 {
		values := make([]*big.Int, 3)
		for index := range values {
			values[index], err = BigIntAt(rows[plan.stakingOffset+index], 0)
			if err != nil {
				return nil, fmt.Errorf("LQTY staking result %d: %w", index, err)
			}
		}
		staking := make([]Component, 0, 3)
		staking = appendLiquityComponent(staking, "asset", liquityLQTY, values[0], liquityLQTYStaking, "stakes")
		staking = appendLiquityComponent(staking, "reward", liquityETH, values[1], liquityLQTYStaking, "getPendingETHGain")
		staking = appendLiquityComponent(staking, "reward", liquityLUSD, values[2], liquityLQTYStaking, "getPendingLUSDGain")
		groups = appendLiquityGroup(groups, "lqty-staking", "LQTY staking", staking)
	}

	if plan.unipoolOffset >= 0 {
		stakedLP, decodeErr := BigIntAt(rows[plan.unipoolOffset], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Unipool balance: %w", decodeErr)
		}
		reward, decodeErr := BigIntAt(rows[plan.unipoolOffset+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Unipool reward: %w", decodeErr)
		}
		unipool := make([]Component, 0, 3)
		if stakedLP.Sign() > 0 {
			if !liquityPairDeployment.ActiveAt(block.Number) {
				return nil, fmt.Errorf("Liquity Unipool pair is not active at block %d", block.Number)
			}
			pairRows, pairErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: liquityPair, ABI: liquityV1PairABI, Method: "token0"},
				{Contract: liquityPair, ABI: liquityV1PairABI, Method: "token1"},
				{Contract: liquityPair, ABI: liquityV1PairABI, Method: "getReserves"},
				{Contract: liquityPair, ABI: liquityV1PairABI, Method: "totalSupply"},
			})
			if pairErr != nil {
				return nil, fmt.Errorf("Liquity Unipool pair state: %w", pairErr)
			}
			token0, decodeErr := AddressAt(pairRows[0], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Unipool token0: %w", decodeErr)
			}
			token1, decodeErr := AddressAt(pairRows[1], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Unipool token1: %w", decodeErr)
			}
			known := map[common.Address]Token{liquityLUSD.Address: liquityLUSD, liquityETH.Address: liquityETH}
			if token0 == token1 || known[token0].Address == (common.Address{}) || known[token1].Address == (common.Address{}) {
				return nil, fmt.Errorf("Liquity Unipool pair identity changed to %s/%s", token0, token1)
			}
			reserve0, decodeErr := BigIntAt(pairRows[2], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Unipool reserve0: %w", decodeErr)
			}
			reserve1, decodeErr := BigIntAt(pairRows[2], 1)
			if decodeErr != nil {
				return nil, fmt.Errorf("Unipool reserve1: %w", decodeErr)
			}
			totalSupply, decodeErr := BigIntAt(pairRows[3], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Unipool total supply: %w", decodeErr)
			}
			if totalSupply.Sign() <= 0 {
				return nil, fmt.Errorf("Unipool total supply is not positive")
			}
			for _, row := range []struct {
				token   Token
				reserve *big.Int
			}{{known[token0], reserve0}, {known[token1], reserve1}} {
				amount := new(big.Int).Mul(stakedLP, row.reserve)
				amount.Div(amount, totalSupply)
				unipool = appendLiquityComponent(unipool, "asset", row.token, amount, liquityPair, "Unipool.balanceOf * pair.getReserves / totalSupply")
			}
		}
		unipool = appendLiquityComponent(unipool, "reward", liquityLQTY, reward, liquityUniPool, "earned")
		groups = appendLiquityGroup(groups, "unipool", "Legacy Unipool", unipool)
	}
	return groups, nil
}
