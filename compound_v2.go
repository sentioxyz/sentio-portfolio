package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var comptrollerABI = MustABI(`[
  {"type":"function","name":"getAllMarkets","stateMutability":"view","inputs":[],"outputs":[{"type":"address[]"}]}
]`)

var cTokenABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"exchangeRateStored","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"borrowBalanceStored","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"underlying","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var compoundLensABI = MustABI(`[
  {
    "type":"function",
    "name":"getCompBalanceMetadataExt",
    "stateMutability":"view",
    "inputs":[
      {"name":"comp","type":"address"},
      {"name":"comptroller","type":"address"},
      {"name":"account","type":"address"}
    ],
    "outputs":[{"name":"metadata","type":"tuple","components":[
      {"name":"balance","type":"uint256"},
      {"name":"votes","type":"uint256"},
      {"name":"delegate","type":"address"},
      {"name":"allocated","type":"uint256"}
    ]}]
  }
]`)

var compoundMultiRewardDistributorABI = MustABI(`[
  {
    "type":"function",
    "name":"getOutstandingRewardsForUser",
    "stateMutability":"view",
    "inputs":[{"name":"user","type":"address"}],
    "outputs":[{"name":"outstanding","type":"tuple[]","components":[
      {"name":"mToken","type":"address"},
      {"name":"rewards","type":"tuple[]","components":[
        {"name":"emissionToken","type":"address"},
        {"name":"totalAmount","type":"uint256"},
        {"name":"supplySide","type":"uint256"},
        {"name":"borrowSide","type":"uint256"}
      ]}
    ]}]
  }
]`)

var compoundStakingModuleABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"}]},
  {"type":"function","name":"STAKED_TOKEN","stateMutability":"view","inputs":[],"outputs":[{"name":"token","type":"address"}]},
  {"type":"function","name":"getTotalRewardsBalance","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"}]},
  {"type":"function","name":"REWARD_TOKEN","stateMutability":"view","inputs":[],"outputs":[{"name":"token","type":"address"}]}
]`)

type compoundRewardMetadata struct {
	Balance   *big.Int
	Votes     *big.Int
	Delegate  common.Address
	Allocated *big.Int
}

type compoundOutstandingReward struct {
	EmissionToken common.Address
	TotalAmount   *big.Int
	SupplySide    *big.Int
	BorrowSide    *big.Int
}

type compoundOutstandingMarketRewards struct {
	MToken  common.Address
	Rewards []compoundOutstandingReward
}

type compoundStakingModule struct {
	Module         common.Address
	IncludeRewards bool
	Window         availabilityWindow
}

type compoundV2Deployment struct {
	Comptroller            common.Address
	ComptrollerWindow      availabilityWindow
	WrappedNative          Token
	NativeMarkets          map[common.Address]struct{}
	RewardLens             common.Address
	RewardLensWindow       availabilityWindow
	RewardToken            Token
	MultiRewardDistributor common.Address
	MultiRewardWindow      availabilityWindow
	StakingModules         []compoundStakingModule
}

type CompoundV2Adapter struct {
	adapterBase
	deployments map[ChainID]compoundV2Deployment
}

func NewCompoundV2Adapter(
	id string,
	name string,
	deployments map[ChainID]compoundV2Deployment,
) *CompoundV2Adapter {
	chains := make([]ChainID, 0, len(deployments))
	for _, chainID := range SupportedChainIDs {
		if _, ok := deployments[chainID]; ok {
			chains = append(chains, chainID)
		}
	}
	return &CompoundV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: id, Name: name, Chains: chains}},
		deployments: deployments,
	}
}

func decodeAddresses(value any) ([]common.Address, error) {
	converted := abi.ConvertType(value, new([]common.Address))
	addresses, ok := converted.(*[]common.Address)
	if !ok || addresses == nil {
		return nil, fmt.Errorf("unexpected address list type %T", value)
	}
	return *addresses, nil
}

func compoundMultiRewardGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	distributor common.Address,
	account common.Address,
) (*Group, error) {
	result, err := client.Call(
		ctx,
		block,
		distributor,
		compoundMultiRewardDistributorABI,
		"getOutstandingRewardsForUser",
		account,
	)
	if err != nil {
		return nil, err
	}
	if len(result) != 1 {
		return nil, fmt.Errorf("getOutstandingRewardsForUser returned %d fields", len(result))
	}
	converted := abi.ConvertType(result[0], new([]compoundOutstandingMarketRewards))
	rows, ok := converted.(*[]compoundOutstandingMarketRewards)
	if !ok || rows == nil {
		return nil, fmt.Errorf("unexpected outstanding rewards type %T", result[0])
	}

	totals := make(map[common.Address]*big.Int)
	for _, market := range *rows {
		for _, reward := range market.Rewards {
			if reward.TotalAmount == nil || reward.TotalAmount.Sign() <= 0 {
				continue
			}
			if _, exists := totals[reward.EmissionToken]; !exists {
				totals[reward.EmissionToken] = new(big.Int)
			}
			totals[reward.EmissionToken].Add(totals[reward.EmissionToken], reward.TotalAmount)
		}
	}
	addresses := make([]common.Address, 0, len(totals))
	for address := range totals {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool {
		return addresses[i].Hex() < addresses[j].Hex()
	})

	components := make([]Component, 0, len(addresses))
	for _, address := range addresses {
		rewardToken, tokenErr := readToken(ctx, client, block, address)
		if tokenErr != nil {
			return nil, fmt.Errorf("%s reward metadata: %w", address, tokenErr)
		}
		components = append(components, NewComponent(
			"reward",
			rewardToken,
			totals[address],
			Source{Contract: distributor, Method: "getOutstandingRewardsForUser"},
		))
	}
	if len(components) == 0 {
		return nil, nil
	}
	return &Group{
		ID:         "rewards",
		Label:      "Rewards",
		Components: components,
		Metadata:   map[string]any{"distributor": distributor},
	}, nil
}

func compoundStakingGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	modules []compoundStakingModule,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0, len(modules))
	for _, module := range modules {
		if !module.Window.ActiveAt(block.Number) {
			continue
		}
		balanceResult, err := client.Call(
			ctx,
			block,
			module.Module,
			compoundStakingModuleABI,
			"balanceOf",
			account,
		)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", module.Module, err)
		}
		stakedAmount, err := BigIntAt(balanceResult, 0)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", module.Module, err)
		}

		rewardAmount := new(big.Int)
		if module.IncludeRewards {
			rewardResult, rewardErr := client.Call(
				ctx,
				block,
				module.Module,
				compoundStakingModuleABI,
				"getTotalRewardsBalance",
				account,
			)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", module.Module, rewardErr)
			}
			rewardAmount, rewardErr = BigIntAt(rewardResult, 0)
			if rewardErr != nil {
				return nil, fmt.Errorf("%s rewards: %w", module.Module, rewardErr)
			}
		}
		if stakedAmount.Sign() == 0 && rewardAmount.Sign() == 0 {
			continue
		}

		components := make([]Component, 0, 2)
		if stakedAmount.Sign() > 0 {
			tokenResult, tokenErr := client.Call(
				ctx,
				block,
				module.Module,
				compoundStakingModuleABI,
				"STAKED_TOKEN",
			)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s staked token: %w", module.Module, tokenErr)
			}
			tokenAddress, tokenErr := AddressAt(tokenResult, 0)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s staked token: %w", module.Module, tokenErr)
			}
			stakedToken, tokenErr := readToken(ctx, client, block, tokenAddress)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s staked token metadata: %w", module.Module, tokenErr)
			}
			components = append(components, NewComponent(
				"asset",
				stakedToken,
				stakedAmount,
				Source{Contract: module.Module, Method: "balanceOf"},
			))
		}
		if rewardAmount.Sign() > 0 {
			tokenResult, tokenErr := client.Call(
				ctx,
				block,
				module.Module,
				compoundStakingModuleABI,
				"REWARD_TOKEN",
			)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s reward token: %w", module.Module, tokenErr)
			}
			tokenAddress, tokenErr := AddressAt(tokenResult, 0)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s reward token: %w", module.Module, tokenErr)
			}
			rewardToken, tokenErr := readToken(ctx, client, block, tokenAddress)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s reward token metadata: %w", module.Module, tokenErr)
			}
			components = append(components, NewComponent(
				"reward",
				rewardToken,
				rewardAmount,
				Source{Contract: module.Module, Method: "getTotalRewardsBalance"},
			))
		}
		groups = append(groups, Group{
			ID:         "staking:" + module.Module.Hex(),
			Label:      "Staked",
			Components: components,
			Metadata:   map[string]any{"module": module.Module},
		})
	}
	return groups, nil
}

func compoundV2MarketGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment compoundV2Deployment,
	account common.Address,
	markets []common.Address,
) (*Group, error) {
	if len(markets) > 512 {
		return nil, fmt.Errorf("market count %d exceeds safety bound", len(markets))
	}
	type marketRead struct {
		market common.Address
		native bool
	}
	reads := make([]marketRead, 0, len(markets))
	calls := make([]ContractCall, 0, len(markets)*4)
	for _, market := range markets {
		_, native := deployment.NativeMarkets[market]
		reads = append(reads, marketRead{market: market, native: native})
		calls = append(calls,
			ContractCall{
				Contract: market,
				ABI:      cTokenABI,
				Method:   "balanceOf",
				Args:     []any{account},
			},
			ContractCall{
				Contract: market,
				ABI:      cTokenABI,
				Method:   "exchangeRateStored",
			},
			ContractCall{
				Contract: market,
				ABI:      cTokenABI,
				Method:   "borrowBalanceStored",
				Args:     []any{account},
			},
		)
		if !native {
			calls = append(calls, ContractCall{
				Contract: market,
				ABI:      cTokenABI,
				Method:   "underlying",
			})
		}
	}
	results, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("read markets: %w", err)
	}

	components := make([]Component, 0)
	resultIndex := 0
	for _, read := range reads {
		shares, readErr := BigIntAt(results[resultIndex], 0)
		if readErr != nil {
			return nil, fmt.Errorf("%s shares: %w", read.market, readErr)
		}
		resultIndex++
		exchangeRate, readErr := BigIntAt(results[resultIndex], 0)
		if readErr != nil {
			return nil, fmt.Errorf("%s exchange rate: %w", read.market, readErr)
		}
		resultIndex++
		debt, readErr := BigIntAt(results[resultIndex], 0)
		if readErr != nil {
			return nil, fmt.Errorf("%s debt: %w", read.market, readErr)
		}
		resultIndex++
		supply := new(big.Int).Quo(
			new(big.Int).Mul(shares, exchangeRate),
			big.NewInt(1_000_000_000_000_000_000),
		)

		var underlying common.Address
		if !read.native {
			underlying, readErr = AddressAt(results[resultIndex], 0)
			if readErr != nil {
				return nil, fmt.Errorf("%s underlying: %w", read.market, readErr)
			}
			resultIndex++
		}
		if supply.Sign() == 0 && debt.Sign() == 0 {
			continue
		}

		underlyingToken := deployment.WrappedNative
		if !read.native {
			underlyingToken, readErr = readToken(ctx, client, block, underlying)
			if readErr != nil {
				return nil, fmt.Errorf("%s metadata: %w", underlying, readErr)
			}
		}
		if supply.Sign() > 0 {
			components = append(components, NewComponent(
				"asset",
				underlyingToken,
				supply,
				Source{Contract: read.market, Method: "balanceOf*exchangeRateStored/1e18"},
			))
		}
		if debt.Sign() > 0 {
			components = append(components, NewComponent(
				"debt",
				underlyingToken,
				debt,
				Source{Contract: read.market, Method: "borrowBalanceStored"},
			))
		}
	}
	if len(components) == 0 {
		return nil, nil
	}
	return &Group{
		ID:             "lending",
		Label:          "Lending",
		Components:     components,
		NetValuePolicy: "floor-zero",
		Metadata:       map[string]any{"comptroller": deployment.Comptroller},
	}, nil
}

func (a *CompoundV2Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, ok := a.deployments[block.ChainID]
	if !ok || !deployment.ComptrollerWindow.ActiveAt(block.Number) {
		return nil, nil
	}
	marketResult, err := client.Call(
		ctx,
		block,
		deployment.Comptroller,
		comptrollerABI,
		"getAllMarkets",
	)
	if err != nil {
		return nil, fmt.Errorf("enumerate markets: %w", err)
	}
	if len(marketResult) != 1 {
		return nil, fmt.Errorf("getAllMarkets returned %d fields", len(marketResult))
	}
	markets, err := decodeAddresses(marketResult[0])
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0, 4)
	lendingGroup, err := compoundV2MarketGroup(ctx, client, block, deployment, account, markets)
	if err != nil {
		return nil, err
	}
	if lendingGroup != nil {
		groups = append(groups, *lendingGroup)
	}
	stakingGroups, err := compoundStakingGroups(
		ctx,
		client,
		block,
		deployment.StakingModules,
		account,
	)
	if err != nil {
		return nil, fmt.Errorf("staking: %w", err)
	}
	groups = append(groups, stakingGroups...)
	if account != (common.Address{}) && deployment.RewardLens != (common.Address{}) &&
		deployment.RewardLensWindow.ActiveAt(block.Number) {
		rewardResult, rewardErr := client.Call(
			ctx,
			block,
			deployment.RewardLens,
			compoundLensABI,
			"getCompBalanceMetadataExt",
			deployment.RewardToken.Address,
			deployment.Comptroller,
			account,
		)
		if rewardErr != nil {
			return nil, fmt.Errorf("rewards: %w", rewardErr)
		}
		converted := abi.ConvertType(rewardResult[0], new(compoundRewardMetadata))
		metadata, ok := converted.(*compoundRewardMetadata)
		if !ok || metadata == nil || metadata.Allocated == nil {
			return nil, fmt.Errorf("unexpected reward metadata type %T", rewardResult[0])
		}
		if metadata.Allocated.Sign() > 0 {
			groups = append(groups, Group{
				ID:    "rewards",
				Label: "Rewards",
				Components: []Component{NewComponent(
					"reward",
					deployment.RewardToken,
					metadata.Allocated,
					Source{
						Contract: deployment.RewardLens,
						Method:   "getCompBalanceMetadataExt",
					},
				)},
			})
		}
	}
	if deployment.MultiRewardDistributor != (common.Address{}) &&
		deployment.MultiRewardWindow.ActiveAt(block.Number) {
		rewardGroup, rewardErr := compoundMultiRewardGroup(
			ctx,
			client,
			block,
			deployment.MultiRewardDistributor,
			account,
		)
		if rewardErr != nil {
			return nil, fmt.Errorf("multi rewards: %w", rewardErr)
		}
		if rewardGroup != nil {
			groups = append(groups, *rewardGroup)
		}
	}
	return groups, nil
}

func token(chainID ChainID, address, symbol string, decimals uint8) Token {
	return Token{
		ChainID:  chainID,
		Address:  common.HexToAddress(address),
		Symbol:   symbol,
		Decimals: decimals,
	}
}

func addressSet(values ...string) map[common.Address]struct{} {
	result := make(map[common.Address]struct{}, len(values))
	for _, value := range values {
		result[common.HexToAddress(value)] = struct{}{}
	}
	return result
}

func compoundV2Adapters() []Adapter {
	return []Adapter{
		NewCompoundV2Adapter("compound-v2", "Compound v2", map[ChainID]compoundV2Deployment{
			Ethereum: {
				Comptroller:       common.HexToAddress("0x3d9819210A31b4961b30EF54bE2aeD79B9c9Cd3B"),
				ComptrollerWindow: availableFrom(10_271_924),
				WrappedNative: token(
					Ethereum,
					"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
					"ETH",
					18,
				),
				NativeMarkets:    addressSet("0x4Ddc2D193948926D02f9B1fE9e1daa0718270ED5"),
				RewardLens:       common.HexToAddress("0xdCbDb7306c6Ff46f77B349188dC18cEd9DF30299"),
				RewardLensWindow: availableFrom(13_468_648),
				RewardToken: token(
					Ethereum,
					"0xc00e94Cb662C3520282E6f5717214004A7f26888",
					"COMP",
					18,
				),
			},
		}),
		NewCompoundV2Adapter("moonwell", "Moonwell", map[ChainID]compoundV2Deployment{
			Base: {
				Comptroller:       common.HexToAddress("0xfBb21d0380beE3312B33c4353c8936a0F13EF26C"),
				ComptrollerWindow: availableFrom(2_162_402),
				WrappedNative: token(
					Base,
					"0x4200000000000000000000000000000000000006",
					"WETH",
					18,
				),
				NativeMarkets:          addressSet(),
				MultiRewardDistributor: common.HexToAddress("0xe9005b078701e2A0948D2EaC43010D35870Ad9d2"),
				MultiRewardWindow:      availableFrom(2_162_417),
				StakingModules: []compoundStakingModule{
					{
						Module: common.HexToAddress("0xe66e3a37c3274ac24fe8590f7d84a2427194dc17"),
						Window: availableFrom(12_187_715),
					},
				},
			},
		}),
		NewCompoundV2Adapter("flux-finance", "Flux Finance", map[ChainID]compoundV2Deployment{
			Ethereum: {
				Comptroller:       common.HexToAddress("0x95Af143a021DF745bc78e845b54591C53a8B3A51"),
				ComptrollerWindow: availableFrom(16_520_940),
				WrappedNative: token(
					Ethereum,
					"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
					"ETH",
					18,
				),
				NativeMarkets: addressSet(),
			},
		}),
		NewCompoundV2Adapter("sonne", "Sonne", map[ChainID]compoundV2Deployment{
			Base: {
				Comptroller:       common.HexToAddress("0x1DB2466d9F5e10D7090E7152B68d62703a2245F0"),
				ComptrollerWindow: availableFrom(2_492_954),
				WrappedNative: token(
					Base,
					"0x4200000000000000000000000000000000000006",
					"WETH",
					18,
				),
				NativeMarkets: addressSet(),
			},
		}),
		NewCompoundV2Adapter("lodestar", "Lodestar", map[ChainID]compoundV2Deployment{
			Arbitrum: {
				Comptroller:       common.HexToAddress("0xa86DD95c210dd186Fa7639F93E4177E97d057576"),
				ComptrollerWindow: availableFrom(111_013_008),
				WrappedNative: token(
					Arbitrum,
					"0x82aF49447D8a07e3bd95BD0d56f35241523fBab1",
					"ETH",
					18,
				),
				NativeMarkets: addressSet("0x2193c45244AF12C280941281c8aa67dD08be0a64"),
			},
		}),
	}
}
