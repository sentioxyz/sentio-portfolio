package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var venusPoolRegistryABI = MustABI(`[
  {
    "type":"function",
    "name":"getAllPools",
    "stateMutability":"view",
    "inputs":[],
    "outputs":[{"name":"pools","type":"tuple[]","components":[
      {"name":"name","type":"string"},
      {"name":"creator","type":"address"},
      {"name":"comptroller","type":"address"},
      {"name":"blockPosted","type":"uint256"},
      {"name":"timestampPosted","type":"uint256"}
    ]}]
  }
]`)

var venusPoolLensABI = MustABI(`[
  {
    "type":"function",
    "name":"getPendingRewards",
    "stateMutability":"view",
    "inputs":[
      {"name":"account","type":"address"},
      {"name":"comptroller","type":"address"}
    ],
    "outputs":[{"name":"summaries","type":"tuple[]","components":[
      {"name":"distributorAddress","type":"address"},
      {"name":"rewardTokenAddress","type":"address"},
      {"name":"totalRewards","type":"uint256"},
      {"name":"pendingRewards","type":"tuple[]","components":[
        {"name":"vTokenAddress","type":"address"},
        {"name":"amount","type":"uint256"}
      ]}
    ]}]
  }
]`)

var venusPendingRewardsLensABI = MustABI(`[
  {
    "type":"function",
    "name":"pendingRewards",
    "stateMutability":"view",
    "inputs":[
      {"name":"holder","type":"address"},
      {"name":"comptroller","type":"address"}
    ],
    "outputs":[{"name":"summary","type":"tuple","components":[
      {"name":"distributorAddress","type":"address"},
      {"name":"rewardTokenAddress","type":"address"},
      {"name":"totalRewards","type":"uint256"},
      {"name":"pendingRewards","type":"tuple[]","components":[
        {"name":"vTokenAddress","type":"address"},
        {"name":"amount","type":"uint256"}
      ]}
    ]}]
  }
]`)

var venusCoreComptrollerABI = MustABI(`[
  {"type":"function","name":"mintedVAIs","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"amount","type":"uint256"}]}
]`)

var xvsVaultABI = MustABI(`[
  {"type":"function","name":"xvsAddress","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"poolLength","stateMutability":"view","inputs":[{"name":"rewardToken","type":"address"}],"outputs":[{"type":"uint256"}]},
  {
    "type":"function",
    "name":"poolInfos",
    "stateMutability":"view",
    "inputs":[{"name":"rewardToken","type":"address"},{"name":"pid","type":"uint256"}],
    "outputs":[
      {"name":"token","type":"address"},
      {"name":"allocPoint","type":"uint256"},
      {"name":"lastRewardBlockOrSecond","type":"uint256"},
      {"name":"accRewardPerShare","type":"uint256"},
      {"name":"lockPeriod","type":"uint256"}
    ]
  },
  {
    "type":"function",
    "name":"getUserInfo",
    "stateMutability":"view",
    "inputs":[
      {"name":"rewardToken","type":"address"},
      {"name":"pid","type":"uint256"},
      {"name":"user","type":"address"}
    ],
    "outputs":[
      {"name":"amount","type":"uint256"},
      {"name":"rewardDebt","type":"uint256"},
      {"name":"pendingWithdrawals","type":"uint256"}
    ]
  },
  {
    "type":"function",
    "name":"pendingReward",
    "stateMutability":"view",
    "inputs":[
      {"name":"rewardToken","type":"address"},
      {"name":"pid","type":"uint256"},
      {"name":"user","type":"address"}
    ],
    "outputs":[{"type":"uint256"}]
  }
]`)

type venusPool struct {
	Name            string
	Creator         common.Address
	Comptroller     common.Address
	BlockPosted     *big.Int
	TimestampPosted *big.Int
}

type venusPendingReward struct {
	VTokenAddress common.Address
	Amount        *big.Int
}

type venusRewardSummary struct {
	DistributorAddress common.Address
	RewardTokenAddress common.Address
	TotalRewards       *big.Int
	PendingRewards     []venusPendingReward
}

type venusDeployment struct {
	PoolRegistry    common.Address
	PoolLens        common.Address
	XVSVault        common.Address
	WrappedNative   Token
	Core            *compoundV2Deployment
	CoreRewardsLens common.Address
	VAI             *Token
}

type VenusAdapter struct {
	adapterBase
	deployments map[ChainID]venusDeployment
}

func newVenusAdapter() Adapter {
	bscVAI := token(
		BSC,
		"0x4BD17003473389A42DAF6a0a729f6Fdb328BbBd7",
		"VAI",
		18,
	)
	bscCore := compoundV2Deployment{
		Comptroller: common.HexToAddress("0xfD36E2c2a6789Db23113685031d7F16329158384"),
		WrappedNative: token(
			BSC,
			"0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c",
			"BNB",
			18,
		),
		NativeMarkets: addressSet("0xA07c5b74C9B40447a954e1466938b865b6BBea36"),
	}
	return &VenusAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID:     "venus",
			Name:   "Venus",
			Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		deployments: map[ChainID]venusDeployment{
			Ethereum: {
				PoolRegistry:  common.HexToAddress("0x61CAff113CCaf05FFc6540302c37adcf077C5179"),
				PoolLens:      common.HexToAddress("0x277950603178BDD223eB53B9b7cF5D0053aa3473"),
				XVSVault:      common.HexToAddress("0xA0882C2D5DF29233A092d2887A258C2b90e9b994"),
				WrappedNative: token(Ethereum, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "ETH", 18),
			},
			BSC: {
				PoolRegistry:    common.HexToAddress("0x9F7b01A536aFA00EF10310A162877fd792cD0666"),
				PoolLens:        common.HexToAddress("0x9459a33c0a4EAd7794497Da85867859CdB06aCc5"),
				XVSVault:        common.HexToAddress("0x051100480289e704d20e9DB4804837068f3f9204"),
				WrappedNative:   bscCore.WrappedNative,
				Core:            &bscCore,
				CoreRewardsLens: common.HexToAddress("0xe797804c5d4410777c70EF8769c4eB9C39BEF662"),
				VAI:             &bscVAI,
			},
			Base: {
				PoolRegistry:  common.HexToAddress("0xeef902918DdeCD773D4B422aa1C6e1673EB9136F"),
				PoolLens:      common.HexToAddress("0x89825677fb4845f5Fc0B227e387455ECa1200058"),
				XVSVault:      common.HexToAddress("0x708B54F2C3f3606ea48a8d94dab88D9Ab22D7fCd"),
				WrappedNative: token(Base, "0x4200000000000000000000000000000000000006", "ETH", 18),
			},
			Arbitrum: {
				PoolRegistry:  common.HexToAddress("0x382238f07Bc4Fe4aA99e561adE8A4164b5f815DA"),
				PoolLens:      common.HexToAddress("0x53F34FF95367B2A4542461a6A63fD321F8da22AD"),
				XVSVault:      common.HexToAddress("0x8b79692AAB2822Be30a6382Eb04763A74752d5B4"),
				WrappedNative: token(Arbitrum, "0x82aF49447D8a07e3bd95BD0d56f35241523fBab1", "ETH", 18),
			},
		},
	}
}

func venusRewardAmount(summary venusRewardSummary) *big.Int {
	total := new(big.Int)
	if summary.TotalRewards != nil {
		total.Set(summary.TotalRewards)
	}
	for _, reward := range summary.PendingRewards {
		if reward.Amount != nil {
			total.Add(total, reward.Amount)
		}
	}
	return total
}

func venusRewardGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	summaries []venusRewardSummary,
	source common.Address,
	method string,
	label string,
) (*Group, error) {
	totals := make(map[common.Address]*big.Int)
	for _, summary := range summaries {
		amount := venusRewardAmount(summary)
		if amount.Sign() == 0 {
			continue
		}
		if _, exists := totals[summary.RewardTokenAddress]; !exists {
			totals[summary.RewardTokenAddress] = new(big.Int)
		}
		totals[summary.RewardTokenAddress].Add(
			totals[summary.RewardTokenAddress],
			amount,
		)
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
		rewardToken, err := readToken(ctx, client, block, address)
		if err != nil {
			return nil, fmt.Errorf("%s reward metadata: %w", address, err)
		}
		components = append(components, NewComponent(
			"reward",
			rewardToken,
			totals[address],
			Source{Contract: source, Method: method},
		))
	}
	if len(components) == 0 {
		return nil, nil
	}
	return &Group{
		ID:         method,
		Label:      label,
		Components: components,
	}, nil
}

func venusCoreRewardGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	lens common.Address,
	comptroller common.Address,
	account common.Address,
) (*Group, error) {
	result, err := client.Call(
		ctx,
		block,
		lens,
		venusPendingRewardsLensABI,
		"pendingRewards",
		account,
		comptroller,
	)
	if err != nil {
		return nil, err
	}
	if len(result) != 1 {
		return nil, fmt.Errorf("pendingRewards returned %d fields", len(result))
	}
	converted := abi.ConvertType(result[0], new(venusRewardSummary))
	summary, ok := converted.(*venusRewardSummary)
	if !ok || summary == nil {
		return nil, fmt.Errorf("unexpected core reward type %T", result[0])
	}
	return venusRewardGroup(
		ctx,
		client,
		block,
		[]venusRewardSummary{*summary},
		lens,
		"pendingRewards",
		"Core pool rewards",
	)
}

func venusIsolatedRewardGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	lens common.Address,
	comptrollers []common.Address,
	account common.Address,
) (*Group, error) {
	calls := make([]ContractCall, 0, len(comptrollers))
	for _, comptroller := range comptrollers {
		calls = append(calls, ContractCall{
			Contract: lens,
			ABI:      venusPoolLensABI,
			Method:   "getPendingRewards",
			Args:     []any{account, comptroller},
		})
	}
	results, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	summaries := make([]venusRewardSummary, 0)
	for _, result := range results {
		if len(result) != 1 {
			return nil, fmt.Errorf("getPendingRewards returned %d fields", len(result))
		}
		converted := abi.ConvertType(result[0], new([]venusRewardSummary))
		rows, ok := converted.(*[]venusRewardSummary)
		if !ok || rows == nil {
			return nil, fmt.Errorf("unexpected isolated reward type %T", result[0])
		}
		summaries = append(summaries, (*rows)...)
	}
	return venusRewardGroup(
		ctx,
		client,
		block,
		summaries,
		lens,
		"getPendingRewards",
		"Isolated pool rewards",
	)
}

func venusVaultGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	vault common.Address,
	account common.Address,
) ([]Group, error) {
	xvsResult, err := client.Call(ctx, block, vault, xvsVaultABI, "xvsAddress")
	if err != nil {
		return nil, err
	}
	xvs, err := AddressAt(xvsResult, 0)
	if err != nil {
		return nil, err
	}
	lengthResult, err := client.Call(ctx, block, vault, xvsVaultABI, "poolLength", xvs)
	if err != nil {
		return nil, err
	}
	length, err := BigIntAt(lengthResult, 0)
	if err != nil {
		return nil, err
	}
	if !length.IsUint64() || length.Uint64() > 128 {
		return nil, fmt.Errorf("XVS vault pool count %s exceeds safety bound", length)
	}

	calls := make([]ContractCall, 0, length.Uint64()*3)
	for pid := uint64(0); pid < length.Uint64(); pid++ {
		poolID := new(big.Int).SetUint64(pid)
		calls = append(calls,
			ContractCall{
				Contract: vault,
				ABI:      xvsVaultABI,
				Method:   "poolInfos",
				Args:     []any{xvs, poolID},
			},
			ContractCall{
				Contract: vault,
				ABI:      xvsVaultABI,
				Method:   "getUserInfo",
				Args:     []any{xvs, poolID, account},
			},
			ContractCall{
				Contract: vault,
				ABI:      xvsVaultABI,
				Method:   "pendingReward",
				Args:     []any{xvs, poolID, account},
			},
		)
	}
	results, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}

	groups := make([]Group, 0)
	var xvsToken *Token
	for pid := uint64(0); pid < length.Uint64(); pid++ {
		index := int(pid * 3)
		stakedTokenAddress, readErr := AddressAt(results[index], 0)
		if readErr != nil {
			return nil, fmt.Errorf("vault pool %d token: %w", pid, readErr)
		}
		amount, readErr := BigIntAt(results[index+1], 0)
		if readErr != nil {
			return nil, fmt.Errorf("vault pool %d amount: %w", pid, readErr)
		}
		pendingWithdrawals, readErr := BigIntAt(results[index+1], 2)
		if readErr != nil {
			return nil, fmt.Errorf("vault pool %d pending withdrawals: %w", pid, readErr)
		}
		pendingReward, readErr := BigIntAt(results[index+2], 0)
		if readErr != nil {
			return nil, fmt.Errorf("vault pool %d pending reward: %w", pid, readErr)
		}
		if amount.Sign() == 0 && pendingReward.Sign() == 0 {
			continue
		}

		components := make([]Component, 0, 2)
		if amount.Sign() > 0 {
			stakedToken, metadataErr := readToken(ctx, client, block, stakedTokenAddress)
			if metadataErr != nil {
				return nil, fmt.Errorf("vault pool %d token metadata: %w", pid, metadataErr)
			}
			component := NewComponent(
				"asset",
				stakedToken,
				amount,
				Source{Contract: vault, Method: "getUserInfo"},
			)
			component.Metadata = map[string]any{
				"pendingWithdrawals": pendingWithdrawals.String(),
			}
			components = append(components, component)
		}
		if pendingReward.Sign() > 0 {
			if xvsToken == nil {
				rewardToken, metadataErr := readToken(ctx, client, block, xvs)
				if metadataErr != nil {
					return nil, fmt.Errorf("XVS metadata: %w", metadataErr)
				}
				xvsToken = &rewardToken
			}
			components = append(components, NewComponent(
				"reward",
				*xvsToken,
				pendingReward,
				Source{Contract: vault, Method: "pendingReward"},
			))
		}
		groups = append(groups, Group{
			ID:         fmt.Sprintf("xvs-vault:%d", pid),
			Label:      fmt.Sprintf("XVS Vault pool %d", pid),
			Components: components,
			Metadata: map[string]any{
				"vault": vault,
				"pid":   pid,
			},
		})
	}
	return groups, nil
}

func (a *VenusAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, ok := a.deployments[block.ChainID]
	if !ok {
		return nil, nil
	}

	poolResult, err := client.Call(
		ctx,
		block,
		deployment.PoolRegistry,
		venusPoolRegistryABI,
		"getAllPools",
	)
	if err != nil {
		return nil, fmt.Errorf("enumerate isolated pools: %w", err)
	}
	if len(poolResult) != 1 {
		return nil, fmt.Errorf("getAllPools returned %d fields", len(poolResult))
	}
	converted := abi.ConvertType(poolResult[0], new([]venusPool))
	pools, ok := converted.(*[]venusPool)
	if !ok || pools == nil {
		return nil, fmt.Errorf("unexpected pool registry type %T", poolResult[0])
	}
	if len(*pools) > 128 {
		return nil, fmt.Errorf("isolated pool count %d exceeds safety bound", len(*pools))
	}

	comptrollers := make([]common.Address, 0, len(*pools))
	marketCalls := make([]ContractCall, 0, len(*pools))
	for _, pool := range *pools {
		comptrollers = append(comptrollers, pool.Comptroller)
		marketCalls = append(marketCalls, ContractCall{
			Contract: pool.Comptroller,
			ABI:      comptrollerABI,
			Method:   "getAllMarkets",
		})
	}
	marketResults, err := client.ParallelCalls(ctx, block, marketCalls)
	if err != nil {
		return nil, fmt.Errorf("enumerate isolated markets: %w", err)
	}
	isolatedMarkets := make([]common.Address, 0)
	seenMarkets := make(map[common.Address]struct{})
	for _, result := range marketResults {
		if len(result) != 1 {
			return nil, fmt.Errorf("getAllMarkets returned %d fields", len(result))
		}
		markets, decodeErr := decodeAddresses(result[0])
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, market := range markets {
			if _, exists := seenMarkets[market]; exists {
				return nil, fmt.Errorf("duplicate isolated market %s", market)
			}
			seenMarkets[market] = struct{}{}
			isolatedMarkets = append(isolatedMarkets, market)
		}
	}

	groups := make([]Group, 0)
	if deployment.Core != nil {
		coreMarketResult, coreErr := client.Call(
			ctx,
			block,
			deployment.Core.Comptroller,
			comptrollerABI,
			"getAllMarkets",
		)
		if coreErr != nil {
			return nil, fmt.Errorf("enumerate core markets: %w", coreErr)
		}
		coreMarkets, decodeErr := decodeAddresses(coreMarketResult[0])
		if decodeErr != nil {
			return nil, decodeErr
		}
		coreGroup, coreErr := compoundV2MarketGroup(
			ctx,
			client,
			block,
			*deployment.Core,
			account,
			coreMarkets,
		)
		if coreErr != nil {
			return nil, fmt.Errorf("core markets: %w", coreErr)
		}
		if coreGroup != nil {
			coreGroup.ID = "core-lending"
			groups = append(groups, *coreGroup)
		}
	}

	isolatedGroup, err := compoundV2MarketGroup(
		ctx,
		client,
		block,
		compoundV2Deployment{
			Comptroller:   deployment.PoolRegistry,
			WrappedNative: deployment.WrappedNative,
			NativeMarkets: addressSet(),
		},
		account,
		isolatedMarkets,
	)
	if err != nil {
		return nil, fmt.Errorf("isolated markets: %w", err)
	}
	if isolatedGroup != nil {
		isolatedGroup.ID = "isolated-lending"
		groups = append(groups, *isolatedGroup)
	}

	if deployment.Core != nil && deployment.CoreRewardsLens != (common.Address{}) {
		coreReward, rewardErr := venusCoreRewardGroup(
			ctx,
			client,
			block,
			deployment.CoreRewardsLens,
			deployment.Core.Comptroller,
			account,
		)
		if rewardErr != nil {
			return nil, fmt.Errorf("core rewards: %w", rewardErr)
		}
		if coreReward != nil {
			groups = append(groups, *coreReward)
		}
	}
	isolatedReward, err := venusIsolatedRewardGroup(
		ctx,
		client,
		block,
		deployment.PoolLens,
		comptrollers,
		account,
	)
	if err != nil {
		return nil, fmt.Errorf("isolated rewards: %w", err)
	}
	if isolatedReward != nil {
		groups = append(groups, *isolatedReward)
	}

	vaultGroups, err := venusVaultGroups(
		ctx,
		client,
		block,
		deployment.XVSVault,
		account,
	)
	if err != nil {
		return nil, fmt.Errorf("XVS vault: %w", err)
	}
	groups = append(groups, vaultGroups...)

	if deployment.Core != nil && deployment.VAI != nil {
		vaiResult, vaiErr := client.Call(
			ctx,
			block,
			deployment.Core.Comptroller,
			venusCoreComptrollerABI,
			"mintedVAIs",
			account,
		)
		if vaiErr != nil {
			return nil, fmt.Errorf("VAI debt: %w", vaiErr)
		}
		vaiDebt, vaiErr := BigIntAt(vaiResult, 0)
		if vaiErr != nil {
			return nil, fmt.Errorf("VAI debt: %w", vaiErr)
		}
		if vaiDebt.Sign() > 0 {
			groups = append(groups, Group{
				ID:    "vai-debt",
				Label: "VAI debt",
				Components: []Component{NewComponent(
					"debt",
					*deployment.VAI,
					vaiDebt,
					Source{
						Contract: deployment.Core.Comptroller,
						Method:   "mintedVAIs",
					},
				)},
			})
		}
	}
	return groups, nil
}
