package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var yearnV3ABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"pricePerShare","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"earned","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

var yearnV3StakingABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakingToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"rewardTokensLength","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"rewardTokens","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"earned","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"rewardToken","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type yearnManifestToken struct {
	Address  common.Address `json:"address"`
	Symbol   string         `json:"symbol"`
	Decimals uint8          `json:"decimals"`
}

type yearnManifestStaking struct {
	Address         common.Address       `json:"address"`
	ActivationBlock uint64               `json:"activationBlock"`
	Source          string               `json:"source"`
	Rewards         []yearnManifestToken `json:"rewards"`
}

type yearnManifestVault struct {
	Address         common.Address        `json:"address"`
	Version         string                `json:"version"`
	Kind            string                `json:"kind"`
	ActivationBlock uint64                `json:"activationBlock"`
	Token           yearnManifestToken    `json:"token"`
	Staking         *yearnManifestStaking `json:"staking"`
}

type yearnManifest struct {
	Version     int                  `json:"version"`
	ChainID     ChainID              `json:"chainId"`
	GeneratedAt string               `json:"generatedAt"`
	Sources     json.RawMessage      `json:"sources"`
	Vaults      []yearnManifestVault `json:"vaults"`
}

//go:embed yearn_v3_markets.json
var yearnV3ManifestJSON []byte

//go:embed yearn_v3_base_markets.json
var yearnV3BaseManifestJSON []byte

//go:embed yearn_v3_arbitrum_markets.json
var yearnV3ArbitrumManifestJSON []byte

var yearnV3Deployments = mustYearnManifests(map[ChainID][]byte{
	Ethereum: yearnV3ManifestJSON,
	Base:     yearnV3BaseManifestJSON,
	Arbitrum: yearnV3ArbitrumManifestJSON,
})

func mustYearnManifests(payloads map[ChainID][]byte) map[ChainID]yearnManifest {
	deployments := make(map[ChainID]yearnManifest, len(payloads))
	for chainID, payload := range payloads {
		deployments[chainID] = mustYearnManifest(payload, chainID)
	}
	return deployments
}

func mustYearnManifest(payload []byte, expectedChainID ChainID) yearnManifest {
	var manifest yearnManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		panic(fmt.Errorf("decode Yearn v3 manifest: %w", err))
	}
	if manifest.Version != 3 || manifest.ChainID != expectedChainID || len(manifest.Sources) == 0 || len(manifest.Vaults) == 0 {
		panic(fmt.Errorf(
			"unsupported Yearn v3 manifest version=%d chain=%d vaults=%d",
			manifest.Version,
			manifest.ChainID,
			len(manifest.Vaults),
		))
	}
	seen := make(map[common.Address]struct{}, len(manifest.Vaults))
	for _, vault := range manifest.Vaults {
		if vault.Address == (common.Address{}) || vault.Token.Address == (common.Address{}) {
			panic("Yearn v3 manifest contains a zero address")
		}
		if vault.ActivationBlock == 0 {
			panic(fmt.Sprintf("Yearn vault %s has no deployment anchor", vault.Address))
		}
		if _, exists := seen[vault.Address]; exists {
			panic(fmt.Sprintf("Yearn v3 manifest contains duplicate vault %s", vault.Address))
		}
		seen[vault.Address] = struct{}{}
		if vault.Staking != nil {
			if vault.Staking.Address == (common.Address{}) || vault.Staking.ActivationBlock == 0 {
				panic(fmt.Sprintf("Yearn staking for vault %s has no deployment anchor", vault.Address))
			}
			if vault.Staking.ActivationBlock < vault.ActivationBlock {
				panic(fmt.Sprintf("Yearn staking for vault %s predates its vault", vault.Address))
			}
			if vault.Staking.Source != "VeYFI" && vault.Staking.Source != "V3 Staking" {
				panic(fmt.Sprintf("unsupported Yearn staking source %q", vault.Staking.Source))
			}
			if len(vault.Staking.Rewards) == 0 {
				panic(fmt.Sprintf("Yearn staking for vault %s has no reward manifest", vault.Address))
			}
			for _, reward := range vault.Staking.Rewards {
				if reward.Address == (common.Address{}) {
					panic(fmt.Sprintf("Yearn staking for vault %s has a zero reward token", vault.Address))
				}
			}
		}
	}
	return manifest
}

type yearnV3Adapter struct {
	adapterBase
	vaults  map[ChainID][]yearnManifestVault
	byVault map[ChainID]map[common.Address]yearnManifestVault
}

func newYearnV3Adapter() Adapter {
	vaults := make(map[ChainID][]yearnManifestVault, len(yearnV3Deployments))
	byVault := make(map[ChainID]map[common.Address]yearnManifestVault, len(yearnV3Deployments))
	chains := make([]ChainID, 0, len(yearnV3Deployments))
	for _, chainID := range SupportedChainIDs {
		deployment, exists := yearnV3Deployments[chainID]
		if !exists {
			continue
		}
		chains = append(chains, chainID)
		vaults[chainID] = deployment.Vaults
		byVault[chainID] = make(map[common.Address]yearnManifestVault, len(deployment.Vaults))
		for _, vault := range deployment.Vaults {
			byVault[chainID][vault.Address] = vault
		}
	}
	return &yearnV3Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "yearn-v3", Name: "Yearn V3", Chains: chains,
		}},
		vaults:  vaults,
		byVault: byVault,
	}
}

func activeYearnVaults(vaults []yearnManifestVault, block uint64) []yearnManifestVault {
	active := make([]yearnManifestVault, 0, len(vaults))
	for _, vault := range vaults {
		if vault.ActivationBlock <= block {
			active = append(active, vault)
		}
	}
	return active
}

func (s *yearnManifestStaking) activeAt(block uint64) bool {
	return s != nil && s.ActivationBlock <= block
}

func yearnToken(chainID ChainID, config yearnManifestToken) Token {
	return Token{
		ChainID: chainID, Address: config.Address, Symbol: config.Symbol, Decimals: config.Decimals,
	}
}

func (a *yearnV3Adapter) convertShares(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	vault yearnManifestVault,
	shares *big.Int,
	visited map[common.Address]struct{},
) (*big.Int, Token, []common.Address, error) {
	if _, exists := visited[vault.Address]; exists {
		return nil, Token{}, nil, fmt.Errorf("Yearn vault nesting contains cycle at %s", vault.Address)
	}
	visited[vault.Address] = struct{}{}
	defer delete(visited, vault.Address)
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: vault.Address, ABI: yearnV3ABI, Method: "pricePerShare"},
		{Contract: vault.Address, ABI: yearnV3ABI, Method: "decimals"},
		{Contract: vault.Address, ABI: yearnV3ABI, Method: "asset"},
	})
	if err != nil {
		return nil, Token{}, nil, fmt.Errorf("vault %s conversion state: %w", vault.Address, err)
	}
	pricePerShare, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, Token{}, nil, fmt.Errorf("vault %s pricePerShare: %w", vault.Address, err)
	}
	shareDecimals, err := Uint8At(rows[1], 0)
	if err != nil {
		return nil, Token{}, nil, fmt.Errorf("vault %s decimals: %w", vault.Address, err)
	}
	asset, err := AddressAt(rows[2], 0)
	if err != nil {
		return nil, Token{}, nil, fmt.Errorf("vault %s asset: %w", vault.Address, err)
	}
	if asset != vault.Token.Address {
		return nil, Token{}, nil, fmt.Errorf(
			"Yearn vault %s asset changed from %s to %s",
			vault.Address,
			vault.Token.Address,
			asset,
		)
	}
	amount := new(big.Int).Mul(shares, pricePerShare)
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(shareDecimals)), nil)
	amount.Div(amount, denominator)
	if nested, exists := a.byVault[block.ChainID][asset]; exists {
		nestedAmount, token, path, nestedErr := a.convertShares(
			ctx,
			client,
			block,
			nested,
			amount,
			visited,
		)
		return nestedAmount, token, append([]common.Address{vault.Address}, path...), nestedErr
	}
	return amount, yearnToken(block.ChainID, vault.Token), []common.Address{vault.Address}, nil
}

func (a *yearnV3Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := yearnV3Deployments[block.ChainID]
	if !exists {
		return nil, nil
	}
	vaults := activeYearnVaults(a.vaults[block.ChainID], block.Number)
	directCalls := make([]ContractCall, 0, len(vaults))
	stakedVaults := make([]yearnManifestVault, 0, 8)
	for _, vault := range vaults {
		directCalls = append(directCalls, ContractCall{
			Contract: vault.Address, ABI: yearnV3ABI, Method: "balanceOf", Args: []any{account},
		})
		if vault.Staking.activeAt(block.Number) {
			stakedVaults = append(stakedVaults, vault)
		}
	}
	directRows, err := client.ParallelCalls(ctx, block, directCalls)
	if err != nil {
		return nil, fmt.Errorf("Yearn direct balances: %w", err)
	}
	type stakingPlan struct {
		vault  yearnManifestVault
		offset int
		count  int
	}
	stakingCalls := make([]ContractCall, 0, len(stakedVaults)*4)
	stakingPlans := make([]stakingPlan, 0, len(stakedVaults))
	for _, vault := range stakedVaults {
		plan := stakingPlan{vault: vault, offset: len(stakingCalls)}
		switch vault.Staking.Source {
		case "VeYFI":
			stakingCalls = append(stakingCalls,
				ContractCall{Contract: vault.Staking.Address, ABI: yearnV3ABI, Method: "balanceOf", Args: []any{account}},
				ContractCall{Contract: vault.Staking.Address, ABI: yearnV3ABI, Method: "earned", Args: []any{account}},
			)
		case "V3 Staking":
			stakingCalls = append(stakingCalls,
				ContractCall{Contract: vault.Staking.Address, ABI: yearnV3StakingABI, Method: "stakingToken"},
				ContractCall{Contract: vault.Staking.Address, ABI: yearnV3StakingABI, Method: "rewardTokensLength"},
				ContractCall{Contract: vault.Staking.Address, ABI: yearnV3StakingABI, Method: "balanceOf", Args: []any{account}},
			)
			for rewardIndex, reward := range vault.Staking.Rewards {
				stakingCalls = append(stakingCalls,
					ContractCall{
						Contract: vault.Staking.Address,
						ABI:      yearnV3StakingABI,
						Method:   "rewardTokens",
						Args:     []any{big.NewInt(int64(rewardIndex))},
					},
					ContractCall{
						Contract: vault.Staking.Address,
						ABI:      yearnV3StakingABI,
						Method:   "earned",
						Args:     []any{account, reward.Address},
					},
				)
			}
		default:
			return nil, fmt.Errorf("Yearn staking %s has unsupported source %q", vault.Staking.Address, vault.Staking.Source)
		}
		plan.count = len(stakingCalls) - plan.offset
		stakingPlans = append(stakingPlans, plan)
	}
	stakingRows, err := client.ParallelCalls(ctx, block, stakingCalls)
	if err != nil {
		return nil, fmt.Errorf("Yearn staking balances: %w", err)
	}
	type stakingState struct {
		shares  *big.Int
		rewards []*big.Int
	}
	stakingByVault := make(map[common.Address]stakingState, len(stakedVaults))
	for _, plan := range stakingPlans {
		vault := plan.vault
		rows := stakingRows[plan.offset : plan.offset+plan.count]
		state := stakingState{rewards: make([]*big.Int, len(vault.Staking.Rewards))}
		switch vault.Staking.Source {
		case "VeYFI":
			if len(vault.Staking.Rewards) != 1 {
				return nil, fmt.Errorf("Yearn VeYFI staking %s changed its reward set", vault.Staking.Address)
			}
			state.shares, err = BigIntAt(rows[0], 0)
			if err != nil {
				return nil, fmt.Errorf("Yearn staking %s shares: %w", vault.Staking.Address, err)
			}
			state.rewards[0], err = BigIntAt(rows[1], 0)
			if err != nil {
				return nil, fmt.Errorf("Yearn staking %s reward: %w", vault.Staking.Address, err)
			}
		case "V3 Staking":
			stakingToken, decodeErr := AddressAt(rows[0], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Yearn staking %s token: %w", vault.Staking.Address, decodeErr)
			}
			if stakingToken != vault.Address {
				return nil, fmt.Errorf("Yearn staking %s token changed to %s", vault.Staking.Address, stakingToken)
			}
			rewardCount, decodeErr := BigIntAt(rows[1], 0)
			if decodeErr != nil || !rewardCount.IsUint64() || rewardCount.Uint64() != uint64(len(vault.Staking.Rewards)) {
				return nil, fmt.Errorf(
					"Yearn staking %s reward count changed: got %v, want %d",
					vault.Staking.Address, rewardCount, len(vault.Staking.Rewards),
				)
			}
			state.shares, decodeErr = BigIntAt(rows[2], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Yearn staking %s shares: %w", vault.Staking.Address, decodeErr)
			}
			for rewardIndex, reward := range vault.Staking.Rewards {
				rewardToken, tokenErr := AddressAt(rows[3+rewardIndex*2], 0)
				if tokenErr != nil {
					return nil, fmt.Errorf("Yearn staking %s reward token %d: %w", vault.Staking.Address, rewardIndex, tokenErr)
				}
				if rewardToken != reward.Address {
					return nil, fmt.Errorf(
						"Yearn staking %s reward token %d changed to %s",
						vault.Staking.Address, rewardIndex, rewardToken,
					)
				}
				state.rewards[rewardIndex], tokenErr = BigIntAt(rows[4+rewardIndex*2], 0)
				if tokenErr != nil {
					return nil, fmt.Errorf("Yearn staking %s reward %d: %w", vault.Staking.Address, rewardIndex, tokenErr)
				}
			}
		}
		stakingByVault[vault.Address] = state
	}

	groups := make([]Group, 0)
	for index, vault := range vaults {
		directShares, decodeErr := BigIntAt(directRows[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Yearn vault %s balance: %w", vault.Address, decodeErr)
		}
		state := stakingByVault[vault.Address]
		if state.shares == nil {
			state.shares = new(big.Int)
		}
		hasReward := false
		for _, reward := range state.rewards {
			if reward != nil && reward.Sign() > 0 {
				hasReward = true
				break
			}
		}
		if directShares.Sign() == 0 && state.shares.Sign() == 0 && !hasReward {
			continue
		}
		stakedVaultShares := new(big.Int)
		if state.shares.Sign() > 0 {
			if vault.Staking.Source == "V3 Staking" {
				stakedVaultShares.Set(state.shares)
			} else {
				rows, callErr := client.ParallelCalls(ctx, block, []ContractCall{
					{Contract: vault.Staking.Address, ABI: yearnV3ABI, Method: "asset"},
					{Contract: vault.Staking.Address, ABI: yearnV3ABI, Method: "convertToAssets", Args: []any{state.shares}},
				})
				if callErr != nil {
					return nil, fmt.Errorf("Yearn staking %s conversion: %w", vault.Staking.Address, callErr)
				}
				asset, decodeErr := AddressAt(rows[0], 0)
				if decodeErr != nil {
					return nil, fmt.Errorf("Yearn staking %s asset: %w", vault.Staking.Address, decodeErr)
				}
				if asset != vault.Address {
					return nil, fmt.Errorf("Yearn staking %s asset changed to %s", vault.Staking.Address, asset)
				}
				stakedVaultShares, decodeErr = BigIntAt(rows[1], 0)
				if decodeErr != nil {
					return nil, fmt.Errorf("Yearn staking %s converted shares: %w", vault.Staking.Address, decodeErr)
				}
			}
		}
		totalShares := new(big.Int).Add(directShares, stakedVaultShares)
		components := make([]Component, 0, 1+len(state.rewards))
		if totalShares.Sign() > 0 {
			amount, underlying, path, conversionErr := a.convertShares(
				ctx,
				client,
				block,
				vault,
				totalShares,
				make(map[common.Address]struct{}),
			)
			if conversionErr != nil {
				return nil, conversionErr
			}
			if amount.Sign() > 0 {
				component := NewComponent(
					"asset",
					underlying,
					amount,
					Source{Contract: vault.Address, Method: "balanceOf*pricePerShare"},
				)
				pathStrings := make([]string, len(path))
				for index, address := range path {
					pathStrings[index] = address.Hex()
				}
				component.Metadata = map[string]any{
					"directShares":      directShares.String(),
					"stakedVaultShares": stakedVaultShares.String(),
					"conversionPath":    strings.Join(pathStrings, ","),
				}
				components = append(components, component)
			}
		}
		for rewardIndex, rewardAmount := range state.rewards {
			if rewardAmount == nil || rewardAmount.Sign() == 0 {
				continue
			}
			method := "earned(account)"
			if vault.Staking.Source == "V3 Staking" {
				method = "earned(account,rewardToken)"
			}
			components = append(components, NewComponent(
				"reward",
				yearnToken(block.ChainID, vault.Staking.Rewards[rewardIndex]),
				rewardAmount,
				Source{Contract: vault.Staking.Address, Method: method},
			))
		}
		if len(components) > 0 {
			id := strings.ToLower(vault.Address.Hex())
			groups = append(groups, Group{
				ID: id, MarketID: id, Label: "Yearn " + vault.Token.Symbol, Components: components,
				Metadata: map[string]any{
					"vault": vault.Address, "version": vault.Version, "kind": vault.Kind,
					"manifestGeneratedAt": deployment.GeneratedAt,
				},
			})
		}
	}
	return groups, nil
}
