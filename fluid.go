package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const (
	fluidMaxFTokens        = 4_096
	fluidMaxAccountNFTs    = 4_096
	fluidPositionBatchSize = 128
)

var fluidLendingResolverABI = MustABI(`[
  {"type":"function","name":"getAllFTokens","stateMutability":"view","inputs":[],"outputs":[{"name":"fTokens","type":"address[]"}]}
]`)

var fluidStakingRewardsABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"earned","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakingToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"rewardsToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var fluidVaultFactoryABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"tokenOfOwnerByIndex","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"index","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var fluidVaultResolverABI = MustABI(`[
  {"type":"function","name":"vaultByNftId","stateMutability":"view","inputs":[{"name":"nftId","type":"uint256"}],"outputs":[{"name":"vault","type":"address"}]}
]`)

var fluidVaultPositionsResolverABI = MustABI(`[
  {"type":"function","name":"getPositionsForNftIds","stateMutability":"view","inputs":[{"name":"nftIds","type":"uint256[]"}],"outputs":[{"name":"positions","type":"tuple[]","components":[
    {"name":"nftId","type":"uint256"},{"name":"owner","type":"address"},
    {"name":"supply","type":"uint256"},{"name":"borrow","type":"uint256"}
  ]}]}
]`)

var fluidVaultABI = MustABI(`[
  {"type":"function","name":"constantsView","stateMutability":"view","inputs":[],"outputs":[{"name":"constantsView","type":"tuple","components":[
    {"name":"liquidity","type":"address"},{"name":"factory","type":"address"},
    {"name":"operateImplementation","type":"address"},{"name":"adminImplementation","type":"address"},
    {"name":"secondaryImplementation","type":"address"},{"name":"deployer","type":"address"},
    {"name":"supply","type":"address"},{"name":"borrow","type":"address"},
    {"name":"supplyToken","type":"tuple","components":[{"name":"token0","type":"address"},{"name":"token1","type":"address"}]},
    {"name":"borrowToken","type":"tuple","components":[{"name":"token0","type":"address"},{"name":"token1","type":"address"}]},
    {"name":"vaultId","type":"uint256"},{"name":"vaultType","type":"uint256"},
    {"name":"supplyExchangePriceSlot","type":"bytes32"},{"name":"borrowExchangePriceSlot","type":"bytes32"},
    {"name":"userSupplySlot","type":"bytes32"},{"name":"userBorrowSlot","type":"bytes32"}
  ]}]}
]`)

var fluidLegacyVaultT1ABI = MustABI(`[
  {"type":"function","name":"constantsView","stateMutability":"view","inputs":[],"outputs":[{"name":"constantsView","type":"tuple","components":[
    {"name":"liquidity","type":"address"},{"name":"factory","type":"address"},
    {"name":"adminImplementation","type":"address"},{"name":"secondaryImplementation","type":"address"},
    {"name":"supplyToken","type":"address"},{"name":"borrowToken","type":"address"},
    {"name":"supplyDecimals","type":"uint8"},{"name":"borrowDecimals","type":"uint8"},
    {"name":"vaultId","type":"uint256"},
    {"name":"liquiditySupplyExchangePriceSlot","type":"bytes32"},{"name":"liquidityBorrowExchangePriceSlot","type":"bytes32"},
    {"name":"liquidityUserSupplySlot","type":"bytes32"},{"name":"liquidityUserBorrowSlot","type":"bytes32"}
  ]}]}
]`)

var fluidDexResolverABI = MustABI(`[
  {"type":"function","name":"getDexState","stateMutability":"nonpayable","inputs":[{"name":"dex","type":"address"}],"outputs":[{"name":"state","type":"tuple","components":[
    {"name":"lastToLastStoredPrice","type":"uint256"},{"name":"lastStoredPrice","type":"uint256"},
    {"name":"centerPrice","type":"uint256"},{"name":"lastUpdateTimestamp","type":"uint256"},
    {"name":"lastPricesTimeDiff","type":"uint256"},{"name":"oracleCheckPoint","type":"uint256"},
    {"name":"oracleMapping","type":"uint256"},{"name":"totalSupplyShares","type":"uint256"},
    {"name":"totalBorrowShares","type":"uint256"},{"name":"isSwapAndArbitragePaused","type":"bool"},
    {"name":"shifts","type":"tuple","components":[
      {"name":"isRangeChangeActive","type":"bool"},{"name":"isThresholdChangeActive","type":"bool"},{"name":"isCenterPriceShiftActive","type":"bool"},
      {"name":"rangeShift","type":"tuple","components":[{"name":"oldUpper","type":"uint256"},{"name":"oldLower","type":"uint256"},{"name":"duration","type":"uint256"},{"name":"startTimestamp","type":"uint256"},{"name":"oldTime","type":"uint256"}]},
      {"name":"thresholdShift","type":"tuple","components":[{"name":"oldUpper","type":"uint256"},{"name":"oldLower","type":"uint256"},{"name":"duration","type":"uint256"},{"name":"startTimestamp","type":"uint256"},{"name":"oldTime","type":"uint256"}]},
      {"name":"centerPriceShift","type":"tuple","components":[{"name":"shiftPercentage","type":"uint256"},{"name":"duration","type":"uint256"},{"name":"startTimestamp","type":"uint256"}]}
    ]},
    {"name":"token0PerSupplyShare","type":"uint256"},{"name":"token1PerSupplyShare","type":"uint256"},
    {"name":"token0PerBorrowShare","type":"uint256"},{"name":"token1PerBorrowShare","type":"uint256"}
  ]}]}
]`)

var (
	fluidNativeSentinel = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")
	fluidShareScale     = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	fluidWrappedNative  = map[ChainID]common.Address{
		Ethereum:  common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		BSC:       common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"),
		Base:      common.HexToAddress("0x4200000000000000000000000000000000000006"),
		Arbitrum:  common.HexToAddress("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1"),
		Polygon:   common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270"),
		Monad:     common.HexToAddress("0x3bd359C1119dA7Da1D913D1C4D2B7c461115433A"),
		Plasma:    common.HexToAddress("0x6100E367285b01F48D07953803A2d8dCA5D19873"),
		Avalanche: common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7"),
		Optimism:  common.HexToAddress("0x4200000000000000000000000000000000000006"),
	}
)

type fluidDeployment struct {
	LendingResolver        common.Address
	VaultFactory           common.Address
	VaultResolver          common.Address
	VaultPositionsResolver common.Address
	DexResolver            common.Address
	LendingWindow          deploymentWindow
	VaultWindow            deploymentWindow
	DexWindow              deploymentWindow
	LiteVaults             []fluidLiteVault
	StakingRewards         []fluidStakingRewards
}

type fluidLiteVault struct {
	Address common.Address
	Window  deploymentWindow
}

type fluidStakingRewards struct {
	Address common.Address
	Window  deploymentWindow
}

var fluidDeployments = map[ChainID]fluidDeployment{
	Ethereum: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0x11D80CfF056Cef4F9E6d23da8672fE9873e5cC07"),
		LendingWindow:          deploymentWindow{ActivationBlock: 23_881_735},
		VaultWindow:            deploymentWindow{ActivationBlock: 24_353_431},
		DexWindow:              deploymentWindow{ActivationBlock: 23_881_747},
		LiteVaults: []fluidLiteVault{{
			Address: common.HexToAddress("0x273DA948ACa9261043fbdb2a857BC255ECC29012"),
			Window:  deploymentWindow{ActivationBlock: 24_616_005},
		}},
		StakingRewards: []fluidStakingRewards{
			{
				Address: common.HexToAddress("0x2fA6c95B69c10f9F52b8990b6C03171F13C46225"),
				Window:  deploymentWindow{ActivationBlock: 19_245_687},
			},
			{
				Address: common.HexToAddress("0x490681095ed277B45377d28cA15Ac41d64583048"),
				Window:  deploymentWindow{ActivationBlock: 19_245_710},
			},
		},
	},
	BSC: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0xAf572EfC84d905926F7b05C1B7bE04e4E89542B0"),
		LendingWindow:          deploymentWindow{ActivationBlock: 71_737_128},
		VaultWindow:            deploymentWindow{ActivationBlock: 79_987_145},
		DexWindow:              deploymentWindow{ActivationBlock: 72_087_151},
	},
	Base: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0x11D80CfF056Cef4F9E6d23da8672fE9873e5cC07"),
		LendingWindow:          deploymentWindow{ActivationBlock: 38_678_564},
		VaultWindow:            deploymentWindow{ActivationBlock: 41_527_842},
		DexWindow:              deploymentWindow{ActivationBlock: 38_678_582},
	},
	Arbitrum: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0x11D80CfF056Cef4F9E6d23da8672fE9873e5cC07"),
		LendingWindow:          deploymentWindow{ActivationBlock: 404_259_654},
		VaultWindow:            deploymentWindow{ActivationBlock: 427_076_636},
		DexWindow:              deploymentWindow{ActivationBlock: 404_259_740},
		StakingRewards: []fluidStakingRewards{
			{
				Address: common.HexToAddress("0x48f89d731C5e3b5BeE8235162FC2C639Ba62DB7d"),
				Window:  deploymentWindow{ActivationBlock: 228_709_698},
			},
			{
				Address: common.HexToAddress("0x65241f6cacde58c03400Cb84542a2c197d6dE9C3"),
				Window:  deploymentWindow{ActivationBlock: 228_709_990},
			},
		},
	},
	Polygon: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0x11D80CfF056Cef4F9E6d23da8672fE9873e5cC07"),
		LendingWindow:          deploymentWindow{ActivationBlock: 79_090_648},
		VaultWindow:            deploymentWindow{ActivationBlock: 82_362_638},
		DexWindow:              deploymentWindow{ActivationBlock: 79_090_686},
	},
	Plasma: {
		LendingResolver:        common.HexToAddress("0x48D32f49aFeAEC7AE66ad7B9264f446fc11a1569"),
		VaultFactory:           common.HexToAddress("0x324c5Dc1fC42c7a4D43d92df1eBA58a54d13Bf2d"),
		VaultResolver:          common.HexToAddress("0xA5C3E16523eeeDDcC34706b0E6bE88b4c6EA95cC"),
		VaultPositionsResolver: common.HexToAddress("0xaA21a86030EAa16546A759d2d10fd3bF9D053Bc7"),
		DexResolver:            common.HexToAddress("0xAf572EfC84d905926F7b05C1B7bE04e4E89542B0"),
		LendingWindow:          deploymentWindow{ActivationBlock: 8_682_622},
		VaultWindow:            deploymentWindow{ActivationBlock: 12_913_750},
		DexWindow:              deploymentWindow{ActivationBlock: 8_682_664},
	},
}

type fluidTokenPair struct {
	Token0 common.Address
	Token1 common.Address
}

type fluidVaultConstants struct {
	Liquidity               common.Address
	Factory                 common.Address
	OperateImplementation   common.Address
	AdminImplementation     common.Address
	SecondaryImplementation common.Address
	Deployer                common.Address
	Supply                  common.Address
	Borrow                  common.Address
	SupplyToken             fluidTokenPair
	BorrowToken             fluidTokenPair
	VaultId                 *big.Int
	VaultType               *big.Int
	SupplyExchangePriceSlot [32]byte
	BorrowExchangePriceSlot [32]byte
	UserSupplySlot          [32]byte
	UserBorrowSlot          [32]byte
}

type fluidLegacyVaultT1Constants struct {
	Liquidity                        common.Address
	Factory                          common.Address
	AdminImplementation              common.Address
	SecondaryImplementation          common.Address
	SupplyToken                      common.Address
	BorrowToken                      common.Address
	SupplyDecimals                   uint8
	BorrowDecimals                   uint8
	VaultId                          *big.Int
	LiquiditySupplyExchangePriceSlot [32]byte
	LiquidityBorrowExchangePriceSlot [32]byte
	LiquidityUserSupplySlot          [32]byte
	LiquidityUserBorrowSlot          [32]byte
}

type fluidVaultPosition struct {
	NftId  *big.Int
	Owner  common.Address
	Supply *big.Int
	Borrow *big.Int
}

type fluidShiftData struct {
	OldUpper       *big.Int
	OldLower       *big.Int
	Duration       *big.Int
	StartTimestamp *big.Int
	OldTime        *big.Int
}

type fluidCenterPriceShift struct {
	ShiftPercentage *big.Int
	Duration        *big.Int
	StartTimestamp  *big.Int
}

type fluidShiftChanges struct {
	IsRangeChangeActive      bool
	IsThresholdChangeActive  bool
	IsCenterPriceShiftActive bool
	RangeShift               fluidShiftData
	ThresholdShift           fluidShiftData
	CenterPriceShift         fluidCenterPriceShift
}

type fluidDexState struct {
	LastToLastStoredPrice    *big.Int
	LastStoredPrice          *big.Int
	CenterPrice              *big.Int
	LastUpdateTimestamp      *big.Int
	LastPricesTimeDiff       *big.Int
	OracleCheckPoint         *big.Int
	OracleMapping            *big.Int
	TotalSupplyShares        *big.Int
	TotalBorrowShares        *big.Int
	IsSwapAndArbitragePaused bool
	Shifts                   fluidShiftChanges
	Token0PerSupplyShare     *big.Int
	Token1PerSupplyShare     *big.Int
	Token0PerBorrowShare     *big.Int
	Token1PerBorrowShare     *big.Int
}

type FluidAdapter struct {
	adapterBase
}

func newFluidAdapter() *FluidAdapter {
	return &FluidAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "fluid", Name: "Fluid", Chains: deploymentChains(fluidDeployments),
	}}}
}

func fluidAddressSlice(values []any, index int) ([]common.Address, error) {
	addresses, err := AddressSliceAt(values, index)
	if err != nil {
		return nil, err
	}
	seen := make(map[common.Address]struct{}, len(addresses))
	for _, address := range addresses {
		if address == (common.Address{}) {
			return nil, fmt.Errorf("resolver returned the zero address")
		}
		if _, exists := seen[address]; exists {
			return nil, fmt.Errorf("resolver returned duplicate address %s", address)
		}
		seen[address] = struct{}{}
	}
	return addresses, nil
}

func fluidUnderlyingAddress(chainID ChainID, address common.Address) (common.Address, bool, error) {
	if address != fluidNativeSentinel {
		return address, false, nil
	}
	wrapped, exists := fluidWrappedNative[chainID]
	if !exists {
		return common.Address{}, false, fmt.Errorf("wrapped native token is not configured for chain %d", chainID)
	}
	return wrapped, true, nil
}

func fluidLendingGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment fluidDeployment,
) ([]Group, error) {
	if !deployment.LendingWindow.ActiveAt(block.Number) {
		return nil, nil
	}
	values, err := client.Call(ctx, block, deployment.LendingResolver, fluidLendingResolverABI, "getAllFTokens")
	if err != nil {
		return nil, fmt.Errorf("lending resolver fToken enumeration: %w", err)
	}
	fTokens, err := fluidAddressSlice(values, 0)
	if err != nil {
		return nil, fmt.Errorf("lending resolver fToken enumeration: %w", err)
	}
	if len(fTokens) > fluidMaxFTokens {
		return nil, fmt.Errorf("lending resolver returned %d fTokens, maximum is %d", len(fTokens), fluidMaxFTokens)
	}
	calls := make([]ContractCall, 0, len(fTokens)*2)
	for _, fToken := range fTokens {
		calls = append(calls,
			ContractCall{Contract: fToken, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: fToken, ABI: erc4626ABI, Method: "asset"},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("fToken headers: %w", err)
	}
	type activeFToken struct {
		address common.Address
		asset   common.Address
		native  bool
		shares  *big.Int
	}
	active := make([]activeFToken, 0)
	for index, fToken := range fTokens {
		shares, decodeErr := BigIntAt(rows[index*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("fToken %s shares: %w", fToken, decodeErr)
		}
		asset, decodeErr := AddressAt(rows[index*2+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("fToken %s asset: %w", fToken, decodeErr)
		}
		asset, native, decodeErr := fluidUnderlyingAddress(block.ChainID, asset)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if shares.Sign() > 0 {
			active = append(active, activeFToken{address: fToken, asset: asset, native: native, shares: shares})
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	conversions := make([]ContractCall, len(active))
	assets := make([]common.Address, len(active))
	for index, position := range active {
		conversions[index] = ContractCall{
			Contract: position.address, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{position.shares},
		}
		assets[index] = position.asset
	}
	converted, err := client.ParallelCalls(ctx, block, conversions)
	if err != nil {
		return nil, fmt.Errorf("fToken share conversion: %w", err)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, assets)
	if err != nil {
		return nil, fmt.Errorf("fToken underlying metadata: %w", err)
	}
	groups := make([]Group, 0, len(active))
	for index, position := range active {
		amount, decodeErr := BigIntAt(converted[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("fToken %s assets: %w", position.address, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		underlying, exists := tokens[position.asset]
		if !exists {
			return nil, fmt.Errorf("fToken %s underlying metadata is absent", position.address)
		}
		component := NewComponent("asset", underlying, amount, Source{
			Contract: position.address, Method: "convertToAssets(balanceOf)",
		})
		component.Metadata = map[string]any{
			"role": "supply", "fToken": position.address, "shares": position.shares.String(),
			"nativeUnderlying": position.native,
		}
		id := "lending:" + strings.ToLower(position.address.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: id, Label: "Yield · " + underlying.Symbol,
			Components: []Component{component}, Metadata: map[string]any{"fToken": position.address},
		})
	}
	return groups, nil
}

func fluidLiteVaultGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment fluidDeployment,
) ([]Group, error) {
	activeVaults := make([]fluidLiteVault, 0, len(deployment.LiteVaults))
	for _, vault := range deployment.LiteVaults {
		if vault.Window.ActiveAt(block.Number) {
			activeVaults = append(activeVaults, vault)
		}
	}
	if len(activeVaults) == 0 {
		return nil, nil
	}
	headerCalls := make([]ContractCall, 0, len(activeVaults)*2)
	for _, vault := range activeVaults {
		headerCalls = append(headerCalls,
			ContractCall{Contract: vault.Address, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: vault.Address, ABI: erc4626ABI, Method: "asset"},
		)
	}
	headers, err := client.ParallelCalls(ctx, block, headerCalls)
	if err != nil {
		return nil, fmt.Errorf("Fluid Lite vault headers: %w", err)
	}
	type activePosition struct {
		vault  common.Address
		asset  common.Address
		shares *big.Int
	}
	positions := make([]activePosition, 0, len(activeVaults))
	for index, vault := range activeVaults {
		shares, decodeErr := BigIntAt(headers[index*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Fluid Lite vault %s shares: %w", vault.Address, decodeErr)
		}
		asset, decodeErr := AddressAt(headers[index*2+1], 0)
		if decodeErr != nil || asset == (common.Address{}) {
			return nil, fmt.Errorf("Fluid Lite vault %s has an invalid asset", vault.Address)
		}
		if shares.Sign() > 0 {
			positions = append(positions, activePosition{vault: vault.Address, asset: asset, shares: shares})
		}
	}
	if len(positions) == 0 {
		return nil, nil
	}
	conversionCalls := make([]ContractCall, len(positions))
	assets := make([]common.Address, len(positions))
	for index, position := range positions {
		conversionCalls[index] = ContractCall{
			Contract: position.vault, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{position.shares},
		}
		assets[index] = position.asset
	}
	conversions, err := client.ParallelCalls(ctx, block, conversionCalls)
	if err != nil {
		return nil, fmt.Errorf("Fluid Lite vault conversions: %w", err)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, assets)
	if err != nil {
		return nil, fmt.Errorf("Fluid Lite vault asset metadata: %w", err)
	}
	groups := make([]Group, 0, len(positions))
	for index, position := range positions {
		amount, decodeErr := BigIntAt(conversions[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Fluid Lite vault %s conversion: %w", position.vault, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		token, exists := tokens[position.asset]
		if !exists {
			return nil, fmt.Errorf("Fluid Lite vault %s asset metadata is absent", position.vault)
		}
		component := NewComponent("asset", token, amount, Source{
			Contract: position.vault, Method: "convertToAssets(balanceOf)",
		})
		component.Metadata = map[string]any{
			"role": "supply", "liteVault": position.vault, "shares": position.shares.String(),
		}
		id := "lite-vault:" + strings.ToLower(position.vault.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: id, Label: "Yield · " + token.Symbol,
			Components: []Component{component}, Metadata: map[string]any{"liteVault": position.vault},
		})
	}
	return groups, nil
}

func fluidStakingRewardGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment fluidDeployment,
) ([]Group, error) {
	activeContracts := make([]fluidStakingRewards, 0, len(deployment.StakingRewards))
	for _, rewards := range deployment.StakingRewards {
		if rewards.Window.ActiveAt(block.Number) {
			activeContracts = append(activeContracts, rewards)
		}
	}
	if len(activeContracts) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(activeContracts)*4)
	for _, rewards := range activeContracts {
		calls = append(calls,
			ContractCall{Contract: rewards.Address, ABI: fluidStakingRewardsABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: rewards.Address, ABI: fluidStakingRewardsABI, Method: "earned", Args: []any{account}},
			ContractCall{Contract: rewards.Address, ABI: fluidStakingRewardsABI, Method: "stakingToken"},
			ContractCall{Contract: rewards.Address, ABI: fluidStakingRewardsABI, Method: "rewardsToken"},
		)
	}
	table, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Fluid staking rewards state: %w", err)
	}
	type activePosition struct {
		contract     common.Address
		stakingToken common.Address
		rewardToken  common.Address
		shares       *big.Int
		earned       *big.Int
	}
	positions := make([]activePosition, 0, len(activeContracts))
	for index, rewards := range activeContracts {
		shares, sharesErr := BigIntAt(table[index*4], 0)
		earned, earnedErr := BigIntAt(table[index*4+1], 0)
		stakingToken, stakingErr := AddressAt(table[index*4+2], 0)
		rewardToken, rewardErr := AddressAt(table[index*4+3], 0)
		if sharesErr != nil || earnedErr != nil || stakingErr != nil || rewardErr != nil ||
			stakingToken == (common.Address{}) || rewardToken == (common.Address{}) {
			return nil, fmt.Errorf("Fluid staking rewards %s returned malformed state", rewards.Address)
		}
		if shares.Sign() > 0 || earned.Sign() > 0 {
			positions = append(positions, activePosition{
				contract: rewards.Address, stakingToken: stakingToken, rewardToken: rewardToken,
				shares: shares, earned: earned,
			})
		}
	}
	if len(positions) == 0 {
		return nil, nil
	}
	underlyingCalls := make([]ContractCall, 0, len(positions)*2)
	for _, position := range positions {
		underlyingCalls = append(underlyingCalls,
			ContractCall{Contract: position.stakingToken, ABI: erc4626ABI, Method: "asset"},
			ContractCall{Contract: position.stakingToken, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{position.shares}},
		)
	}
	underlyingRows, err := client.ParallelCalls(ctx, block, underlyingCalls)
	if err != nil {
		return nil, fmt.Errorf("Fluid staked fToken conversions: %w", err)
	}
	underlyings := make([]common.Address, len(positions))
	amounts := make([]*big.Int, len(positions))
	metadataAddresses := make([]common.Address, 0, len(positions)*2)
	for index, position := range positions {
		underlying, underlyingErr := AddressAt(underlyingRows[index*2], 0)
		amount, amountErr := BigIntAt(underlyingRows[index*2+1], 0)
		if underlyingErr != nil || amountErr != nil || underlying == (common.Address{}) {
			return nil, fmt.Errorf("Fluid staking rewards %s has an invalid fToken", position.contract)
		}
		underlyings[index] = underlying
		amounts[index] = amount
		metadataAddresses = append(metadataAddresses, underlying, position.rewardToken)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, metadataAddresses)
	if err != nil {
		return nil, fmt.Errorf("Fluid staking rewards token metadata: %w", err)
	}
	groups := make([]Group, 0, len(positions))
	for index, position := range positions {
		components := make([]Component, 0, 2)
		if amounts[index].Sign() > 0 {
			underlying, exists := tokens[underlyings[index]]
			if !exists {
				return nil, fmt.Errorf("Fluid staking rewards %s underlying metadata is absent", position.contract)
			}
			component := NewComponent("asset", underlying, amounts[index], Source{
				Contract: position.stakingToken, Method: "convertToAssets(staking balance)",
			})
			component.Metadata = map[string]any{
				"role": "staked-supply", "stakingRewards": position.contract,
				"stakingToken": position.stakingToken, "shares": position.shares.String(),
			}
			components = append(components, component)
		}
		if position.earned.Sign() > 0 {
			reward, exists := tokens[position.rewardToken]
			if !exists {
				return nil, fmt.Errorf("Fluid staking rewards %s reward metadata is absent", position.contract)
			}
			component := NewComponent("reward", reward, position.earned, Source{
				Contract: position.contract, Method: "earned",
			})
			component.Metadata = map[string]any{"role": "reward", "stakingRewards": position.contract}
			components = append(components, component)
		}
		if len(components) == 0 {
			continue
		}
		id := "staking-rewards:" + strings.ToLower(position.contract.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: id, Label: "Farming", Components: components,
			Metadata: map[string]any{
				"stakingRewards": position.contract, "stakingToken": position.stakingToken,
			},
		})
	}
	return groups, nil
}

func fluidVaultNFTs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	factory common.Address,
) ([]*big.Int, error) {
	values, err := client.Call(ctx, block, factory, fluidVaultFactoryABI, "balanceOf", account)
	if err != nil {
		return nil, fmt.Errorf("vault NFT balance: %w", err)
	}
	count, err := BigIntAt(values, 0)
	if err != nil {
		return nil, fmt.Errorf("vault NFT balance: %w", err)
	}
	if !count.IsUint64() || count.Uint64() > fluidMaxAccountNFTs {
		return nil, fmt.Errorf("account has %s Fluid vault NFTs, maximum is %d", count, fluidMaxAccountNFTs)
	}
	calls := make([]ContractCall, count.Uint64())
	for index := range calls {
		calls[index] = ContractCall{
			Contract: factory, ABI: fluidVaultFactoryABI, Method: "tokenOfOwnerByIndex",
			Args: []any{account, new(big.Int).SetUint64(uint64(index))},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("vault NFT enumeration: %w", err)
	}
	ids := make([]*big.Int, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		id, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil || id.Sign() <= 0 {
			return nil, fmt.Errorf("vault NFT index %d returned an invalid ID", index)
		}
		if _, duplicate := seen[id.String()]; duplicate {
			return nil, fmt.Errorf("vault NFT enumeration returned duplicate ID %s", id)
		}
		seen[id.String()] = struct{}{}
		ids[index] = id
	}
	return ids, nil
}

func fluidDecodeVaultPositions(values []any) ([]fluidVaultPosition, error) {
	if len(values) != 1 {
		return nil, fmt.Errorf("position resolver returned %d values", len(values))
	}
	converted := abi.ConvertType(values[0], new([]fluidVaultPosition))
	positions, ok := converted.(*[]fluidVaultPosition)
	if !ok || positions == nil {
		return nil, fmt.Errorf("position resolver returned %T", values[0])
	}
	return *positions, nil
}

func fluidDecodeVaultConstants(values []any) (fluidVaultConstants, error) {
	if len(values) != 1 {
		return fluidVaultConstants{}, fmt.Errorf("constantsView returned %d values", len(values))
	}
	converted := abi.ConvertType(values[0], new(fluidVaultConstants))
	constants, ok := converted.(*fluidVaultConstants)
	if !ok || constants == nil || constants.VaultId == nil || constants.VaultType == nil {
		return fluidVaultConstants{}, fmt.Errorf("constantsView returned malformed %T", values[0])
	}
	return *constants, nil
}

func fluidDecodeLegacyVaultT1Constants(values []any) (fluidVaultConstants, error) {
	if len(values) != 1 {
		return fluidVaultConstants{}, fmt.Errorf("legacy constantsView returned %d values", len(values))
	}
	converted := abi.ConvertType(values[0], new(fluidLegacyVaultT1Constants))
	legacy, ok := converted.(*fluidLegacyVaultT1Constants)
	if !ok || legacy == nil || legacy.VaultId == nil {
		return fluidVaultConstants{}, fmt.Errorf("legacy constantsView returned malformed %T", values[0])
	}
	return fluidVaultConstants{
		Liquidity:               legacy.Liquidity,
		Factory:                 legacy.Factory,
		AdminImplementation:     legacy.AdminImplementation,
		SecondaryImplementation: legacy.SecondaryImplementation,
		Supply:                  legacy.Liquidity,
		Borrow:                  legacy.Liquidity,
		SupplyToken:             fluidTokenPair{Token0: legacy.SupplyToken},
		BorrowToken:             fluidTokenPair{Token0: legacy.BorrowToken},
		VaultId:                 legacy.VaultId,
		VaultType:               big.NewInt(1),
		SupplyExchangePriceSlot: legacy.LiquiditySupplyExchangePriceSlot,
		BorrowExchangePriceSlot: legacy.LiquidityBorrowExchangePriceSlot,
		UserSupplySlot:          legacy.LiquidityUserSupplySlot,
		UserBorrowSlot:          legacy.LiquidityUserBorrowSlot,
	}, nil
}

func fluidDecodeDexState(values []any) (fluidDexState, error) {
	if len(values) != 1 {
		return fluidDexState{}, fmt.Errorf("getDexState returned %d values", len(values))
	}
	converted := abi.ConvertType(values[0], new(fluidDexState))
	state, ok := converted.(*fluidDexState)
	if !ok || state == nil || state.Token0PerSupplyShare == nil || state.Token1PerSupplyShare == nil ||
		state.Token0PerBorrowShare == nil || state.Token1PerBorrowShare == nil {
		return fluidDexState{}, fmt.Errorf("getDexState returned malformed %T", values[0])
	}
	return *state, nil
}

func fluidVaultGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment fluidDeployment,
) ([]Group, error) {
	if !deployment.VaultWindow.ActiveAt(block.Number) {
		return nil, nil
	}
	nftIDs, err := fluidVaultNFTs(ctx, client, block, account, deployment.VaultFactory)
	if err != nil || len(nftIDs) == 0 {
		return nil, err
	}
	positions := make([]fluidVaultPosition, 0, len(nftIDs))
	for start := 0; start < len(nftIDs); start += fluidPositionBatchSize {
		end := min(start+fluidPositionBatchSize, len(nftIDs))
		values, callErr := client.Call(
			ctx, block, deployment.VaultPositionsResolver, fluidVaultPositionsResolverABI,
			"getPositionsForNftIds", nftIDs[start:end],
		)
		if callErr != nil {
			return nil, fmt.Errorf("vault positions %d-%d: %w", start, end-1, callErr)
		}
		batch, decodeErr := fluidDecodeVaultPositions(values)
		if decodeErr != nil || len(batch) != end-start {
			return nil, fmt.Errorf("vault positions %d-%d returned incomplete rows: %w", start, end-1, decodeErr)
		}
		positions = append(positions, batch...)
	}
	vaultCalls := make([]ContractCall, len(nftIDs))
	for index, nftID := range nftIDs {
		vaultCalls[index] = ContractCall{
			Contract: deployment.VaultResolver, ABI: fluidVaultResolverABI,
			Method: "vaultByNftId", Args: []any{nftID},
		}
	}
	vaultRows, err := client.ParallelCalls(ctx, block, vaultCalls)
	if err != nil {
		return nil, fmt.Errorf("vault address resolution: %w", err)
	}
	vaultByID := make(map[string]common.Address, len(nftIDs))
	uniqueVaults := make([]common.Address, 0)
	seenVaults := make(map[common.Address]struct{})
	for index, nftID := range nftIDs {
		if positions[index].NftId == nil || positions[index].NftId.Cmp(nftID) != 0 ||
			positions[index].Owner != account || positions[index].Supply == nil || positions[index].Borrow == nil {
			return nil, fmt.Errorf("vault position resolver returned a foreign or malformed row for NFT %s", nftID)
		}
		vault, decodeErr := AddressAt(vaultRows[index], 0)
		if decodeErr != nil || vault == (common.Address{}) {
			return nil, fmt.Errorf("vault address for NFT %s is invalid", nftID)
		}
		vaultByID[nftID.String()] = vault
		if _, exists := seenVaults[vault]; !exists {
			seenVaults[vault] = struct{}{}
			uniqueVaults = append(uniqueVaults, vault)
		}
	}
	constantCalls := make([]ContractCall, len(uniqueVaults))
	for index, vault := range uniqueVaults {
		constantCalls[index] = ContractCall{Contract: vault, ABI: fluidVaultABI, Method: "constantsView"}
	}
	constantRows, err := client.ParallelCallsAllowFailure(ctx, block, constantCalls)
	if err != nil {
		return nil, fmt.Errorf("vault constants: %w", err)
	}
	constantsByVault := make(map[common.Address]fluidVaultConstants, len(uniqueVaults))
	dexSet := make(map[common.Address]struct{})
	tokenAddresses := make([]common.Address, 0, len(uniqueVaults)*4)
	legacyVaults := make([]common.Address, 0)
	legacyIndexes := make([]int, 0)
	decodedConstants := make([]fluidVaultConstants, len(uniqueVaults))
	for index, vault := range uniqueVaults {
		if constantRows[index].Error != nil {
			legacyVaults = append(legacyVaults, vault)
			legacyIndexes = append(legacyIndexes, index)
			continue
		}
		constants, decodeErr := fluidDecodeVaultConstants(constantRows[index].Values)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s constants: %w", vault, decodeErr)
		}
		decodedConstants[index] = constants
	}
	if len(legacyVaults) > 0 {
		legacyCalls := make([]ContractCall, len(legacyVaults))
		for index, vault := range legacyVaults {
			legacyCalls[index] = ContractCall{Contract: vault, ABI: fluidLegacyVaultT1ABI, Method: "constantsView"}
		}
		legacyRows, legacyErr := client.ParallelCalls(ctx, block, legacyCalls)
		if legacyErr != nil {
			return nil, fmt.Errorf("legacy T1 vault constants: %w", legacyErr)
		}
		for index, row := range legacyRows {
			constants, decodeErr := fluidDecodeLegacyVaultT1Constants(row)
			if decodeErr != nil {
				return nil, fmt.Errorf("vault %s constants: %w", legacyVaults[index], decodeErr)
			}
			decodedConstants[legacyIndexes[index]] = constants
		}
	}
	for index, vault := range uniqueVaults {
		constants := decodedConstants[index]
		if constants.Factory != deployment.VaultFactory {
			return nil, fmt.Errorf("vault %s factory changed from %s to %s", vault, deployment.VaultFactory, constants.Factory)
		}
		constantsByVault[vault] = constants
		for _, pair := range []struct {
			protocol common.Address
			tokens   fluidTokenPair
		}{{constants.Supply, constants.SupplyToken}, {constants.Borrow, constants.BorrowToken}} {
			if pair.tokens.Token1 != (common.Address{}) {
				dexSet[pair.protocol] = struct{}{}
			}
			for _, address := range []common.Address{pair.tokens.Token0, pair.tokens.Token1} {
				if address == (common.Address{}) {
					continue
				}
				normalized, _, normalizeErr := fluidUnderlyingAddress(block.ChainID, address)
				if normalizeErr != nil {
					return nil, normalizeErr
				}
				tokenAddresses = append(tokenAddresses, normalized)
			}
		}
	}
	if len(dexSet) > 0 && !deployment.DexWindow.ActiveAt(block.Number) {
		return nil, fmt.Errorf("Fluid smart vault resolver is not active at block %d", block.Number)
	}
	dexes := make([]common.Address, 0, len(dexSet))
	for dex := range dexSet {
		dexes = append(dexes, dex)
	}
	sort.Slice(dexes, func(i, j int) bool { return strings.ToLower(dexes[i].Hex()) < strings.ToLower(dexes[j].Hex()) })
	dexCalls := make([]ContractCall, len(dexes))
	for index, dex := range dexes {
		dexCalls[index] = ContractCall{
			Contract: deployment.DexResolver, ABI: fluidDexResolverABI, Method: "getDexState", Args: []any{dex},
		}
	}
	dexRows, err := client.ParallelCalls(ctx, block, dexCalls)
	if err != nil {
		return nil, fmt.Errorf("smart vault share conversion: %w", err)
	}
	dexStates := make(map[common.Address]fluidDexState, len(dexes))
	for index, dex := range dexes {
		state, decodeErr := fluidDecodeDexState(dexRows[index])
		if decodeErr != nil {
			return nil, fmt.Errorf("DEX %s state: %w", dex, decodeErr)
		}
		dexStates[dex] = state
	}
	tokens, err := tokenMetadataAt(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, fmt.Errorf("vault token metadata: %w", err)
	}
	groups := make([]Group, 0, len(positions))
	for _, position := range positions {
		if position.Supply.Sign() == 0 && position.Borrow.Sign() == 0 {
			continue
		}
		vault := vaultByID[position.NftId.String()]
		constants := constantsByVault[vault]
		components := make([]Component, 0, 4)
		appendNormal := func(kind string, amount *big.Int, address common.Address, role string) error {
			if amount.Sign() == 0 {
				return nil
			}
			normalized, native, normalizeErr := fluidUnderlyingAddress(block.ChainID, address)
			if normalizeErr != nil {
				return normalizeErr
			}
			tokenInfo, exists := tokens[normalized]
			if !exists {
				return fmt.Errorf("token metadata is absent for %s", normalized)
			}
			component := NewComponent(kind, tokenInfo, amount, Source{
				Contract: deployment.VaultPositionsResolver, Method: "getPositionsForNftIds",
			})
			component.Metadata = map[string]any{"role": role, "nativeUnderlying": native}
			components = append(components, component)
			return nil
		}
		appendSmart := func(kind string, shares *big.Int, pair fluidTokenPair, state fluidDexState, role string, supply bool) error {
			if shares.Sign() == 0 {
				return nil
			}
			ratios := []*big.Int{state.Token0PerBorrowShare, state.Token1PerBorrowShare}
			if supply {
				ratios = []*big.Int{state.Token0PerSupplyShare, state.Token1PerSupplyShare}
			}
			for index, address := range []common.Address{pair.Token0, pair.Token1} {
				if address == (common.Address{}) || ratios[index].Sign() == 0 {
					continue
				}
				normalized, native, normalizeErr := fluidUnderlyingAddress(block.ChainID, address)
				if normalizeErr != nil {
					return normalizeErr
				}
				tokenInfo, exists := tokens[normalized]
				if !exists {
					return fmt.Errorf("token metadata is absent for %s", normalized)
				}
				numerator := new(big.Int).Mul(shares, ratios[index])
				component := NewComponent(kind, tokenInfo, numerator, Source{
					Contract: deployment.DexResolver, Method: "getDexState share ratio",
				})
				component.AmountDenominatorRaw = fluidShareScale.String()
				component.Metadata = map[string]any{
					"role": role, "shares": shares.String(), "tokenPerShare": ratios[index].String(),
					"nativeUnderlying": native,
				}
				components = append(components, component)
			}
			return nil
		}
		if constants.SupplyToken.Token1 == (common.Address{}) {
			err = appendNormal("asset", position.Supply, constants.SupplyToken.Token0, "collateral")
		} else {
			err = appendSmart("asset", position.Supply, constants.SupplyToken, dexStates[constants.Supply], "smart-collateral", true)
		}
		if err != nil {
			return nil, fmt.Errorf("vault %s supply: %w", vault, err)
		}
		if constants.BorrowToken.Token1 == (common.Address{}) {
			err = appendNormal("debt", position.Borrow, constants.BorrowToken.Token0, "borrow")
		} else {
			err = appendSmart("debt", position.Borrow, constants.BorrowToken, dexStates[constants.Borrow], "smart-debt", false)
		}
		if err != nil {
			return nil, fmt.Errorf("vault %s borrow: %w", vault, err)
		}
		if len(components) == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:       "vault-nft:" + position.NftId.String(),
			MarketID: "vault:" + strings.ToLower(vault.Hex()),
			Label:    "Lending", Components: components, NetValuePolicy: "floor-zero",
			Metadata: map[string]any{
				"nftId": position.NftId.String(), "vault": vault,
				"vaultId": constants.VaultId.String(), "vaultType": constants.VaultType.String(),
			},
		})
	}
	return groups, nil
}

func (a *FluidAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := fluidDeployments[block.ChainID]
	if !exists {
		return nil, nil
	}
	lending, err := fluidLendingGroups(ctx, client, block, account, deployment)
	if err != nil {
		return nil, err
	}
	liteVaults, err := fluidLiteVaultGroups(ctx, client, block, account, deployment)
	if err != nil {
		return nil, err
	}
	stakingRewards, err := fluidStakingRewardGroups(ctx, client, block, account, deployment)
	if err != nil {
		return nil, err
	}
	vaults, err := fluidVaultGroups(ctx, client, block, account, deployment)
	if err != nil {
		return nil, err
	}
	groups := append(lending, liteVaults...)
	groups = append(groups, stakingRewards...)
	groups = append(groups, vaults...)
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups, nil
}
