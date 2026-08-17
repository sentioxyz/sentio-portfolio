package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var aaveUmbrellaRewardsABI = MustABI(`[
  {
    "type":"function",
    "name":"calculateCurrentUserRewards",
    "stateMutability":"view",
    "inputs":[
      {"name":"asset","type":"address"},
      {"name":"user","type":"address"}
    ],
    "outputs":[
      {"name":"rewardsList","type":"address[]"},
      {"name":"unclaimedAmounts","type":"uint256[]"}
    ]
  }
]`)

var aaveReceiptTokenABI = MustABI(`[
  {
    "type":"function",
    "name":"UNDERLYING_ASSET_ADDRESS",
    "stateMutability":"view",
    "inputs":[],
    "outputs":[{"name":"underlying","type":"address"}]
  }
]`)

var aaveSafetyModuleABI = MustABI(`[
  {
    "type":"function",
    "name":"balanceOf",
    "stateMutability":"view",
    "inputs":[{"name":"account","type":"address"}],
    "outputs":[{"name":"balance","type":"uint256"}]
  },
  {
    "type":"function",
    "name":"getTotalRewardsBalance",
    "stateMutability":"view",
    "inputs":[{"name":"account","type":"address"}],
    "outputs":[{"name":"rewards","type":"uint256"}]
  },
  {
    "type":"function",
    "name":"STAKED_TOKEN",
    "stateMutability":"view",
    "inputs":[],
    "outputs":[{"name":"token","type":"address"}]
  },
  {
    "type":"function",
    "name":"REWARD_TOKEN",
    "stateMutability":"view",
    "inputs":[],
    "outputs":[{"name":"token","type":"address"}]
  }
]`)

type aaveUmbrellaStakeAsset struct {
	deploymentWindow
	Label         string
	StakeToken    common.Address
	Underlying    common.Address
	NestedERC4626 bool
}

type aaveSafetyModuleDeployment struct {
	deploymentWindow
	Address common.Address
}

type aaveUmbrellaDeployment struct {
	RewardsController common.Address
	StakeAssets       []aaveUmbrellaStakeAsset
}

var ethereumAaveV3Umbrella = aaveUmbrellaDeployment{
	RewardsController: common.HexToAddress("0x4655Ce3D625a63d30bA704087E52B4C31E38188B"),
	StakeAssets: []aaveUmbrellaStakeAsset{
		{
			deploymentWindow: deploymentWindow{ActivationBlock: 22_638_170},
			Label:            "waUSDC",
			StakeToken:       common.HexToAddress("0x6bf183243FdD1e306ad2C4450BC7dcf6f0bf8Aa6"),
			Underlying:       common.HexToAddress("0xD4fa2D31b7968E448877f69A96DE69f5de8cD23E"),
			NestedERC4626:    true,
		},
		{
			deploymentWindow: deploymentWindow{ActivationBlock: 22_638_170},
			Label:            "waUSDT",
			StakeToken:       common.HexToAddress("0xA484Ab92fe32B143AEE7019fC1502b1dAA522D31"),
			Underlying:       common.HexToAddress("0x7Bc3485026Ac48b6cf9BaF0A377477Fff5703Af8"),
			NestedERC4626:    true,
		},
		{
			deploymentWindow: deploymentWindow{ActivationBlock: 22_638_170},
			Label:            "waWETH",
			StakeToken:       common.HexToAddress("0xaAFD07D53A7365D3e9fb6F3a3B09EC19676B73Ce"),
			Underlying:       common.HexToAddress("0x0bfc9d54Fc184518A81162F8fB99c2eACa081202"),
			NestedERC4626:    true,
		},
		{
			deploymentWindow: deploymentWindow{ActivationBlock: 22_638_170},
			Label:            "GHO",
			StakeToken:       common.HexToAddress("0x4f827A63755855cDf3e8f3bcD20265C833f15033"),
			Underlying:       common.HexToAddress("0x40D16FC0246aD3160Ccc09B8D0D3A2cD28aE6C2f"),
		},
	},
}

var ethereumAaveV3SafetyModules = []aaveSafetyModuleDeployment{
	{
		deploymentWindow: deploymentWindow{ActivationBlock: 19_027_929},
		Address:          common.HexToAddress("0x1a88Df1cFe15Af22B3c4c783D4e6F7F9e0C1885d"),
	},
}

func decodeBigInts(value any) ([]*big.Int, error) {
	converted := abi.ConvertType(value, new([]*big.Int))
	amounts, ok := converted.(*[]*big.Int)
	if !ok || amounts == nil {
		return nil, fmt.Errorf("unexpected uint256 array type %T", value)
	}
	result := make([]*big.Int, len(*amounts))
	for index, amount := range *amounts {
		if amount == nil {
			return nil, fmt.Errorf("uint256 array contains nil at index %d", index)
		}
		result[index] = new(big.Int).Set(amount)
	}
	return result, nil
}

func readAaveRewardToken(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	address common.Address,
) (Token, error) {
	underlyingResult, err := client.Call(
		ctx,
		block,
		address,
		aaveReceiptTokenABI,
		"UNDERLYING_ASSET_ADDRESS",
	)
	if err == nil {
		underlying, decodeErr := AddressAt(underlyingResult, 0)
		if decodeErr != nil {
			return Token{}, decodeErr
		}
		if underlying == (common.Address{}) {
			return Token{}, fmt.Errorf("reward receipt %s returned zero underlying", address)
		}
		address = underlying
	}
	return readToken(ctx, client, block, address)
}

func readAaveSafetyModulePositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	modules []aaveSafetyModuleDeployment,
) ([]Group, error) {
	groups := make([]Group, 0, len(modules))
	for _, deployment := range modules {
		if !deployment.ActiveAt(block.Number) {
			continue
		}
		module := deployment.Address
		amountRows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{
				Contract: module,
				ABI:      aaveSafetyModuleABI,
				Method:   "balanceOf",
				Args:     []any{account},
			},
			{
				Contract: module,
				ABI:      aaveSafetyModuleABI,
				Method:   "getTotalRewardsBalance",
				Args:     []any{account},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%s balances: %w", module, err)
		}
		stakedAmount, err := BigIntAt(amountRows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s staked amount: %w", module, err)
		}
		rewardAmount, err := BigIntAt(amountRows[1], 0)
		if err != nil {
			return nil, fmt.Errorf("%s reward amount: %w", module, err)
		}
		if stakedAmount.Sign() == 0 && rewardAmount.Sign() == 0 {
			continue
		}

		tokenRows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: module, ABI: aaveSafetyModuleABI, Method: "STAKED_TOKEN"},
			{Contract: module, ABI: aaveSafetyModuleABI, Method: "REWARD_TOKEN"},
		})
		if err != nil {
			return nil, fmt.Errorf("%s token addresses: %w", module, err)
		}
		stakedAddress, err := AddressAt(tokenRows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s staked token: %w", module, err)
		}
		rewardAddress, err := AddressAt(tokenRows[1], 0)
		if err != nil {
			return nil, fmt.Errorf("%s reward token: %w", module, err)
		}
		tokens := make(map[common.Address]Token, 2)
		for _, address := range []common.Address{stakedAddress, rewardAddress} {
			if _, exists := tokens[address]; exists {
				continue
			}
			value, tokenErr := readToken(ctx, client, block, address)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s token %s: %w", module, address, tokenErr)
			}
			tokens[address] = value
		}

		components := make([]Component, 0, 2)
		if stakedAmount.Sign() > 0 {
			components = append(components, NewComponent(
				"asset",
				tokens[stakedAddress],
				stakedAmount,
				Source{Contract: module, Method: "balanceOf"},
			))
		}
		if rewardAmount.Sign() > 0 {
			components = append(components, NewComponent(
				"reward",
				tokens[rewardAddress],
				rewardAmount,
				Source{Contract: module, Method: "getTotalRewardsBalance"},
			))
		}
		groups = append(groups, Group{
			ID:         "safety-module:" + strings.ToLower(module.Hex()),
			MarketID:   "safety-module:" + strings.ToLower(module.Hex()),
			Label:      "Staked",
			Components: components,
			Metadata:   map[string]any{"module": module},
		})
	}
	return groups, nil
}

func readAaveUmbrellaPrincipal(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	stake aaveUmbrellaStakeAsset,
	shares *big.Int,
) (Component, error) {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: stake.StakeToken, ABI: erc4626ABI, Method: "asset"},
		{
			Contract: stake.StakeToken,
			ABI:      erc4626ABI,
			Method:   "convertToAssets",
			Args:     []any{shares},
		},
	})
	if err != nil {
		return Component{}, err
	}
	configuredAsset, err := AddressAt(rows[0], 0)
	if err != nil {
		return Component{}, fmt.Errorf("decode configured asset: %w", err)
	}
	if configuredAsset != stake.Underlying {
		return Component{}, fmt.Errorf(
			"stake asset changed from %s to %s",
			stake.Underlying,
			configuredAsset,
		)
	}
	amount, err := BigIntAt(rows[1], 0)
	if err != nil {
		return Component{}, fmt.Errorf("decode converted stake shares: %w", err)
	}
	source := Source{Contract: stake.StakeToken, Method: "convertToAssets(balanceOf)"}
	metadata := map[string]any{
		"shares":     shares.String(),
		"stakeToken": stake.StakeToken,
	}
	if stake.NestedERC4626 {
		nestedRows, nestedErr := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: configuredAsset, ABI: erc4626ABI, Method: "asset"},
			{
				Contract: configuredAsset,
				ABI:      erc4626ABI,
				Method:   "convertToAssets",
				Args:     []any{amount},
			},
		})
		if nestedErr != nil {
			return Component{}, fmt.Errorf("unwrap nested ERC-4626: %w", nestedErr)
		}
		asset, decodeErr := AddressAt(nestedRows[0], 0)
		if decodeErr != nil {
			return Component{}, fmt.Errorf("decode nested asset: %w", decodeErr)
		}
		amount, decodeErr = BigIntAt(nestedRows[1], 0)
		if decodeErr != nil {
			return Component{}, fmt.Errorf("decode nested assets: %w", decodeErr)
		}
		source = Source{Contract: configuredAsset, Method: "convertToAssets"}
		metadata["wrappedShares"] = rows[1][0].(*big.Int).String()
		metadata["wrapper"] = configuredAsset
		configuredAsset = asset
	}
	assetToken, err := readToken(ctx, client, block, configuredAsset)
	if err != nil {
		return Component{}, fmt.Errorf("read principal token: %w", err)
	}
	component := NewComponent("asset", assetToken, amount, source)
	component.Metadata = metadata
	return component, nil
}

func readAaveUmbrellaPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment aaveUmbrellaDeployment,
) ([]Group, error) {
	groups := make([]Group, 0, len(deployment.StakeAssets))
	for _, stake := range deployment.StakeAssets {
		if !stake.ActiveAt(block.Number) {
			continue
		}
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{
				Contract: stake.StakeToken,
				ABI:      erc20ABI,
				Method:   "balanceOf",
				Args:     []any{account},
			},
			{
				Contract: deployment.RewardsController,
				ABI:      aaveUmbrellaRewardsABI,
				Method:   "calculateCurrentUserRewards",
				Args:     []any{stake.StakeToken, account},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", stake.Label, err)
		}
		shares, err := BigIntAt(rows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s shares: %w", stake.Label, err)
		}
		rewardAddresses, err := decodeAddresses(rows[1][0])
		if err != nil {
			return nil, fmt.Errorf("%s reward addresses: %w", stake.Label, err)
		}
		rewardAmounts, err := decodeBigInts(rows[1][1])
		if err != nil {
			return nil, fmt.Errorf("%s reward amounts: %w", stake.Label, err)
		}
		if len(rewardAddresses) != len(rewardAmounts) {
			return nil, fmt.Errorf(
				"%s reward enumeration mismatch: %d addresses and %d amounts",
				stake.Label,
				len(rewardAddresses),
				len(rewardAmounts),
			)
		}

		components := make([]Component, 0, 1+len(rewardAddresses))
		if shares.Sign() > 0 {
			principal, principalErr := readAaveUmbrellaPrincipal(
				ctx,
				client,
				block,
				stake,
				shares,
			)
			if principalErr != nil {
				return nil, fmt.Errorf("%s principal: %w", stake.Label, principalErr)
			}
			components = append(components, principal)
		}
		for index, amount := range rewardAmounts {
			if amount.Sign() == 0 {
				continue
			}
			rewardToken, rewardErr := readAaveRewardToken(
				ctx,
				client,
				block,
				rewardAddresses[index],
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s reward %s: %w", stake.Label, rewardAddresses[index], rewardErr)
			}
			component := NewComponent(
				"reward",
				rewardToken,
				amount,
				Source{
					Contract: deployment.RewardsController,
					Method:   "calculateCurrentUserRewards",
				},
			)
			component.Metadata = map[string]any{"rewardReceipt": rewardAddresses[index]}
			components = append(components, component)
		}
		if len(components) == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:         strings.ToLower(stake.StakeToken.Hex()),
			MarketID:   strings.ToLower(stake.StakeToken.Hex()),
			Label:      "Yield · Umbrella " + stake.Label,
			Components: components,
			Metadata: map[string]any{
				"stakeToken":      stake.StakeToken,
				"configuredAsset": stake.Underlying,
				"nestedErc4626":   stake.NestedERC4626,
			},
		})
	}
	return groups, nil
}
