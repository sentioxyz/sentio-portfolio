package portfolio

import (
	"context"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var aaveDataProviderABI = MustABI(`[
  {
    "type":"function",
    "name":"getAllReservesTokens",
    "stateMutability":"view",
    "inputs":[],
    "outputs":[{"type":"tuple[]","components":[{"name":"symbol","type":"string"},{"name":"tokenAddress","type":"address"}]}]
  },
  {
    "type":"function",
    "name":"getUserReserveData",
    "stateMutability":"view",
    "inputs":[{"name":"asset","type":"address"},{"name":"user","type":"address"}],
    "outputs":[
      {"name":"currentATokenBalance","type":"uint256"},
      {"name":"currentStableDebt","type":"uint256"},
      {"name":"currentVariableDebt","type":"uint256"},
      {"name":"principalStableDebt","type":"uint256"},
      {"name":"scaledVariableDebt","type":"uint256"},
      {"name":"stableBorrowRate","type":"uint256"},
      {"name":"liquidityRate","type":"uint256"},
      {"name":"stableRateLastUpdated","type":"uint40"},
      {"name":"usageAsCollateralEnabled","type":"bool"}
    ]
  },
  {
    "type":"function",
    "name":"getReserveTokensAddresses",
    "stateMutability":"view",
    "inputs":[{"name":"asset","type":"address"}],
    "outputs":[
      {"name":"aTokenAddress","type":"address"},
      {"name":"stableDebtTokenAddress","type":"address"},
      {"name":"variableDebtTokenAddress","type":"address"}
    ]
  }
]`)

var aaveV2RewardsControllerABI = MustABI(`[
  {
    "type":"function",
    "name":"getRewardsBalance",
    "stateMutability":"view",
    "inputs":[{"name":"assets","type":"address[]"},{"name":"user","type":"address"}],
    "outputs":[{"name":"rewards","type":"uint256"}]
  }
]`)

var aaveV3RewardsControllerABI = MustABI(`[
  {
    "type":"function",
    "name":"getAllUserRewards",
    "stateMutability":"view",
    "inputs":[{"name":"assets","type":"address[]"},{"name":"user","type":"address"}],
    "outputs":[{"name":"rewardsList","type":"address[]"},{"name":"unclaimedAmounts","type":"uint256[]"}]
  }
]`)

type aaveReserve struct {
	Symbol       string
	TokenAddress common.Address
}

type aaveMarket struct {
	deploymentWindow
	Label        string
	Pool         common.Address
	DataProvider common.Address
}

type aaveV2RewardDeployment struct {
	deploymentWindow
	Controller common.Address
	Token      Token
}

type AaveAdapter struct {
	adapterBase
	markets map[ChainID][]aaveMarket
	vaults  map[ChainID][]vaultConfig
}

var ethereumAaveV2SafetyModules = []aaveSafetyModuleDeployment{
	{
		Address:          common.HexToAddress("0x4da27a545c0c5B758a6BA100e3a049001de870f5"),
		deploymentWindow: deploymentWindow{ActivationBlock: 10_927_018},
	},
	{
		Address:          common.HexToAddress("0xa1116930326D21fB917d5A27F1E9943A9595fb47"),
		deploymentWindow: deploymentWindow{ActivationBlock: 11_751_685},
	},
	{
		Address:          common.HexToAddress("0x9eDA81C21C273a82BE9Bbc19B6A6182212068101"),
		deploymentWindow: deploymentWindow{ActivationBlock: 19_034_135},
	},
}

var aaveV2RewardDeployments = map[ChainID]aaveV2RewardDeployment{
	Ethereum: {
		deploymentWindow: deploymentWindow{ActivationBlock: 12_251_569},
		Controller:       common.HexToAddress("0xd784927Ff2f95ba542BfC824c8a8a98F3495f6b5"),
		Token: token(
			Ethereum,
			"0x7Fc66500c84A76Ad7e9c93437bFc5Ac33E2dDaE9",
			"AAVE",
			18,
		),
	},
	Polygon: {
		deploymentWindow: deploymentWindow{ActivationBlock: 12_486_774},
		Controller:       common.HexToAddress("0x357D51124f59836DeD84c8a1730D72B749d8BC23"),
		Token: token(
			Polygon,
			"0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270",
			"WPOL",
			18,
		),
	},
	Avalanche: {
		deploymentWindow: deploymentWindow{ActivationBlock: 3_424_262},
		Controller:       common.HexToAddress("0x01D83Fe6A10D2f2B7AF17034343746188272cAc9"),
		Token: token(
			Avalanche,
			"0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7",
			"WAVAX",
			18,
		),
	},
}

var ethereumSparkRewardsController = common.HexToAddress(
	"0x4370D3b6C9588E02ce9D22e684387859c7Ff5b34",
)

var ethereumSparkRewardsDeployment = deploymentWindow{ActivationBlock: 16_776_417}

var sparkSavingsVaults = map[ChainID][]vaultConfig{
	Ethereum: {
		{
			ID:              "0xbc65ad17c5c0a2a4d159fa5a503f4992c7b545fe",
			Label:           "Yield · sUSDC",
			Address:         common.HexToAddress("0xbc65ad17c5c0a2a4d159fa5a503f4992c7b545fe"),
			Asset:           token(Ethereum, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDC", 6),
			ActivationBlock: 21_969_024,
		},
		{
			ID:              "0x28b3a8fb53b741a8fd78c0fb9a6b2393d896a43d",
			Label:           "Yield · spUSDC",
			Address:         common.HexToAddress("0x28b3a8fb53b741a8fd78c0fb9a6b2393d896a43d"),
			Asset:           token(Ethereum, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDC", 6),
			ActivationBlock: 23_484_422,
		},
		{
			ID:              "0x80128dbb9f07b93dde62a6daeadb69ed14a7d354",
			Label:           "Yield · spPYUSD",
			Address:         common.HexToAddress("0x80128dbb9f07b93dde62a6daeadb69ed14a7d354"),
			Asset:           token(Ethereum, "0x6c3ea9036406852006290770BEDFcAbA0e23A0e8", "PYUSD", 6),
			ActivationBlock: 23_919_207,
		},
		{
			ID:              "0xe2e7a17dff93280dec073c995595155283e3c372",
			Label:           "Yield · spUSDT",
			Address:         common.HexToAddress("0xe2e7a17dff93280dec073c995595155283e3c372"),
			Asset:           token(Ethereum, "0xdac17f958d2ee523a2206206994597c13d831ec7", "USDT", 6),
			ActivationBlock: 23_484_439,
		},
		{
			ID:              "0xfe6eb3b609a7c8352a241f7f3a21cea4e9209b8f",
			Label:           "Yield · spETH",
			Address:         common.HexToAddress("0xfe6eb3b609a7c8352a241f7f3a21cea4e9209b8f"),
			Asset:           token(Ethereum, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "WETH", 18),
			ActivationBlock: 23_484_474,
		},
	},
	Base: {
		{
			ID:              "0x3128a0f7f0ea68e7b7c9b00afa7e41045828e858",
			Label:           "Yield · sUSDC",
			Address:         common.HexToAddress("0x3128a0F7f0ea68E7B7c9B00AFa7E41045828e858"),
			Asset:           token(Base, "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913", "USDC", 6),
			ActivationBlock: 27_123_520,
		},
	},
	Arbitrum: {
		{
			ID:              "0x940098b108fb7d0a7e374f6eded7760787464609",
			Label:           "Yield · sUSDC",
			Address:         common.HexToAddress("0x940098b108fB7D0a7E374f6eDED7760787464609"),
			Asset:           token(Arbitrum, "0xaf88d065e77c8cC2239327C5EDb3A432268e5831", "USDC", 6),
			ActivationBlock: 311_940_473,
		},
		{
			ID:              "0x45d91340b3b7b96985a72b5c678f7d9e8d664b62",
			Label:           "Yield · spUSDT",
			Address:         common.HexToAddress("0x45d91340B3B7B96985A72b5c678F7D9e8D664b62"),
			Asset:           token(Arbitrum, "0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9", "USDT", 6),
			ActivationBlock: 476_542_096,
		},
	},
	Avalanche: {
		{
			ID:              "0x28b3a8fb53b741a8fd78c0fb9a6b2393d896a43d",
			Label:           "Yield · spUSDC",
			Address:         common.HexToAddress("0x28B3a8fb53B741A8Fd78c0fb9A6B2393d896a43d"),
			Asset:           token(Avalanche, "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", "USDC", 6),
			ActivationBlock: 69_983_672,
		},
	},
	Optimism: {
		{
			ID:              "0xcf9326e24ebffbef22ce1050007a43a3c0b6db55",
			Label:           "Yield · sUSDC",
			Address:         common.HexToAddress("0xCF9326e24EBfFBEF22ce1050007A43A3c0B6DB55"),
			Asset:           token(Optimism, "0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85", "USDC", 6),
			ActivationBlock: 136_322_256,
		},
	},
}

func NewAaveAdapter(id, name string, markets map[ChainID][]aaveMarket) *AaveAdapter {
	return newAaveAdapter(id, name, markets, nil)
}

func newAaveAdapter(
	id string,
	name string,
	markets map[ChainID][]aaveMarket,
	vaults map[ChainID][]vaultConfig,
) *AaveAdapter {
	deployments := make(map[ChainID]struct{}, len(markets)+len(vaults))
	for chainID := range markets {
		deployments[chainID] = struct{}{}
	}
	for chainID := range vaults {
		deployments[chainID] = struct{}{}
	}
	return &AaveAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: id, Name: name, Chains: deploymentChains(deployments),
		}},
		markets: markets,
		vaults:  vaults,
	}
}

func (a *AaveAdapter) activeMarkets(chainID ChainID, block uint64) ([]aaveMarket, error) {
	active := make([]aaveMarket, 0, len(a.markets[chainID]))
	labels := make(map[string]struct{})
	for _, market := range a.markets[chainID] {
		if !market.ActiveAt(block) {
			continue
		}
		if _, exists := labels[market.Label]; exists {
			return nil, fmt.Errorf(
				"multiple %s deployments are active at block %d",
				market.Label,
				block,
			)
		}
		labels[market.Label] = struct{}{}
		active = append(active, market)
	}
	return active, nil
}

func decodeAaveReserves(value any) ([]aaveReserve, error) {
	converted := abi.ConvertType(value, new([]aaveReserve))
	reserves, ok := converted.(*[]aaveReserve)
	if !ok || reserves == nil {
		return nil, fmt.Errorf("unexpected Aave reserve list type %T", value)
	}
	return *reserves, nil
}

func decodeAaveIncentiveAssets(
	reserves []aaveReserve,
	tokenRows [][]any,
	includeStableDebt bool,
) ([]common.Address, error) {
	if len(tokenRows) != len(reserves) {
		return nil, fmt.Errorf(
			"reserve token row count %d differs from reserve count %d",
			len(tokenRows),
			len(reserves),
		)
	}
	incentiveAssets := make([]common.Address, 0, len(tokenRows)*3)
	seen := make(map[common.Address]struct{}, len(tokenRows)*3)
	for index, row := range tokenRows {
		if len(row) != 3 {
			return nil, fmt.Errorf(
				"%s reserve token tuple has %d fields",
				reserves[index].Symbol,
				len(row),
			)
		}
		for tokenIndex, tokenKind := range []string{"aToken", "stable debt token", "variable debt token"} {
			// Aave's V2 and V3 reward controllers read every supplied asset
			// through the scaled-balance interface. Stable debt tokens do not
			// implement that interface, so production callers exclude them.
			if tokenIndex == 1 && !includeStableDebt {
				continue
			}
			address, decodeErr := AddressAt(row, tokenIndex)
			if decodeErr != nil {
				return nil, fmt.Errorf("%s %s: %w", reserves[index].Symbol, tokenKind, decodeErr)
			}
			if address == (common.Address{}) {
				if tokenIndex == 0 {
					return nil, fmt.Errorf("%s returned zero aToken", reserves[index].Symbol)
				}
				continue
			}
			if _, exists := seen[address]; exists {
				continue
			}
			seen[address] = struct{}{}
			incentiveAssets = append(incentiveAssets, address)
		}
	}
	return incentiveAssets, nil
}

func readAaveIncentiveAssets(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	dataProvider common.Address,
	reserves []aaveReserve,
	includeStableDebt bool,
) ([]common.Address, error) {
	tokenCalls := make([]ContractCall, 0, len(reserves))
	for _, reserve := range reserves {
		tokenCalls = append(tokenCalls, ContractCall{
			Contract: dataProvider,
			ABI:      aaveDataProviderABI,
			Method:   "getReserveTokensAddresses",
			Args:     []any{reserve.TokenAddress},
		})
	}
	tokenRows, err := client.ParallelCalls(ctx, block, tokenCalls)
	if err != nil {
		return nil, err
	}
	return decodeAaveIncentiveAssets(reserves, tokenRows, includeStableDebt)
}

func readSparkRewardComponents(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	incentiveAssets []common.Address,
	account common.Address,
) ([]Component, error) {
	result, err := client.Call(
		ctx,
		block,
		ethereumSparkRewardsController,
		aaveV3RewardsControllerABI,
		"getAllUserRewards",
		incentiveAssets,
		account,
	)
	if err != nil {
		return nil, err
	}
	if len(result) != 2 {
		return nil, fmt.Errorf("getAllUserRewards returned %d fields", len(result))
	}
	rewardAddresses, err := decodeAddresses(result[0])
	if err != nil {
		return nil, fmt.Errorf("reward addresses: %w", err)
	}
	rewardAmounts, err := decodeBigInts(result[1])
	if err != nil {
		return nil, fmt.Errorf("reward amounts: %w", err)
	}
	if len(rewardAddresses) != len(rewardAmounts) {
		return nil, fmt.Errorf(
			"reward result length differs: %d addresses, %d amounts",
			len(rewardAddresses),
			len(rewardAmounts),
		)
	}

	components := make([]Component, 0, len(rewardAddresses))
	for index, rewardAddress := range rewardAddresses {
		amount := rewardAmounts[index]
		if amount.Sign() == 0 {
			continue
		}
		rewardToken := Token{}
		sourceMethod := "getAllUserRewards"
		metadata := map[string]any(nil)
		if rewardAddress == lidoWstETHAddress {
			rows, convertErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: rewardAddress, ABI: lidoWstETHABI, Method: "stETH"},
				{
					Contract: rewardAddress,
					ABI:      lidoWstETHABI,
					Method:   "getStETHByWstETH",
					Args:     []any{amount},
				},
			})
			if convertErr != nil {
				return nil, fmt.Errorf("unwrap wstETH reward: %w", convertErr)
			}
			underlying, convertErr := AddressAt(rows[0], 0)
			if convertErr != nil {
				return nil, fmt.Errorf("wstETH underlying: %w", convertErr)
			}
			if underlying != lidoStETHAddress {
				return nil, fmt.Errorf("wstETH underlying changed to %s", underlying)
			}
			convertedAmount, convertErr := BigIntAt(rows[1], 0)
			if convertErr != nil {
				return nil, fmt.Errorf("wstETH reward conversion: %w", convertErr)
			}
			rewardToken = lidoStETHToken
			metadata = map[string]any{
				"rewardWrapper":    rewardAddress,
				"wrapperAmountRaw": amount.String(),
			}
			amount = convertedAmount
			sourceMethod = "getAllUserRewards + getStETHByWstETH"
		} else {
			rewardToken, err = readAaveRewardToken(ctx, client, block, rewardAddress)
			if err != nil {
				return nil, fmt.Errorf("%s reward token: %w", rewardAddress, err)
			}
		}
		component := NewComponent(
			"reward",
			rewardToken,
			amount,
			Source{Contract: ethereumSparkRewardsController, Method: sourceMethod},
		)
		component.Metadata = metadata
		components = append(components, component)
	}
	return components, nil
}

func (a *AaveAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	markets, err := a.activeMarkets(block.ChainID, block.Number)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0, len(markets))
	for _, market := range markets {
		listResult, err := client.Call(
			ctx,
			block,
			market.DataProvider,
			aaveDataProviderABI,
			"getAllReservesTokens",
		)
		if err != nil {
			return nil, fmt.Errorf("%s reserve enumeration: %w", market.Label, err)
		}
		if len(listResult) != 1 {
			return nil, fmt.Errorf("%s reserve enumeration returned %d fields", market.Label, len(listResult))
		}
		reserves, err := decodeAaveReserves(listResult[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", market.Label, err)
		}
		if len(reserves) > 256 {
			return nil, fmt.Errorf("%s reserve count %d exceeds safety bound", market.Label, len(reserves))
		}

		calls := make([]ContractCall, 0, len(reserves)*2)
		for _, reserve := range reserves {
			calls = append(calls,
				ContractCall{
					Contract: market.DataProvider,
					ABI:      aaveDataProviderABI,
					Method:   "getUserReserveData",
					Args:     []any{reserve.TokenAddress, account},
				},
				ContractCall{
					Contract: reserve.TokenAddress,
					ABI:      erc20ABI,
					Method:   "decimals",
				},
			)
		}
		results, err := client.ParallelCalls(ctx, block, calls)
		if err != nil {
			return nil, fmt.Errorf("%s user reserves: %w", market.Label, err)
		}
		components := make([]Component, 0)
		for index, reserve := range reserves {
			userData := results[index*2]
			decimalsData := results[index*2+1]
			supply, err := BigIntAt(userData, 0)
			if err != nil {
				return nil, fmt.Errorf("%s %s supply: %w", market.Label, reserve.Symbol, err)
			}
			stableDebt, err := BigIntAt(userData, 1)
			if err != nil {
				return nil, fmt.Errorf("%s %s stable debt: %w", market.Label, reserve.Symbol, err)
			}
			variableDebt, err := BigIntAt(userData, 2)
			if err != nil {
				return nil, fmt.Errorf("%s %s variable debt: %w", market.Label, reserve.Symbol, err)
			}
			collateral, err := BoolAt(userData, 8)
			if err != nil {
				return nil, fmt.Errorf("%s %s collateral: %w", market.Label, reserve.Symbol, err)
			}
			decimals, err := Uint8At(decimalsData, 0)
			if err != nil {
				return nil, fmt.Errorf("%s %s decimals are invalid", market.Label, reserve.Symbol)
			}
			token := Token{
				ChainID:  block.ChainID,
				Address:  reserve.TokenAddress,
				Symbol:   reserve.Symbol,
				Decimals: decimals,
			}
			if supply.Sign() > 0 {
				component := NewComponent(
					"asset",
					token,
					supply,
					Source{Contract: market.DataProvider, Method: "getUserReserveData.currentATokenBalance"},
				)
				component.Metadata = map[string]any{"collateral": collateral}
				components = append(components, component)
			}
			if stableDebt.Sign() > 0 {
				component := NewComponent(
					"debt",
					token,
					stableDebt,
					Source{Contract: market.DataProvider, Method: "getUserReserveData.currentStableDebt"},
				)
				component.Metadata = map[string]any{"debtType": "stable"}
				components = append(components, component)
			}
			if variableDebt.Sign() > 0 {
				component := NewComponent(
					"debt",
					token,
					variableDebt,
					Source{Contract: market.DataProvider, Method: "getUserReserveData.currentVariableDebt"},
				)
				component.Metadata = map[string]any{"debtType": "variable"}
				components = append(components, component)
			}
		}
		// Reward support is independently gated because controller deployments do
		// not necessarily start at the same block as their corresponding pools.
		rewardDeployment, rewardExists := aaveV2RewardDeployments[block.ChainID]
		if a.Info().ID == "aave-v2" && rewardExists &&
			rewardDeployment.ActiveAt(block.Number) {
			incentiveAssets, rewardErr := readAaveIncentiveAssets(
				ctx,
				client,
				block,
				market.DataProvider,
				reserves,
				false,
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s incentive assets: %w", market.Label, rewardErr)
			}
			rewardResult, rewardErr := client.Call(
				ctx,
				block,
				rewardDeployment.Controller,
				aaveV2RewardsControllerABI,
				"getRewardsBalance",
				incentiveAssets,
				account,
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", market.Label, rewardErr)
			}
			rewardAmount, rewardErr := BigIntAt(rewardResult, 0)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", market.Label, rewardErr)
			}
			if rewardAmount.Sign() > 0 {
				components = append(components, NewComponent(
					"reward",
					rewardDeployment.Token,
					rewardAmount,
					Source{
						Contract: rewardDeployment.Controller,
						Method:   "getRewardsBalance",
					},
				))
			}
		}
		if a.Info().ID == "spark" && block.ChainID == Ethereum &&
			ethereumSparkRewardsDeployment.ActiveAt(block.Number) {
			incentiveAssets, rewardErr := readAaveIncentiveAssets(
				ctx,
				client,
				block,
				market.DataProvider,
				reserves,
				false,
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s incentive assets: %w", market.Label, rewardErr)
			}
			rewardComponents, rewardErr := readSparkRewardComponents(
				ctx,
				client,
				block,
				incentiveAssets,
				account,
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", market.Label, rewardErr)
			}
			components = append(components, rewardComponents...)
		}
		if len(components) > 0 {
			marketID := market.DataProvider
			if market.Pool != (common.Address{}) {
				marketID = market.Pool
			}
			groups = append(groups, Group{
				ID:             strings.ToLower(marketID.Hex()),
				MarketID:       strings.ToLower(marketID.Hex()),
				Label:          "Lending · " + market.Label,
				Components:     components,
				NetValuePolicy: "floor-zero",
				Metadata: map[string]any{
					"dataProvider": market.DataProvider,
					"pool":         market.Pool,
				},
			})
		}
	}
	if a.Info().ID == "aave-v2" && block.ChainID == Ethereum {
		safetyGroups, err := readAaveSafetyModulePositions(
			ctx,
			client,
			block,
			account,
			ethereumAaveV2SafetyModules,
		)
		if err != nil {
			return nil, fmt.Errorf("safety modules: %w", err)
		}
		groups = append(groups, safetyGroups...)
	}
	if len(a.vaults[block.ChainID]) > 0 {
		vaultGroups, err := readVaultPositions(
			ctx,
			client,
			block,
			account,
			a.vaults[block.ChainID],
		)
		if err != nil {
			return nil, fmt.Errorf("Spark Savings: %w", err)
		}
		groups = append(groups, vaultGroups...)
	}
	if a.Info().ID == "aave-v3" && block.ChainID == Ethereum {
		safetyGroups, err := readAaveSafetyModulePositions(
			ctx,
			client,
			block,
			account,
			ethereumAaveV3SafetyModules,
		)
		if err != nil {
			return nil, fmt.Errorf("safety modules: %w", err)
		}
		groups = append(groups, safetyGroups...)

		umbrellaGroups, err := readAaveUmbrellaPositions(
			ctx,
			client,
			block,
			account,
			ethereumAaveV3Umbrella,
		)
		if err != nil {
			return nil, fmt.Errorf("Umbrella: %w", err)
		}
		groups = append(groups, umbrellaGroups...)

		vaultGroups, err := readVaultPositions(
			ctx,
			client,
			block,
			account,
			[]vaultConfig{{
				ID:      "vault:0xe1753f2e00940cc31213dd92013cf019dfe4ca1d",
				Label:   "Yield · sGHO",
				Address: common.HexToAddress("0xE1753F2e00940cC31213dd92013cF019DFE4ca1d"),
				Asset: token(
					Ethereum,
					"0x40D16FC0246aD3160Ccc09B8D0D3A2cD28aE6C2f",
					"GHO",
					18,
				),
				ActivationBlock: 25_028_623,
			}},
		)
		if err != nil {
			return nil, fmt.Errorf("sGHO vault: %w", err)
		}
		groups = append(groups, vaultGroups...)

		stataGroups, err := readAaveStataPositions(ctx, client, block, account)
		if err != nil {
			return nil, fmt.Errorf("stata tokens: %w", err)
		}
		groups = append(groups, stataGroups...)
	}
	return groups, nil
}

func aaveMarketAt(
	label string,
	pool string,
	dataProvider string,
	activationBlock uint64,
	deactivationBlock uint64,
) aaveMarket {
	return aaveMarket{
		deploymentWindow: deploymentWindow{
			ActivationBlock:   activationBlock,
			DeactivationBlock: deactivationBlock,
		},
		Label:        label,
		Pool:         common.HexToAddress(pool),
		DataProvider: common.HexToAddress(dataProvider),
	}
}

func aaveAdapters() []Adapter {
	return []Adapter{
		NewAaveAdapter("aave-v3", "Aave v3", map[ChainID][]aaveMarket{
			Ethereum: {
				aaveMarketAt("Core", "0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2", "0x7B4EB56E7CD4b454BA8ff71E4518426369a138a3", 16_291_078, 20_261_938),
				aaveMarketAt("Core", "0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2", "0x20e074F62EcBD8BC5E38211adCb6103006113A22", 20_261_939, 20_827_146),
				aaveMarketAt("Core", "0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2", "0x41393e5e337606dc3821075Af65AeE84D7688CBD", 20_827_147, 21_780_616),
				aaveMarketAt("Core", "0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2", "0x497a1994c46d4f6C864904A9f1fac6328Cb7C8a6", 21_780_617, 22_686_777),
				aaveMarketAt("Core", "0x87870Bca3F3fD6335C3F4ce8392D69350B4fA4E2", "0x0a16f2FCC0D44FaE41cc54e079281D84A363bECD", 22_686_778, 0),
				aaveMarketAt("Prime", "0x4e033931ad43597d96D6bcc25c280717730B58B1", "0x66FeAe868EBEd74A34A7043e88742AAE00D2bC53", 21_780_567, 22_686_914),
				aaveMarketAt("Prime", "0x4e033931ad43597d96D6bcc25c280717730B58B1", "0xB85B2bFEbeC4F5f401dbf92ac147A3076391fCD5", 22_686_915, 0),
				aaveMarketAt("EtherFi", "0x0AA97c284e98396202b6A04024F5E2c65026F3c0", "0x8Cb4b66f7B13F2Ae4D3c91338fC007dbF8C14208", 20_625_515, 20_830_503),
				aaveMarketAt("EtherFi", "0x0AA97c284e98396202b6A04024F5E2c65026F3c0", "0xE7d490885A68f00d9886508DF281D67263ed5758", 20_830_504, 21_780_262),
				aaveMarketAt("EtherFi", "0x0AA97c284e98396202b6A04024F5E2c65026F3c0", "0xECdA3F25B73261d1FdFa1E158967660AA29f00cC", 21_780_263, 22_686_935),
				aaveMarketAt("EtherFi", "0x0AA97c284e98396202b6A04024F5E2c65026F3c0", "0x7c8509591f9693D21280d96e149a08A3bf69Cd0c", 22_686_936, 0),
				aaveMarketAt("Horizon", "0xAe05Cd22df81871bc7cC2a04BeCfb516bFe332C8", "0x53519c32f73fE1797d10210c4950fFeBa3b21504", 23_125_531, 0),
			},
			BSC: {
				aaveMarketAt("Core", "0x6807dc923806fE8Fd134338EABCA509979a7e0cB", "0x1e26247502e90b4fab9D0d17e4775e90085D2A35", 46_367_909, 51_262_444),
				aaveMarketAt("Core", "0x6807dc923806fE8Fd134338EABCA509979a7e0cB", "0xc90Df74A7c16245c5F5C5870327Ceb38Fe5d5328", 51_262_445, 0),
			},
			Base: {
				aaveMarketAt("Core", "0xA238Dd80C259a72e81d7e4664a9801593F98d1c5", "0xC4Fcf9893072d61Cc2899C0054877Cb752587981", 25_954_709, 31_377_574),
				aaveMarketAt("Core", "0xA238Dd80C259a72e81d7e4664a9801593F98d1c5", "0x0F43731EB8d45A581f4a36DD74F5f358bc90C73A", 31_377_575, 0),
			},
			Arbitrum: {
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", 302_650_382, 345_855_960),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", 345_855_961, 0),
			},
			Polygon: {
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", 25_826_028, 41_174_631),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x9441B65EE553F70df9C77d45d3283B6BC24F222d", 41_174_632, 59_108_788),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", 59_108_789, 62_249_156),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7F23D86Ee20D869112572136221e173428DD740B", 62_249_157, 67_532_798),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", 67_532_799, 72_592_540),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", 72_592_541, 0),
			},
			Monad: {
				aaveMarketAt("Core", "0x69a5F9AD4f96ebf0a0C792dD42a01cC5C0102fef", "0xB65A68B98274ef7D9a60E0C0747dD1BEc3D32fad", 81_909_763, 0),
			},
			Plasma: {
				aaveMarketAt("Core", "0x925a2A7214Ed92428B5b1B090F80b25700095e12", "0xf2D6E38B407e31E7E7e4a16E6769728b76c7419F", 489_197, 0),
			},
			Avalanche: {
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", 11_970_506, 28_384_510),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x50ddd0Cd4266299527d25De9CBb55fE0EB8dAc30", 28_384_511, 47_712_700),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", 47_712_701, 50_972_229),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7F23D86Ee20D869112572136221e173428DD740B", 50_972_230, 56_836_940),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", 56_836_941, 63_632_022),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", 63_632_023, 0),
			},
			Optimism: {
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", 4_365_693, 86_483_662),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0xd9Ca4878dd38B021583c1B669905592EAe76E044", 86_483_663, 122_423_343),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", 122_423_344, 125_827_825),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x7F23D86Ee20D869112572136221e173428DD740B", 125_827_826, 131_542_951),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", 131_542_952, 136_976_691),
				aaveMarketAt("Core", "0x794a61358D6845594F94dc1DB02A252b5b4814aD", "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", 136_976_692, 0),
			},
		}),
		NewAaveAdapter("aave-v2", "Aave v2", map[ChainID][]aaveMarket{
			Ethereum: {
				aaveMarketAt("Core", "0x7d2768dE32b0b80b7a3454c06BdAc94A69DdC7A9", "0x057835Ad21a177dbdd3090bB1CAE03EaCF78Fc6d", 11_362_589, 0),
			},
			Polygon: {
				aaveMarketAt("Core", "0x8dFf5E27EA6b7AC08EbFdf9eB090F32ee9a30fcf", "0x7551b5D2763519d4e37E8B81929D336De671d46d", 12_687_302, 0),
			},
			Avalanche: {
				aaveMarketAt("Core", "0x4F01AeD16D97E3aB5ab2B501154DC9bb0F1A5A2C", "0x65285E9dfab318f57051ab2b139ccCf232945451", 4_607_174, 0),
			},
		}),
		newAaveAdapter("spark", "Spark", map[ChainID][]aaveMarket{
			Ethereum: {
				aaveMarketAt("Core", "0xC13e21B648A5Ee794902342038FF3aDAB66BE987", "0xFc21d6d146E6086B8359705C8b28512a983db0cb", 16_776_391, 0),
			},
		}, sparkSavingsVaults),
		NewAaveAdapter("kinza", "Kinza", map[ChainID][]aaveMarket{
			BSC: {
				aaveMarketAt("Core", "", "0x09Ddc4AE826601b0F9671b9edffDf75e7E6f5D61", 29_232_063, 0),
			},
		}),
		NewAaveAdapter("seamless", "Seamless", map[ChainID][]aaveMarket{
			Base: {
				aaveMarketAt("Core", "", "0x2A0979257105834789bC6b9E1B00446DFbA8dFBa", 3_318_562, 0),
			},
		}),
	}
}
