package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var cometABI = MustABI(`[
  {"type":"function","name":"baseToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"numAssets","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
  {
    "type":"function",
    "name":"getAssetInfo",
    "stateMutability":"view",
    "inputs":[{"name":"i","type":"uint8"}],
    "outputs":[{"type":"tuple","components":[
      {"name":"offset","type":"uint8"},
      {"name":"asset","type":"address"},
      {"name":"priceFeed","type":"address"},
      {"name":"scale","type":"uint64"},
      {"name":"borrowCollateralFactor","type":"uint64"},
      {"name":"liquidateCollateralFactor","type":"uint64"},
      {"name":"liquidationFactor","type":"uint64"},
      {"name":"supplyCap","type":"uint128"}
    ]}]
  },
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"borrowBalanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"collateralBalanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"asset","type":"address"}],"outputs":[{"type":"uint128"}]}
]`)

var cometRewardsABI = MustABI(`[
  {
    "type":"function",
    "name":"rewardConfig",
    "stateMutability":"view",
    "inputs":[{"name":"comet","type":"address"}],
    "outputs":[{"type":"tuple","components":[
      {"name":"token","type":"address"},
      {"name":"rescaleFactor","type":"uint64"},
      {"name":"shouldUpscale","type":"bool"}
    ]}]
  },
  {
    "type":"function",
    "name":"getRewardOwed",
    "stateMutability":"nonpayable",
    "inputs":[{"name":"comet","type":"address"},{"name":"account","type":"address"}],
    "outputs":[{"type":"tuple","components":[
      {"name":"token","type":"address"},
      {"name":"owed","type":"uint256"}
    ]}]
  }
]`)

type cometAssetInfo struct {
	Offset                    uint8
	Asset                     common.Address
	PriceFeed                 common.Address
	Scale                     uint64
	BorrowCollateralFactor    uint64
	LiquidateCollateralFactor uint64
	LiquidationFactor         uint64
	SupplyCap                 *big.Int
}

type cometMarket struct {
	deploymentWindow
	Label                  string
	Comet                  common.Address
	Rewards                common.Address
	RewardsActivationBlock uint64
}

func (m cometMarket) RewardsActiveAt(block uint64) bool {
	return m.Rewards != (common.Address{}) && block >= m.RewardsActivationBlock
}

type cometRewardConfig struct {
	Token         common.Address
	RescaleFactor uint64
	ShouldUpscale bool
}

type cometRewardOwed struct {
	Token common.Address
	Owed  *big.Int
}

type CompoundV3Adapter struct {
	adapterBase
	markets map[ChainID][]cometMarket
}

func (a *CompoundV3Adapter) activeMarkets(chainID ChainID, block uint64) []cometMarket {
	active := make([]cometMarket, 0, len(a.markets[chainID]))
	for _, market := range a.markets[chainID] {
		if market.ActiveAt(block) {
			active = append(active, market)
		}
	}
	return active
}

func decodeCometAsset(value any) (cometAssetInfo, error) {
	converted := abi.ConvertType(value, new(cometAssetInfo))
	info, ok := converted.(*cometAssetInfo)
	if !ok || info == nil {
		return cometAssetInfo{}, fmt.Errorf("unexpected Comet asset info type %T", value)
	}
	return *info, nil
}

func decodeCometRewardConfig(value any) (cometRewardConfig, error) {
	converted := abi.ConvertType(value, new(cometRewardConfig))
	config, ok := converted.(*cometRewardConfig)
	if !ok || config == nil {
		return cometRewardConfig{}, fmt.Errorf("unexpected Comet reward config type %T", value)
	}
	return *config, nil
}

func decodeCometRewardOwed(value any) (cometRewardOwed, error) {
	converted := abi.ConvertType(value, new(cometRewardOwed))
	owed, ok := converted.(*cometRewardOwed)
	if !ok || owed == nil || owed.Owed == nil {
		return cometRewardOwed{}, fmt.Errorf("unexpected Comet reward owed type %T", value)
	}
	return *owed, nil
}

func readToken(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	address common.Address,
) (Token, error) {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: address, ABI: erc20ABI, Method: "decimals"},
	})
	if err != nil {
		return Token{}, err
	}
	symbolRow, stringCallErr := client.Call(ctx, block, address, erc20ABI, "symbol")
	symbol := ""
	if stringCallErr == nil {
		symbol, stringCallErr = StringAt(symbolRow, 0)
	}
	if stringCallErr != nil || symbol == "" {
		bytesRow, bytesCallErr := client.Call(
			ctx,
			block,
			address,
			erc20Bytes32SymbolABI,
			"symbol",
		)
		if bytesCallErr != nil {
			return Token{}, fmt.Errorf(
				"read token symbol as string (%v) or bytes32: %w",
				stringCallErr,
				bytesCallErr,
			)
		}
		symbol, err = Bytes32StringAt(bytesRow, 0)
		if err != nil {
			return Token{}, fmt.Errorf("decode token symbol bytes32: %w", err)
		}
		if symbol == "" {
			return Token{}, fmt.Errorf("decode token symbol bytes32: empty symbol")
		}
	}
	decimals, err := Uint8At(rows[0], 0)
	if err != nil {
		return Token{}, err
	}
	return Token{ChainID: block.ChainID, Address: address, Symbol: symbol, Decimals: decimals}, nil
}

func (a *CompoundV3Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0)
	for _, market := range a.activeMarkets(block.ChainID, block.Number) {
		header, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: market.Comet, ABI: cometABI, Method: "baseToken"},
			{Contract: market.Comet, ABI: cometABI, Method: "numAssets"},
		})
		if err != nil {
			return nil, fmt.Errorf("%s market metadata: %w", market.Label, err)
		}
		baseAddress, err := AddressAt(header[0], 0)
		if err != nil {
			return nil, err
		}
		assetCount, err := Uint8At(header[1], 0)
		if err != nil {
			return nil, err
		}
		if assetCount > 32 {
			return nil, fmt.Errorf("%s collateral count %d exceeds safety bound", market.Label, assetCount)
		}
		assetCalls := make([]ContractCall, assetCount)
		for index := uint8(0); index < assetCount; index++ {
			assetCalls[index] = ContractCall{
				Contract: market.Comet,
				ABI:      cometABI,
				Method:   "getAssetInfo",
				Args:     []any{index},
			}
		}
		assetRows, err := client.ParallelCalls(ctx, block, assetCalls)
		if err != nil {
			return nil, fmt.Errorf("%s collateral metadata: %w", market.Label, err)
		}
		collaterals := make([]common.Address, 0, assetCount)
		for _, row := range assetRows {
			if len(row) != 1 {
				return nil, fmt.Errorf("%s getAssetInfo returned %d fields", market.Label, len(row))
			}
			info, err := decodeCometAsset(row[0])
			if err != nil {
				return nil, err
			}
			collaterals = append(collaterals, info.Asset)
		}

		positionCalls := []ContractCall{
			{Contract: market.Comet, ABI: cometABI, Method: "balanceOf", Args: []any{account}},
			{Contract: market.Comet, ABI: cometABI, Method: "borrowBalanceOf", Args: []any{account}},
		}
		for _, collateral := range collaterals {
			positionCalls = append(positionCalls, ContractCall{
				Contract: market.Comet,
				ABI:      cometABI,
				Method:   "collateralBalanceOf",
				Args:     []any{account, collateral},
			})
		}
		positionRows, err := client.ParallelCalls(ctx, block, positionCalls)
		if err != nil {
			return nil, fmt.Errorf("%s balances: %w", market.Label, err)
		}
		components := make([]Component, 0)
		supply, err := BigIntAt(positionRows[0], 0)
		if err != nil {
			return nil, err
		}
		debt, err := BigIntAt(positionRows[1], 0)
		if err != nil {
			return nil, err
		}
		collateralAmounts := make([]*big.Int, len(collaterals))
		hasPosition := supply.Sign() > 0 || debt.Sign() > 0
		for index := range collaterals {
			amount, err := BigIntAt(positionRows[index+2], 0)
			if err != nil {
				return nil, err
			}
			collateralAmounts[index] = amount
			hasPosition = hasPosition || amount.Sign() > 0
		}
		rewardOwed := cometRewardOwed{Owed: new(big.Int)}
		if market.RewardsActiveAt(block.Number) {
			rewardRows, rewardErr := client.ParallelCalls(ctx, block, []ContractCall{
				{
					Contract: market.Rewards,
					ABI:      cometRewardsABI,
					Method:   "rewardConfig",
					Args:     []any{market.Comet},
				},
				{
					Contract: market.Rewards,
					ABI:      cometRewardsABI,
					Method:   "getRewardOwed",
					Args:     []any{market.Comet, account},
				},
			})
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", market.Label, rewardErr)
			}
			if len(rewardRows[0]) != 1 || len(rewardRows[1]) != 1 {
				return nil, fmt.Errorf("%s reward tuple shape changed", market.Label)
			}
			rewardConfig, decodeErr := decodeCometRewardConfig(rewardRows[0][0])
			if decodeErr != nil {
				return nil, fmt.Errorf("%s reward config: %w", market.Label, decodeErr)
			}
			if rewardConfig.Token != (common.Address{}) {
				rewardOwed, decodeErr = decodeCometRewardOwed(rewardRows[1][0])
				if decodeErr != nil {
					return nil, fmt.Errorf("%s reward owed: %w", market.Label, decodeErr)
				}
				if rewardOwed.Owed.Sign() > 0 {
					if rewardOwed.Token == (common.Address{}) {
						return nil, fmt.Errorf("%s reward owed returned zero token", market.Label)
					}
					if rewardOwed.Token != rewardConfig.Token {
						return nil, fmt.Errorf(
							"%s reward token changed from %s to %s",
							market.Label,
							rewardConfig.Token,
							rewardOwed.Token,
						)
					}
					hasPosition = true
				}
			}
		}
		if !hasPosition {
			continue
		}

		tokenAddresses := make([]common.Address, 0, len(collaterals)+1)
		if supply.Sign() > 0 || debt.Sign() > 0 {
			tokenAddresses = append(tokenAddresses, baseAddress)
		}
		for index, collateral := range collaterals {
			if collateralAmounts[index].Sign() > 0 {
				tokenAddresses = append(tokenAddresses, collateral)
			}
		}
		if rewardOwed.Owed.Sign() > 0 {
			tokenAddresses = append(tokenAddresses, rewardOwed.Token)
		}
		tokens := make(map[common.Address]Token, len(tokenAddresses))
		var tokenMutex sync.Mutex
		var tokenWait sync.WaitGroup
		tokenErrors := make([]error, len(tokenAddresses))
		for index, tokenAddress := range tokenAddresses {
			tokenWait.Add(1)
			go func(index int, tokenAddress common.Address) {
				defer tokenWait.Done()
				token, err := readToken(ctx, client, block, tokenAddress)
				if err != nil {
					tokenErrors[index] = err
					return
				}
				tokenMutex.Lock()
				tokens[tokenAddress] = token
				tokenMutex.Unlock()
			}(index, tokenAddress)
		}
		tokenWait.Wait()
		for index, err := range tokenErrors {
			if err != nil {
				return nil, fmt.Errorf("%s token %s: %w", market.Label, tokenAddresses[index], err)
			}
		}

		if supply.Sign() > 0 {
			component := NewComponent(
				"asset",
				tokens[baseAddress],
				supply,
				Source{Contract: market.Comet, Method: "balanceOf"},
			)
			component.Metadata = map[string]any{"role": "base-supply"}
			components = append(components, component)
		}
		if debt.Sign() > 0 {
			component := NewComponent(
				"debt",
				tokens[baseAddress],
				debt,
				Source{Contract: market.Comet, Method: "borrowBalanceOf"},
			)
			component.Metadata = map[string]any{"role": "base-borrow"}
			components = append(components, component)
		}
		for index := range collaterals {
			amount := collateralAmounts[index]
			if amount.Sign() == 0 {
				continue
			}
			component := NewComponent(
				"asset",
				tokens[collaterals[index]],
				amount,
				Source{Contract: market.Comet, Method: "collateralBalanceOf"},
			)
			component.Metadata = map[string]any{"role": "collateral"}
			components = append(components, component)
		}
		if rewardOwed.Owed.Sign() > 0 {
			component := NewComponent(
				"reward",
				tokens[rewardOwed.Token],
				rewardOwed.Owed,
				Source{Contract: market.Rewards, Method: "getRewardOwed"},
			)
			component.Metadata = map[string]any{"role": "market-reward"}
			components = append(components, component)
		}
		if len(components) > 0 {
			groups = append(groups, Group{
				ID:             common.Bytes2Hex(market.Comet.Bytes()),
				Label:          market.Label + " market",
				Components:     components,
				NetValuePolicy: "floor-zero",
				Metadata:       map[string]any{"comet": market.Comet},
			})
		}
	}
	return groups, nil
}

func newCompoundV3Adapter() Adapter {
	market := func(
		label string,
		address string,
		rewards string,
		activationBlock uint64,
		rewardsActivationBlock uint64,
	) cometMarket {
		return cometMarket{
			deploymentWindow:       deploymentWindow{ActivationBlock: activationBlock},
			Label:                  label,
			Comet:                  common.HexToAddress(address),
			Rewards:                common.HexToAddress(rewards),
			RewardsActivationBlock: rewardsActivationBlock,
		}
	}
	const ethereumRewards = "0x1B0e765F6224C21223AeA2af16c1C46E38885a40"
	const baseRewards = "0x123964802e6ABabBE1Bc9547D72Ef1B69B00A6b1"
	const arbitrumRewards = "0x88730d254A2f7e6AC8388c3198aFd694bA9f7fae"
	const polygonRewards = "0x45939657d1CA34A8FA39A924B71D28Fe8431e581"
	const optimismRewards = "0x443EA0340cb75a160F31A440722dec7b5bc3C2E9"
	const ethereumRewardsActivation = 15_331_591
	const baseRewardsActivation = 2_197_596
	const arbitrumRewardsActivation = 87_335_253
	markets := map[ChainID][]cometMarket{
		Ethereum: {
			market("USDC", "0xc3d688B66703497DAA19211EEdff47f25384cdc3", ethereumRewards, 15_331_586, ethereumRewardsActivation),
			market("USDS", "0x5D409e56D886231aDAf00c8775665AD0f9897b56", ethereumRewards, 20_987_551, ethereumRewardsActivation),
			market("USDT", "0x3Afdc9BCA9213A35503b077a6072F3D0d5AB0840", ethereumRewards, 20_190_637, ethereumRewardsActivation),
			market("WBTC", "0xe85Dc543813B8c2CFEaAc371517b925a166a9293", ethereumRewards, 21_820_087, ethereumRewardsActivation),
			market("WETH", "0xA17581A9E3356d9A858b789D68B4d866e593aE94", ethereumRewards, 16_400_710, ethereumRewardsActivation),
			market("wstETH", "0x3D0bb1ccaB520A66e607822fC55BC921738fAFE3", ethereumRewards, 20_683_535, ethereumRewardsActivation),
		},
		Base: {
			market("AERO", "0x784efeB622244d2348d4F2522f8860B96fbEcE89", baseRewards, 20_852_405, baseRewardsActivation),
			market("USDbC", "0x9c4ec768c28520B50860ea7a15bd7213a9fF58bf", baseRewards, 2_197_588, baseRewardsActivation),
			market("USDC", "0xb125E6687d4313864e53df431d5425969c15Eb2F", baseRewards, 11_699_480, baseRewardsActivation),
			market("USDS", "0x2c776041CCFe903071AF44aa147368a9c8EEA518", baseRewards, 26_046_502, baseRewardsActivation),
			market("WETH", "0x46e6b214b524310239732D51387075E0e70970bf", baseRewards, 2_495_303, baseRewardsActivation),
		},
		Arbitrum: {
			market("USDC.e", "0xA5EDBDD9646f8dFF606d7448e414884C7d905dCA", arbitrumRewards, 87_335_214, arbitrumRewardsActivation),
			market("USDC", "0x9c4ec768c28520B50860ea7a15bd7213a9fF58bf", arbitrumRewards, 122_080_500, arbitrumRewardsActivation),
			market("USDT", "0xd98Be00b5D27fc98112BdE293e487f8D4cA57d07", arbitrumRewards, 223_796_350, arbitrumRewardsActivation),
			market("WETH", "0x6f7D514bbD4aFf3BcD1140B7344b32f063dEe486", arbitrumRewards, 219_386_101, arbitrumRewardsActivation),
		},
		Polygon: {
			market("USDC.e", "0xF25212E676D1F7F89Cd72fFEe66158f541246445", polygonRewards, 39_412_367, 39_413_527),
			market("USDT", "0xaeB318360f27748Acb200CE616E389A6C9409a07", polygonRewards, 58_479_907, 58_793_297),
		},
		Optimism: {
			market("USDC", "0x2e44e174f7D53F0212823acC11C01A11d58c5bCB", optimismRewards, 118_406_276, 118_840_983),
			market("USDT", "0x995E394b8B2437aC8Ce61Ee0bC610D617962B214", optimismRewards, 120_295_564, 121_727_936),
			market("WETH", "0xE36A30D249f7761327fd973001A32010b521b6Fd", optimismRewards, 122_730_232, 123_072_627),
		},
	}
	return &CompoundV3Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID:     "compound-v3",
			Name:   "Compound III",
			Chains: deploymentChains(markets),
		}},
		markets: markets,
	}
}
