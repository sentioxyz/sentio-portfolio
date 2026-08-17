package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

const (
	morphoMaxCurvePoolCoins = 8
	morphoMaxERC4626Depth   = 4
)

var morphoStrategyWrapperABI = MustABI(`[
  {"type":"function","name":"LENDING_PROTOCOL","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"REWARD_VAULT","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"lendingMarketId","stateMutability":"view","inputs":[],"outputs":[{"type":"bytes32"}]}
]`)

var morphoCurvePoolABI = MustABI(`[
  {"type":"function","name":"coins","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"balances","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"totalSupply","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

type morphoPoolCoin struct {
	Token   common.Address
	Balance *big.Int
}

func detectMorphoStrategyWrappers(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment morphoDeployment,
	positions []morphoCorePosition,
) (map[int]common.Address, error) {
	positionIndexes := make([]int, 0, len(positions))
	calls := make([]ContractCall, 0, len(positions)*3)
	for index, position := range positions {
		if position.Collateral.Sign() == 0 {
			continue
		}
		positionIndexes = append(positionIndexes, index)
		calls = append(calls,
			ContractCall{Contract: position.CollateralToken, ABI: morphoStrategyWrapperABI, Method: "LENDING_PROTOCOL"},
			ContractCall{Contract: position.CollateralToken, ABI: morphoStrategyWrapperABI, Method: "REWARD_VAULT"},
			ContractCall{Contract: position.CollateralToken, ABI: morphoStrategyWrapperABI, Method: "lendingMarketId"},
		)
	}
	if len(calls) == 0 {
		return nil, nil
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("probe Morpho collateral wrappers: %w", err)
	}
	wrappers := make(map[int]common.Address)
	for probeIndex, positionIndex := range positionIndexes {
		position := positions[positionIndex]
		probe := rows[probeIndex*3 : probeIndex*3+3]
		if probe[0].Error != nil || probe[1].Error != nil || probe[2].Error != nil {
			continue
		}
		lendingProtocol, protocolErr := AddressAt(probe[0].Values, 0)
		rewardVault, vaultErr := AddressAt(probe[1].Values, 0)
		marketID, marketErr := Bytes32At(probe[2].Values, 0)
		if protocolErr != nil || vaultErr != nil || marketErr != nil {
			return nil, fmt.Errorf("collateral wrapper %s returned malformed identity", position.CollateralToken)
		}
		if lendingProtocol == deployment.Morpho && marketID == position.MarketID && rewardVault != (common.Address{}) {
			wrappers[positionIndex] = rewardVault
		}
	}
	return wrappers, nil
}

func readMorphoCurvePoolCoins(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	pool common.Address,
) (*big.Int, []morphoPoolCoin, error) {
	calls := make([]ContractCall, 0, 1+morphoMaxCurvePoolCoins*2)
	calls = append(calls, ContractCall{Contract: pool, ABI: morphoCurvePoolABI, Method: "totalSupply"})
	for index := 0; index < morphoMaxCurvePoolCoins; index++ {
		calls = append(calls,
			ContractCall{Contract: pool, ABI: morphoCurvePoolABI, Method: "coins", Args: []any{big.NewInt(int64(index))}},
			ContractCall{Contract: pool, ABI: morphoCurvePoolABI, Method: "balances", Args: []any{big.NewInt(int64(index))}},
		)
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, nil, fmt.Errorf("read Curve pool %s: %w", pool, err)
	}
	if rows[0].Error != nil {
		return nil, nil, fmt.Errorf("Curve pool %s total supply: %w", pool, rows[0].Error)
	}
	totalSupply, err := BigIntAt(rows[0].Values, 0)
	if err != nil || totalSupply.Sign() <= 0 {
		return nil, nil, fmt.Errorf("Curve pool %s has invalid total supply", pool)
	}
	coins := make([]morphoPoolCoin, 0, morphoMaxCurvePoolCoins)
	terminated := false
	for index := 0; index < morphoMaxCurvePoolCoins; index++ {
		coinRow := rows[1+index*2]
		balanceRow := rows[2+index*2]
		if coinRow.Error != nil && balanceRow.Error != nil {
			terminated = true
			continue
		}
		if terminated || coinRow.Error != nil || balanceRow.Error != nil {
			return nil, nil, fmt.Errorf("Curve pool %s has a non-contiguous coin list at index %d", pool, index)
		}
		token, tokenErr := AddressAt(coinRow.Values, 0)
		balance, balanceErr := BigIntAt(balanceRow.Values, 0)
		if tokenErr != nil || balanceErr != nil || token == (common.Address{}) {
			return nil, nil, fmt.Errorf("Curve pool %s has an invalid coin at index %d", pool, index)
		}
		coins = append(coins, morphoPoolCoin{Token: token, Balance: balance})
	}
	if len(coins) < 2 {
		return nil, nil, fmt.Errorf("Curve pool %s has %d coins", pool, len(coins))
	}
	if !terminated {
		return nil, nil, fmt.Errorf("Curve pool %s exceeds the %d-coin safety bound", pool, morphoMaxCurvePoolCoins)
	}
	return totalSupply, coins, nil
}

// unwrapMorphoPoolCoin follows only ERC-4626-compatible pool constituents. The conversion is
// evaluated against the pool's complete integer reserve and the LP ownership fraction is applied
// afterwards. This preserves the same rounding boundary as the vault's on-chain convertToAssets.
func unwrapMorphoPoolCoin(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	token common.Address,
	reserve *big.Int,
) (common.Address, *big.Int, []common.Address, error) {
	currentToken := token
	currentReserve := new(big.Int).Set(reserve)
	path := make([]common.Address, 0, morphoMaxERC4626Depth)
	seen := map[common.Address]struct{}{token: {}}
	for depth := 0; depth <= morphoMaxERC4626Depth; depth++ {
		rows, err := client.ParallelCallsAllowFailure(ctx, block, []ContractCall{
			{Contract: currentToken, ABI: erc4626ABI, Method: "asset"},
			{Contract: currentToken, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{currentReserve}},
		})
		if err != nil {
			return common.Address{}, nil, nil, fmt.Errorf("probe nested ERC-4626 token %s: %w", currentToken, err)
		}
		if rows[0].Error != nil || rows[1].Error != nil {
			return currentToken, currentReserve, path, nil
		}
		if depth == morphoMaxERC4626Depth {
			return common.Address{}, nil, nil, fmt.Errorf(
				"nested ERC-4626 path from %s exceeds depth %d", token, morphoMaxERC4626Depth,
			)
		}
		asset, assetErr := AddressAt(rows[0].Values, 0)
		converted, amountErr := BigIntAt(rows[1].Values, 0)
		if assetErr != nil || amountErr != nil || asset == (common.Address{}) || converted.Sign() < 0 {
			return common.Address{}, nil, nil, fmt.Errorf("nested ERC-4626 token %s returned invalid state", currentToken)
		}
		if _, duplicate := seen[asset]; duplicate {
			return common.Address{}, nil, nil, fmt.Errorf("nested ERC-4626 cycle at %s", asset)
		}
		path = append(path, currentToken)
		seen[asset] = struct{}{}
		currentToken = asset
		currentReserve = converted
	}
	return common.Address{}, nil, nil, fmt.Errorf("nested ERC-4626 traversal from %s terminated unexpectedly", token)
}

func morphoStrategyCollateralComponents(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	position morphoCorePosition,
	rewardVault common.Address,
) ([]morphoComponentDraft, error) {
	vaultRows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: rewardVault, ABI: erc4626ABI, Method: "asset"},
		{Contract: rewardVault, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{position.Collateral}},
	})
	if err != nil {
		return nil, fmt.Errorf("strategy wrapper %s reward vault: %w", position.CollateralToken, err)
	}
	pool, err := AddressAt(vaultRows[0], 0)
	if err != nil || pool == (common.Address{}) {
		return nil, fmt.Errorf("strategy wrapper %s has invalid reward-vault asset", position.CollateralToken)
	}
	lpAmount, err := BigIntAt(vaultRows[1], 0)
	if err != nil {
		return nil, fmt.Errorf("strategy wrapper %s LP amount: %w", position.CollateralToken, err)
	}
	totalSupply, coins, err := readMorphoCurvePoolCoins(ctx, client, block, pool)
	if err != nil {
		return nil, fmt.Errorf("strategy wrapper %s: %w", position.CollateralToken, err)
	}
	components := make([]morphoComponentDraft, 0, len(coins))
	for index, coin := range coins {
		if coin.Balance.Sign() == 0 {
			continue
		}
		token, reserve, unwrapPath, unwrapErr := unwrapMorphoPoolCoin(
			ctx, client, block, coin.Token, coin.Balance,
		)
		if unwrapErr != nil {
			return nil, fmt.Errorf("strategy wrapper %s coin %d: %w", position.CollateralToken, index, unwrapErr)
		}
		amount := new(big.Int).Mul(lpAmount, reserve)
		components = append(components, morphoComponentDraft{
			Kind: "asset", Token: token, Amount: amount, Denominator: new(big.Int).Set(totalSupply),
			Source: Source{Contract: pool, Method: "pro-rata pool balances"},
			Metadata: map[string]any{
				"role": "collateral", "marketId": position.MarketID.Hex(),
				"strategyWrapper": position.CollateralToken, "rewardVault": rewardVault,
				"pool": pool, "poolCoin": coin.Token, "poolCoinIndex": index,
				"erc4626UnwrapPath": unwrapPath,
			},
		})
	}
	if len(components) == 0 {
		return nil, fmt.Errorf("strategy wrapper %s resolves to no assets", position.CollateralToken)
	}
	return components, nil
}

func expandMorphoStrategyCollateral(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment morphoDeployment,
	positions []morphoCorePosition,
	drafts []morphoGroupDraft,
) error {
	if len(positions) != len(drafts) {
		return fmt.Errorf("Morpho core position/draft count mismatch: %d/%d", len(positions), len(drafts))
	}
	wrappers, err := detectMorphoStrategyWrappers(ctx, client, block, deployment, positions)
	if err != nil {
		return err
	}
	for index, position := range positions {
		rewardVault, detected := wrappers[index]
		if !detected {
			continue
		}
		components, err := morphoStrategyCollateralComponents(
			ctx, client, block, position, rewardVault,
		)
		if err != nil {
			return err
		}
		retained := make([]morphoComponentDraft, 0, len(drafts[index].Components)-1+len(components))
		for _, component := range drafts[index].Components {
			if component.Metadata["role"] != "collateral" {
				retained = append(retained, component)
			}
		}
		drafts[index].Components = append(retained, components...)
		drafts[index].Metadata["collateralExpansion"] = "strategy-wrapper-curve-pool"
	}
	return nil
}
