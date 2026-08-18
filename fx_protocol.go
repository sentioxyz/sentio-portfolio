package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const fxMaxRewardTokensPerPool = 64

var fxRebalancePoolABI = MustABI(`[
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getActiveRewardTokens","stateMutability":"view","inputs":[],"outputs":[{"type":"address[]"}]},
  {"type":"function","name":"getHistoricalRewardTokens","stateMutability":"view","inputs":[],"outputs":[{"type":"address[]"}]},
  {"type":"function","name":"baseRewardToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"extraRewardsLength","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"extraRewards","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"unlockedBalanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"unlockingBalanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]},
  {"type":"function","name":"claimable","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"token","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type fxRebalancePool struct {
	Address         common.Address
	ActivationBlock uint64
	Legacy          bool
}

type FxProtocolAdapter struct {
	adapterBase
	pools []fxRebalancePool
}

func newFxProtocolAdapter() Adapter {
	pool := func(address string, activation uint64, legacy ...bool) fxRebalancePool {
		return fxRebalancePool{
			Address: common.HexToAddress(address), ActivationBlock: activation,
			Legacy: len(legacy) > 0 && legacy[0],
		}
	}
	return &FxProtocolAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "fxprotocol", Name: "f(x) Protocol", Chains: []ChainID{Ethereum},
		}},
		pools: []fxRebalancePool{
			pool("0x9aD382b028e03977D446635Ba6b8492040F829b7", 19_287_792),
			pool("0x0417CE2934899d7130229CDa39Db456Ff2332685", 19_287_793),
			pool("0xb925F8CAA6BE0BFCd1A7383168D1c932D185A748", 19_287_949),
			pool("0x4a2ab45D27428901E826db4a52Dae00594b68022", 19_287_950),
			pool("0xc2DeF1E39FF35367F2F2a312a793477C576fD4c3", 19_460_726),
			pool("0x7EB0ed173480299e1310d55E04Ece401c2B06626", 19_460_727),
			pool("0xf58c499417e36714e99803Cb135f507a95ae7169", 19_489_641),
			pool("0xBa947cba270D30967369Bf1f73884Be2533d7bDB", 19_489_642),
			pool("0xf291EC9C2F87A41386fd94eC4BCdC3270eD04482", 19_682_620),
			pool("0xBB549046497364A1E26F94f7e93685Dc29FAd8c0", 19_682_622),
			pool("0x0AB9Dc99a33Cd02A776a9117f211803Fb69Fd7C4", 20_291_292),
			pool("0xA04d761adad1029e4f2F60ac973a76c5307EfceA", 20_291_293),
			pool("0xa677d95B91530d56791FbA72C01a862f1B01A49e", 17_818_955, true),
			pool("0xc6dEe5913e010895F3702bc43a40d661B13a40BD", 18_781_002),
			pool("0xB87A8332dFb1C76Bb22477dCfEdDeB69865cA9f9", 18_781_003),
		},
	}
}

type fxPoolState struct {
	pool         fxRebalancePool
	asset        common.Address
	balance      *big.Int
	unlockAt     *big.Int
	rewardTokens []common.Address
}

func uniqueFxRewardTokens(active, historical []common.Address) ([]common.Address, error) {
	unique := make(map[common.Address]struct{}, len(active)+len(historical))
	for _, address := range append(append([]common.Address(nil), active...), historical...) {
		if address != (common.Address{}) {
			unique[address] = struct{}{}
		}
	}
	if len(unique) > fxMaxRewardTokensPerPool {
		return nil, fmt.Errorf("reward token count exceeds %d", fxMaxRewardTokensPerPool)
	}
	result := make([]common.Address, 0, len(unique))
	for address := range unique {
		result = append(result, address)
	}
	sort.Slice(result, func(left, right int) bool {
		return strings.ToLower(result[left].Hex()) < strings.ToLower(result[right].Hex())
	})
	return result, nil
}

func (a *FxProtocolAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	active := make([]fxRebalancePool, 0, len(a.pools))
	headerCalls := make([]ContractCall, 0, len(a.pools)*4)
	for _, pool := range a.pools {
		if block.Number < pool.ActivationBlock {
			continue
		}
		active = append(active, pool)
		headerCalls = append(headerCalls,
			ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "asset"},
			ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "balanceOf", Args: []any{account}},
		)
		if pool.Legacy {
			headerCalls = append(headerCalls,
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "baseRewardToken"},
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "extraRewardsLength"},
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "unlockedBalanceOf", Args: []any{account}},
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "unlockingBalanceOf", Args: []any{account}},
			)
		} else {
			headerCalls = append(headerCalls,
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "getActiveRewardTokens"},
				ContractCall{Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "getHistoricalRewardTokens"},
			)
		}
	}
	if len(headerCalls) == 0 {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, headerCalls)
	if err != nil {
		return nil, fmt.Errorf("rebalance pool headers: %w", err)
	}
	states := make([]fxPoolState, 0, len(active))
	metadataAddresses := make([]common.Address, 0)
	claimCalls := make([]ContractCall, 0)
	rowIndex := 0
	for _, pool := range active {
		asset, decodeErr := AddressAt(rows[rowIndex], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("pool %s asset: %w", pool.Address, decodeErr)
		}
		balance, decodeErr := BigIntAt(rows[rowIndex+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("pool %s balance: %w", pool.Address, decodeErr)
		}
		rewards := make([]common.Address, 0)
		unlockAt := new(big.Int)
		if pool.Legacy {
			baseReward, addressErr := AddressAt(rows[rowIndex+2], 0)
			if addressErr != nil {
				return nil, fmt.Errorf("pool %s base reward: %w", pool.Address, addressErr)
			}
			extraLength, lengthErr := BigIntAt(rows[rowIndex+3], 0)
			if lengthErr != nil || !extraLength.IsUint64() || extraLength.Uint64() > fxMaxRewardTokensPerPool {
				return nil, fmt.Errorf("pool %s extra reward count is invalid", pool.Address)
			}
			extraCalls := make([]ContractCall, 0, extraLength.Uint64())
			for extraIndex := uint64(0); extraIndex < extraLength.Uint64(); extraIndex++ {
				extraCalls = append(extraCalls, ContractCall{
					Contract: pool.Address, ABI: fxRebalancePoolABI, Method: "extraRewards",
					Args: []any{new(big.Int).SetUint64(extraIndex)},
				})
			}
			extraRows, extraErr := client.ParallelCalls(ctx, block, extraCalls)
			if extraErr != nil {
				return nil, fmt.Errorf("pool %s extra rewards: %w", pool.Address, extraErr)
			}
			extras := make([]common.Address, 0, len(extraRows))
			for extraIndex, extraRow := range extraRows {
				extra, addressErr := AddressAt(extraRow, 0)
				if addressErr != nil {
					return nil, fmt.Errorf("pool %s extra reward %d: %w", pool.Address, extraIndex, addressErr)
				}
				extras = append(extras, extra)
			}
			rewards, decodeErr = uniqueFxRewardTokens([]common.Address{baseReward}, extras)
			if decodeErr != nil {
				return nil, fmt.Errorf("pool %s: %w", pool.Address, decodeErr)
			}
			unlocked, amountErr := BigIntAt(rows[rowIndex+4], 0)
			if amountErr != nil {
				return nil, fmt.Errorf("pool %s unlocked balance: %w", pool.Address, amountErr)
			}
			unlocking, amountErr := BigIntAt(rows[rowIndex+5], 0)
			if amountErr != nil {
				return nil, fmt.Errorf("pool %s unlocking balance: %w", pool.Address, amountErr)
			}
			unlockAt, amountErr = BigIntAt(rows[rowIndex+5], 1)
			if amountErr != nil {
				return nil, fmt.Errorf("pool %s unlock time: %w", pool.Address, amountErr)
			}
			balance.Add(balance, unlocked)
			balance.Add(balance, unlocking)
			rowIndex += 6
		} else {
			activeRewards, rewardsErr := AddressSliceAt(rows[rowIndex+2], 0)
			if rewardsErr != nil {
				return nil, fmt.Errorf("pool %s active rewards: %w", pool.Address, rewardsErr)
			}
			historicalRewards, rewardsErr := AddressSliceAt(rows[rowIndex+3], 0)
			if rewardsErr != nil {
				return nil, fmt.Errorf("pool %s historical rewards: %w", pool.Address, rewardsErr)
			}
			rewards, decodeErr = uniqueFxRewardTokens(activeRewards, historicalRewards)
			if decodeErr != nil {
				return nil, fmt.Errorf("pool %s: %w", pool.Address, decodeErr)
			}
			rowIndex += 4
		}
		state := fxPoolState{
			pool: pool, asset: asset, balance: balance, unlockAt: unlockAt, rewardTokens: rewards,
		}
		states = append(states, state)
		metadataAddresses = append(metadataAddresses, asset)
		for _, reward := range rewards {
			metadataAddresses = append(metadataAddresses, reward)
			claimCalls = append(claimCalls, ContractCall{
				Contract: pool.Address, ABI: fxRebalancePoolABI,
				Method: "claimable", Args: []any{account, reward},
			})
		}
	}
	claimRows, err := client.ParallelCallsAllowFailure(ctx, block, claimCalls)
	if err != nil {
		return nil, fmt.Errorf("rebalance pool rewards: %w", err)
	}
	tokens, err := readERC20Tokens(ctx, client, block, metadataAddresses)
	if err != nil {
		return nil, fmt.Errorf("rebalance pool token metadata: %w", err)
	}
	groups := make([]Group, 0, len(states))
	claimIndex := 0
	for _, state := range states {
		components := make([]Component, 0, 1+len(state.rewardTokens))
		if state.balance.Sign() > 0 {
			component := NewComponent(
				"asset", tokens[state.asset], state.balance,
				Source{Contract: state.pool.Address, Method: "balanceOf"},
			)
			if state.pool.Legacy {
				component.Source.Method = "balanceOf+unlockedBalanceOf+unlockingBalanceOf"
				component.Metadata = map[string]any{"unlockAt": state.unlockAt.String()}
			}
			components = append(components, component)
		}
		for _, reward := range state.rewardTokens {
			claimResult := claimRows[claimIndex]
			claimIndex++
			if claimResult.Error != nil {
				// The original 2023 pool divides by an uninitialized user snapshot for
				// accounts that never touched that pool. A zero balance makes that revert
				// an authenticated absence; a live position must still fail explicitly.
				if state.balance.Sign() == 0 && state.pool.Legacy {
					continue
				}
				return nil, fmt.Errorf(
					"pool %s reward %s: %w", state.pool.Address, reward, claimResult.Error,
				)
			}
			amount, decodeErr := BigIntAt(claimResult.Values, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("pool %s reward %s: %w", state.pool.Address, reward, decodeErr)
			}
			if amount.Sign() == 0 {
				continue
			}
			components = append(components, NewComponent(
				"reward", tokens[reward], amount,
				Source{Contract: state.pool.Address, Method: "claimable"},
			))
		}
		if len(components) == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:         strings.ToLower(state.pool.Address.Hex()),
			MarketID:   strings.ToLower(state.pool.Address.Hex()),
			Label:      "Rebalance pool · " + tokens[state.asset].Symbol,
			Components: components,
			Metadata:   map[string]any{"pool": state.pool.Address},
		})
	}
	return groups, nil
}
