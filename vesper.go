package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var vesperPoolABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"totalValue","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"totalSupply","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"token","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"poolRewards","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var vesperRewardsABI = MustABI(`[
  {"type":"function","name":"claimable","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"address[]"},{"type":"uint256[]"}]}
]`)

var vesperLegacyRewardsABI = MustABI(`[
  {"type":"function","name":"claimable","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"rewardToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var vesperLockedVSPABI = MustABI(`[
  {"type":"function","name":"locked","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"rewards","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var vesperLockedRewardsABI = MustABI(`[
  {"type":"function","name":"claimableRewards","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"address[]"},{"type":"uint256[]"}]}
]`)

var (
	vesperLockedVSP       = common.HexToAddress("0xD02d6eC21851092A9cca8a8eb388fdF66bA96F9B")
	vesperReth            = common.HexToAddress("0xae78736Cd615f374D3085123A210448E74Fc6393")
	vesperWrappedEther    = token(Ethereum, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "ETH", 18)
	vesperVSP             = token(Ethereum, "0x1b40183EFB4Dd766f11bDa7A7c3AD8982e998421", "VSP", 18)
	vesperLockedVSPWindow = deploymentWindow{ActivationBlock: 17_741_140}
)

type vesperManifestToken struct {
	Address  common.Address `json:"address"`
	Symbol   string         `json:"symbol"`
	Decimals uint8          `json:"decimals"`
}

type vesperManifestPool struct {
	ChainID         ChainID             `json:"chainId"`
	Name            string              `json:"name"`
	Address         common.Address      `json:"address"`
	ActivationBlock uint64              `json:"activationBlock"`
	Version         int                 `json:"version"`
	Status          string              `json:"status"`
	Type            string              `json:"type"`
	Token           vesperManifestToken `json:"token"`
}

type vesperManifest struct {
	Version      int                  `json:"version"`
	GeneratedAt  string               `json:"generatedAt"`
	Source       string               `json:"source"`
	SourceCommit string               `json:"sourceCommit"`
	Pools        []vesperManifestPool `json:"pools"`
}

//go:embed vesper-pools.json
var vesperManifestJSON []byte

var vesperDeployments = mustVesperManifest()

func mustVesperManifest() vesperManifest {
	var manifest vesperManifest
	if err := json.Unmarshal(vesperManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode Vesper manifest: %w", err))
	}
	if manifest.Version != 1 || manifest.GeneratedAt == "" || manifest.Source == "" ||
		manifest.SourceCommit == "" || len(manifest.Pools) == 0 {
		panic(fmt.Sprintf("invalid Vesper manifest version=%d pools=%d", manifest.Version, len(manifest.Pools)))
	}
	seen := make(map[ChainID]map[common.Address]struct{})
	for _, pool := range manifest.Pools {
		if !supportsChain(SupportedChainIDs, pool.ChainID) || pool.Name == "" ||
			pool.Address == (common.Address{}) || pool.ActivationBlock == 0 || pool.Status == "" ||
			pool.Type == "" || pool.Token.Address == (common.Address{}) || pool.Token.Symbol == "" {
			panic(fmt.Sprintf("invalid Vesper pool %s on chain %d", pool.Address, pool.ChainID))
		}
		if pool.Version != 0 && pool.Version != 3 && pool.Version != 4 && pool.Version != 5 {
			panic(fmt.Sprintf("unsupported Vesper pool version %d for %s", pool.Version, pool.Address))
		}
		if seen[pool.ChainID] == nil {
			seen[pool.ChainID] = make(map[common.Address]struct{})
		}
		if _, exists := seen[pool.ChainID][pool.Address]; exists {
			panic(fmt.Sprintf("duplicate Vesper pool %s on chain %d", pool.Address, pool.ChainID))
		}
		seen[pool.ChainID][pool.Address] = struct{}{}
	}
	return manifest
}

type VesperAdapter struct {
	adapterBase
	pools map[ChainID][]vesperManifestPool
}

func newVesperAdapter() Adapter {
	pools := make(map[ChainID][]vesperManifestPool)
	for _, pool := range vesperDeployments.Pools {
		pools[pool.ChainID] = append(pools[pool.ChainID], pool)
	}
	chains := deploymentChains(pools)
	for _, chainID := range chains {
		sort.Slice(pools[chainID], func(left, right int) bool {
			return strings.ToLower(pools[chainID][left].Address.Hex()) <
				strings.ToLower(pools[chainID][right].Address.Hex())
		})
	}
	return &VesperAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "vesper", Name: "Vesper", Chains: chains}},
		pools:       pools,
	}
}

func activeVesperPools(pools []vesperManifestPool, block uint64) []vesperManifestPool {
	active := make([]vesperManifestPool, 0, len(pools))
	for _, pool := range pools {
		if pool.ActivationBlock <= block {
			active = append(active, pool)
		}
	}
	return active
}

func vesperAmounts(value any) ([]*big.Int, error) {
	converted := abi.ConvertType(value, new([]*big.Int))
	amounts, ok := converted.(*[]*big.Int)
	if !ok || amounts == nil {
		return nil, fmt.Errorf("unexpected amount list type %T", value)
	}
	return *amounts, nil
}

func vesperUnderlyingAmount(shares, totalValue, totalSupply *big.Int) (*big.Int, error) {
	if totalSupply.Sign() <= 0 {
		return nil, fmt.Errorf("pool total supply is zero with non-zero account shares")
	}
	return new(big.Int).Div(new(big.Int).Mul(shares, totalValue), totalSupply), nil
}

type vesperPoolState struct {
	pool       vesperManifestPool
	shares     *big.Int
	rewards    common.Address
	underlying *big.Int
	token      Token
	source     Source
}

type vesperRewardState struct {
	pool    vesperManifestPool
	address common.Address
	tokens  []common.Address
	amounts []*big.Int
}

func (a *VesperAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	pools := activeVesperPools(a.pools[block.ChainID], block.Number)
	if len(pools) == 0 {
		return nil, nil
	}
	headerCalls := make([]ContractCall, 0, len(pools)*2)
	for _, pool := range pools {
		headerCalls = append(headerCalls, ContractCall{
			Contract: pool.Address, ABI: vesperPoolABI, Method: "balanceOf", Args: []any{account},
		})
		if pool.Version >= 5 {
			headerCalls = append(headerCalls, ContractCall{
				Contract: pool.Address, ABI: vesperPoolABI, Method: "poolRewards",
			})
		}
	}
	headers, err := client.ParallelCalls(ctx, block, headerCalls)
	if err != nil {
		return nil, fmt.Errorf("Vesper pool headers: %w", err)
	}
	states := make([]vesperPoolState, len(pools))
	detailCalls := make([]ContractCall, 0)
	heldIndexes := make([]int, 0)
	rewardCalls := make([]ContractCall, 0)
	rewardIndexes := make([]int, 0)
	headerIndex := 0
	for index, pool := range pools {
		shares, decodeErr := BigIntAt(headers[headerIndex], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s balance: %w", pool.Address, decodeErr)
		}
		headerIndex++
		rewards := common.Address{}
		if pool.Version >= 5 {
			rewards, decodeErr = AddressAt(headers[headerIndex], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Vesper pool %s rewards: %w", pool.Address, decodeErr)
			}
			headerIndex++
		}
		states[index] = vesperPoolState{pool: pool, shares: shares, rewards: rewards}
		if shares.Sign() > 0 {
			heldIndexes = append(heldIndexes, index)
			detailCalls = append(detailCalls,
				ContractCall{Contract: pool.Address, ABI: vesperPoolABI, Method: "token"},
				ContractCall{Contract: pool.Address, ABI: vesperPoolABI, Method: "totalValue"},
				ContractCall{Contract: pool.Address, ABI: vesperPoolABI, Method: "totalSupply"},
			)
		}
		// DeBank attributes pool rewards only while the account still owns pool shares.
		// Residual claimable rewards after a full exit are not a visible Vesper position.
		if shares.Sign() > 0 && rewards != (common.Address{}) {
			rewardIndexes = append(rewardIndexes, index)
			rewardCalls = append(rewardCalls, ContractCall{
				Contract: rewards, ABI: vesperRewardsABI, Method: "claimable", Args: []any{account},
			})
		}
	}
	details, err := client.ParallelCalls(ctx, block, detailCalls)
	if err != nil {
		return nil, fmt.Errorf("Vesper held-pool state: %w", err)
	}
	for detailIndex, stateIndex := range heldIndexes {
		asset, decodeErr := AddressAt(details[detailIndex*3], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s token: %w", states[stateIndex].pool.Address, decodeErr)
		}
		if asset != states[stateIndex].pool.Token.Address {
			return nil, fmt.Errorf(
				"Vesper pool %s token changed from %s to %s",
				states[stateIndex].pool.Address,
				states[stateIndex].pool.Token.Address,
				asset,
			)
		}
		totalValue, decodeErr := BigIntAt(details[detailIndex*3+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s totalValue: %w", states[stateIndex].pool.Address, decodeErr)
		}
		totalSupply, decodeErr := BigIntAt(details[detailIndex*3+2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s totalSupply: %w", states[stateIndex].pool.Address, decodeErr)
		}
		states[stateIndex].underlying, decodeErr = vesperUnderlyingAmount(
			states[stateIndex].shares, totalValue, totalSupply,
		)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s conversion: %w", states[stateIndex].pool.Address, decodeErr)
		}
		states[stateIndex].token = Token{
			ChainID: block.ChainID, Address: states[stateIndex].pool.Token.Address,
			Symbol: states[stateIndex].pool.Token.Symbol, Decimals: states[stateIndex].pool.Token.Decimals,
		}
		states[stateIndex].source = Source{
			Contract: states[stateIndex].pool.Address, Method: "balanceOf*totalValue/totalSupply",
		}
		if block.ChainID == Ethereum && asset == vesperReth {
			converted, conversionErr := client.Call(
				ctx, block, asset, convertedBalanceABI, "getEthValue", states[stateIndex].underlying,
			)
			if conversionErr != nil {
				return nil, fmt.Errorf("Vesper pool %s rETH conversion: %w", states[stateIndex].pool.Address, conversionErr)
			}
			states[stateIndex].underlying, decodeErr = BigIntAt(converted, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Vesper pool %s rETH conversion: %w", states[stateIndex].pool.Address, decodeErr)
			}
			states[stateIndex].token = vesperWrappedEther
			states[stateIndex].source.Method += "*rETH.getEthValue"
		}
	}
	rewardRows, err := client.ParallelCallsAllowFailure(ctx, block, rewardCalls)
	if err != nil {
		return nil, fmt.Errorf("Vesper rewards: %w", err)
	}
	rewards := make([]vesperRewardState, 0, len(rewardRows))
	rewardTokenAddresses := make([]common.Address, 0)
	legacyCalls := make([]ContractCall, 0)
	legacyIndexes := make([]int, 0)
	for rowIndex, row := range rewardRows {
		state := states[rewardIndexes[rowIndex]]
		if row.Error != nil {
			legacyIndexes = append(legacyIndexes, rewardIndexes[rowIndex])
			legacyCalls = append(legacyCalls,
				ContractCall{
					Contract: state.rewards, ABI: vesperLegacyRewardsABI,
					Method: "claimable", Args: []any{account},
				},
				ContractCall{Contract: state.rewards, ABI: vesperLegacyRewardsABI, Method: "rewardToken"},
			)
			continue
		}
		if len(row.Values) != 2 {
			return nil, fmt.Errorf("Vesper pool %s claimable returned %d fields", state.pool.Address, len(row.Values))
		}
		tokens, decodeErr := decodeAddresses(row.Values[0])
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s reward tokens: %w", state.pool.Address, decodeErr)
		}
		amounts, decodeErr := vesperAmounts(row.Values[1])
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s reward amounts: %w", state.pool.Address, decodeErr)
		}
		if len(tokens) != len(amounts) {
			return nil, fmt.Errorf(
				"Vesper pool %s returned %d reward tokens and %d amounts",
				state.pool.Address,
				len(tokens),
				len(amounts),
			)
		}
		for index, amount := range amounts {
			if amount.Sign() > 0 {
				rewardTokenAddresses = append(rewardTokenAddresses, tokens[index])
			}
		}
		rewards = append(rewards, vesperRewardState{
			pool: state.pool, address: state.rewards, tokens: tokens, amounts: amounts,
		})
	}
	legacyRows, err := client.ParallelCalls(ctx, block, legacyCalls)
	if err != nil {
		return nil, fmt.Errorf("Vesper legacy rewards: %w", err)
	}
	for legacyIndex, stateIndex := range legacyIndexes {
		state := states[stateIndex]
		amount, decodeErr := BigIntAt(legacyRows[legacyIndex*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s legacy reward amount: %w", state.pool.Address, decodeErr)
		}
		rewardToken, decodeErr := AddressAt(legacyRows[legacyIndex*2+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper pool %s legacy reward token: %w", state.pool.Address, decodeErr)
		}
		if amount.Sign() > 0 {
			rewardTokenAddresses = append(rewardTokenAddresses, rewardToken)
		}
		rewards = append(rewards, vesperRewardState{
			pool: state.pool, address: state.rewards,
			tokens: []common.Address{rewardToken}, amounts: []*big.Int{amount},
		})
	}
	rewardTokens, err := readERC20Tokens(ctx, client, block, rewardTokenAddresses)
	if err != nil {
		return nil, fmt.Errorf("Vesper reward token metadata: %w", err)
	}

	groups := make([]Group, 0)
	groupIndexes := make(map[common.Address]int)
	ensureGroup := func(pool vesperManifestPool) *Group {
		if index, exists := groupIndexes[pool.Address]; exists {
			return &groups[index]
		}
		groupIndexes[pool.Address] = len(groups)
		groups = append(groups, Group{
			ID: strings.ToLower(pool.Address.Hex()), MarketID: strings.ToLower(pool.Address.Hex()),
			Label: "Yield · " + pool.Name,
			Metadata: map[string]any{
				"pool": pool.Address, "version": pool.Version, "status": pool.Status, "type": pool.Type,
			},
		})
		return &groups[len(groups)-1]
	}
	for _, state := range states {
		if state.underlying == nil || state.underlying.Sign() == 0 {
			continue
		}
		component := NewComponent("asset", state.token, state.underlying, state.source)
		component.Metadata = map[string]any{"shares": state.shares.String()}
		group := ensureGroup(state.pool)
		group.Components = append(group.Components, component)
	}
	for _, reward := range rewards {
		for index, amount := range reward.amounts {
			if amount.Sign() == 0 {
				continue
			}
			token, exists := rewardTokens[reward.tokens[index]]
			if !exists {
				return nil, fmt.Errorf("Vesper reward token %s metadata is missing", reward.tokens[index])
			}
			group := ensureGroup(reward.pool)
			group.Components = append(group.Components, NewComponent(
				"reward", token, amount,
				Source{Contract: reward.address, Method: "claimable"},
			))
		}
	}
	if block.ChainID == Ethereum && vesperLockedVSPWindow.ActiveAt(block.Number) {
		lockedRows, lockedErr := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: vesperLockedVSP, ABI: vesperLockedVSPABI, Method: "locked", Args: []any{account}},
			{Contract: vesperLockedVSP, ABI: vesperLockedVSPABI, Method: "rewards"},
		})
		if lockedErr != nil {
			return nil, fmt.Errorf("Vesper locked VSP: %w", lockedErr)
		}
		locked, decodeErr := BigIntAt(lockedRows[0], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Vesper locked VSP balance: %w", decodeErr)
		}
		if locked.Sign() > 0 {
			rewardsAddress, decodeErr := AddressAt(lockedRows[1], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Vesper locked VSP rewards: %w", decodeErr)
			}
			components := []Component{NewComponent(
				"asset", vesperVSP, locked,
				Source{Contract: vesperLockedVSP, Method: "locked"},
			)}
			claimableRow, claimableErr := client.Call(
				ctx, block, rewardsAddress, vesperLockedRewardsABI, "claimableRewards", account,
			)
			if claimableErr != nil {
				return nil, fmt.Errorf("Vesper locked VSP claimable rewards: %w", claimableErr)
			}
			tokens, decodeErr := decodeAddresses(claimableRow[0])
			if decodeErr != nil {
				return nil, fmt.Errorf("Vesper locked VSP reward tokens: %w", decodeErr)
			}
			amounts, decodeErr := vesperAmounts(claimableRow[1])
			if decodeErr != nil {
				return nil, fmt.Errorf("Vesper locked VSP reward amounts: %w", decodeErr)
			}
			if len(tokens) != len(amounts) {
				return nil, fmt.Errorf("Vesper locked VSP returned %d tokens and %d amounts", len(tokens), len(amounts))
			}
			metadata, metadataErr := readERC20Tokens(ctx, client, block, tokens)
			if metadataErr != nil {
				return nil, fmt.Errorf("Vesper locked VSP reward metadata: %w", metadataErr)
			}
			for index, amount := range amounts {
				if amount.Sign() == 0 {
					continue
				}
				components = append(components, NewComponent(
					"reward", metadata[tokens[index]], amount,
					Source{Contract: rewardsAddress, Method: "claimableRewards"},
				))
			}
			groups = append(groups, Group{
				ID: strings.ToLower(vesperLockedVSP.Hex()), MarketID: strings.ToLower(vesperLockedVSP.Hex()),
				Label: "Staked · Locked VSP", Components: components,
				Metadata: map[string]any{"staking": vesperLockedVSP, "rewards": rewardsAddress},
			})
		}
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, nil
}
