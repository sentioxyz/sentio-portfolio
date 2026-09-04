package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

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

var compoundDistributorStakingABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"}]},
  {"type":"function","name":"sonne","stateMutability":"view","inputs":[],"outputs":[{"name":"token","type":"address"}]},
  {"type":"function","name":"withdrawal","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"},{"name":"releaseTime","type":"uint256"}]},
  {"type":"function","name":"tokens","stateMutability":"view","inputs":[{"name":"index","type":"uint256"}],"outputs":[{"name":"token","type":"address"}]},
  {"type":"function","name":"getClaimable","stateMutability":"view","inputs":[{"name":"token","type":"address"},{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"}]}
]`)

const (
	compoundDistributorRewardProbeSize = 8
	compoundDistributorMaxRewards      = 64
	compoundV2ExchangeRateDenominator  = "1000000000000000000"
)

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

// compoundDistributorStakingModule is a 1:1 staking wrapper backed by a
// Distributor reward index. Its public tokens array deliberately has no length
// getter, so the adapter enumerates it with bounded tokens(index) probes.
type compoundDistributorStakingModule struct {
	Module common.Address
	Window availabilityWindow
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
	DistributorStaking     []compoundDistributorStakingModule
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
	return &CompoundV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: id, Name: name, Chains: deploymentChains(deployments),
		}},
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

func compoundDistributorRewardTokens(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	distributor common.Address,
) ([]common.Address, error) {
	rewardTokens := make([]common.Address, 0)
	seen := make(map[common.Address]struct{})
	// Index zero is a sentinel installed by Distributor's constructor. Probe one
	// element beyond the maximum and cap malformed or unexpectedly long lists;
	// reward discovery must never suppress the independently readable principal.
	for start := 0; start <= compoundDistributorMaxRewards+1; start += compoundDistributorRewardProbeSize {
		end := min(
			start+compoundDistributorRewardProbeSize,
			compoundDistributorMaxRewards+2,
		)
		calls := make([]ContractCall, 0, end-start)
		for index := start; index < end; index++ {
			calls = append(calls, ContractCall{
				Contract: distributor,
				ABI:      compoundDistributorStakingABI,
				Method:   "tokens",
				Args:     []any{new(big.Int).SetUint64(uint64(index))},
			})
		}
		rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
		if err != nil {
			return nil, fmt.Errorf("enumerate reward tokens %d-%d: %w", start, end-1, err)
		}
		for offset, row := range rows {
			index := start + offset
			if row.Error != nil {
				return rewardTokens, nil
			}
			address, decodeErr := AddressAt(row.Values, 0)
			if decodeErr != nil {
				continue
			}
			if index == 0 {
				continue
			}
			if address == (common.Address{}) {
				continue
			}
			if len(rewardTokens) == compoundDistributorMaxRewards {
				return rewardTokens, nil
			}
			if _, exists := seen[address]; exists {
				continue
			}
			seen[address] = struct{}{}
			rewardTokens = append(rewardTokens, address)
		}
	}
	return rewardTokens, nil
}

func compoundDistributorStakingGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	module compoundDistributorStakingModule,
	account common.Address,
	tokenCache map[common.Address]Token,
) (*Group, error) {
	principalRows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{
			Contract: module.Module,
			ABI:      compoundDistributorStakingABI,
			Method:   "balanceOf",
			Args:     []any{account},
		},
		{
			Contract: module.Module,
			ABI:      compoundDistributorStakingABI,
			Method:   "withdrawal",
			Args:     []any{account},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("principal: %w", err)
	}
	stakedAmount, err := BigIntAt(principalRows[0], 0)
	if err != nil {
		return nil, fmt.Errorf("balance: %w", err)
	}
	pendingWithdrawal, err := BigIntAt(principalRows[1], 0)
	if err != nil {
		return nil, fmt.Errorf("pending withdrawal: %w", err)
	}
	releaseTime, err := BigIntAt(principalRows[1], 1)
	if err != nil {
		return nil, fmt.Errorf("withdrawal release time: %w", err)
	}
	var partialErr error
	rewardTokens, rewardDiscoveryErr := compoundDistributorRewardTokens(
		ctx,
		client,
		block,
		module.Module,
	)
	if rewardDiscoveryErr != nil {
		partialErr = errors.Join(partialErr, fmt.Errorf("reward discovery: %w", rewardDiscoveryErr))
		rewardTokens = nil
	}
	rewardCalls := make([]ContractCall, 0, len(rewardTokens))
	for _, rewardToken := range rewardTokens {
		rewardCalls = append(rewardCalls, ContractCall{
			Contract: module.Module,
			ABI:      compoundDistributorStakingABI,
			Method:   "getClaimable",
			Args:     []any{rewardToken, account},
		})
	}
	rewardRows, rewardCallsErr := client.ParallelCallsAllowFailure(ctx, block, rewardCalls)
	if rewardCallsErr != nil {
		partialErr = errors.Join(partialErr, fmt.Errorf("claimable rewards: %w", rewardCallsErr))
		rewardRows = nil
		rewardTokens = nil
	}

	readCachedToken := func(address common.Address) (Token, error) {
		if cached, exists := tokenCache[address]; exists {
			return cached, nil
		}
		metadata, metadataErr := readToken(ctx, client, block, address)
		if metadataErr != nil {
			return Token{}, metadataErr
		}
		tokenCache[address] = metadata
		return metadata, nil
	}

	components := make([]Component, 0, 1+len(rewardRows))
	principal := new(big.Int).Add(stakedAmount, pendingWithdrawal)
	if principal.Sign() > 0 {
		stakedTokenResult, tokenErr := client.Call(
			ctx,
			block,
			module.Module,
			compoundDistributorStakingABI,
			"sonne",
		)
		if tokenErr != nil {
			return nil, fmt.Errorf("staked token: %w", tokenErr)
		}
		stakedTokenAddress, tokenErr := AddressAt(stakedTokenResult, 0)
		if tokenErr != nil {
			return nil, fmt.Errorf("staked token: %w", tokenErr)
		}
		if stakedTokenAddress == (common.Address{}) {
			return nil, fmt.Errorf("staked token is the zero address")
		}
		stakedToken, tokenErr := readCachedToken(stakedTokenAddress)
		if tokenErr != nil {
			return nil, fmt.Errorf("staked token metadata: %w", tokenErr)
		}
		component := NewComponent(
			"asset",
			stakedToken,
			principal,
			Source{Contract: module.Module, Method: "balanceOf+withdrawal"},
		)
		component.Metadata = map[string]any{
			"stakedAmountRaw":            stakedAmount.String(),
			"pendingWithdrawalAmountRaw": pendingWithdrawal.String(),
			"withdrawalReleaseTime":      releaseTime.String(),
		}
		components = append(components, component)
	}
	for index, row := range rewardRows {
		if row.Error != nil {
			continue
		}
		amount, amountErr := BigIntAt(row.Values, 0)
		if amountErr != nil {
			continue
		}
		if amount.Sign() <= 0 {
			continue
		}
		rewardToken, tokenErr := readCachedToken(rewardTokens[index])
		if tokenErr != nil {
			continue
		}
		components = append(components, NewComponent(
			"reward",
			rewardToken,
			amount,
			Source{Contract: module.Module, Method: "getClaimable"},
		))
	}
	if len(components) == 0 {
		return nil, partialErr
	}
	return &Group{
		ID:         "staking:" + strings.ToLower(module.Module.Hex()),
		Label:      "Staked",
		Components: components,
		Metadata:   map[string]any{"module": module.Module},
	}, partialErr
}

func compoundDistributorStakingGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	modules []compoundDistributorStakingModule,
	account common.Address,
) ([]Group, error) {
	if account == (common.Address{}) {
		return nil, nil
	}
	groups := make([]Group, 0, len(modules))
	tokenCache := make(map[common.Address]Token)
	var partialErr error
	for _, module := range modules {
		if !module.Window.ActiveAt(block.Number) {
			continue
		}
		group, err := compoundDistributorStakingGroup(
			ctx,
			client,
			block,
			module,
			account,
			tokenCache,
		)
		if err != nil {
			partialErr = errors.Join(partialErr, fmt.Errorf("%s: %w", module.Module, err))
		}
		if group != nil {
			groups = append(groups, *group)
		}
	}
	return groups, partialErr
}

func compoundV2SupplyComponent(
	token Token,
	numerator *big.Int,
	market common.Address,
) Component {
	component := NewComponent(
		"asset",
		token,
		numerator,
		Source{Contract: market, Method: "balanceOf*exchangeRateStored/1e18"},
	)
	component.AmountDenominatorRaw = compoundV2ExchangeRateDenominator
	return component
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
		supplyNumerator := new(big.Int).Mul(shares, exchangeRate)

		var underlying common.Address
		if !read.native {
			underlying, readErr = AddressAt(results[resultIndex], 0)
			if readErr != nil {
				return nil, fmt.Errorf("%s underlying: %w", read.market, readErr)
			}
			resultIndex++
		}
		if supplyNumerator.Sign() == 0 && debt.Sign() == 0 {
			continue
		}

		underlyingToken := deployment.WrappedNative
		if !read.native {
			underlyingToken, readErr = readToken(ctx, client, block, underlying)
			if readErr != nil {
				return nil, fmt.Errorf("%s metadata: %w", underlying, readErr)
			}
		}
		if supplyNumerator.Sign() > 0 {
			components = append(
				components,
				compoundV2SupplyComponent(underlyingToken, supplyNumerator, read.market),
			)
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
	if !ok {
		return nil, nil
	}
	groups := make([]Group, 0, 4+len(deployment.DistributorStaking))
	if deployment.ComptrollerWindow.ActiveAt(block.Number) {
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
		lendingGroup, err := compoundV2MarketGroup(ctx, client, block, deployment, account, markets)
		if err != nil {
			return nil, err
		}
		if lendingGroup != nil {
			groups = append(groups, *lendingGroup)
		}
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
	distributorStakingGroups, err := compoundDistributorStakingGroups(
		ctx,
		client,
		block,
		deployment.DistributorStaking,
		account,
	)
	groups = append(groups, distributorStakingGroups...)
	if err != nil {
		return groups, fmt.Errorf("distributor staking: %w", err)
	}
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
			Optimism: {
				Comptroller:       common.HexToAddress("0xCa889f40aae37FFf165BccF69aeF1E82b5C511B9"),
				ComptrollerWindow: availableFrom(122_531_304),
				WrappedNative: token(
					Optimism,
					"0x4200000000000000000000000000000000000006",
					"WETH",
					18,
				),
				NativeMarkets:          addressSet(),
				MultiRewardDistributor: common.HexToAddress("0xF9524bfa18C19C3E605FbfE8DFd05C6e967574Aa"),
				MultiRewardWindow:      availableFrom(122_531_322),
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
			Optimism: {
				Comptroller:       common.HexToAddress("0x60CF091cD3f50420d50fD7f707414d0DF4751C58"),
				ComptrollerWindow: availableFrom(26_050_163),
				WrappedNative: token(
					Optimism,
					"0x4200000000000000000000000000000000000006",
					"WETH",
					18,
				),
				NativeMarkets: addressSet(),
				DistributorStaking: []compoundDistributorStakingModule{
					{
						Module: common.HexToAddress("0xdc05d85069dc4aba65954008ff99f2d73ff12618"),
						Window: availableFrom(25_840_175),
					},
					{
						Module: common.HexToAddress("0x41279e29586eb20f9a4f65e031af09fced171166"),
						Window: availableFrom(25_840_274),
					},
				},
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
