package portfolio

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	eulerMaxPositionRefs = 4_096
	eulerMaxRewardRefs   = 16_384
)

type eulerVaultKind string

const (
	eulerEVault     eulerVaultKind = "evault"
	eulerEarn       eulerVaultKind = "euler-earn"
	eulerSecuritize eulerVaultKind = "securitize"
)

type eulerPositionRef struct {
	Account      common.Address
	Vault        common.Address
	Kind         eulerVaultKind
	RewardTokens []common.Address
}

func validEulerVaultKind(kind eulerVaultKind) bool {
	return kind == eulerEVault || kind == eulerEarn || kind == eulerSecuritize
}

// eulerIndexer reads the Euler index, keyed by owner prefix: an EVC owner's 256 sub-accounts share
// the first 19 bytes of their addresses, and the index lists the positions of all of them.
type eulerIndexer struct {
	index *sentioIndex
}

func newEulerIndexer(config SentioIndexerConfig) *eulerIndexer {
	return &eulerIndexer{index: &sentioIndex{
		name:   "euler-v2",
		api:    newSentioAPIClient(),
		config: config,
		chains: deploymentChains(eulerV2ChainConfigs),
		start:  func(chainID ChainID) uint64 { return eulerV2ChainConfigs[chainID].ActivationBlock },
	}}
}

func eulerOwnerPrefix(account common.Address) string {
	return strings.ToLower(account.Hex())[:40]
}

// eulerOwnerRowPrefix starts the ID of every position and reward row of the accounts that share
// owner's prefix on the chain.
func eulerOwnerRowPrefix(chainID ChainID, owner common.Address) string {
	return fmt.Sprintf("%d:%s:", chainID, eulerOwnerPrefix(owner))
}

func eulerPositionRowID(chainID ChainID, account, vault common.Address) string {
	return eulerOwnerRowPrefix(chainID, account) + strings.ToLower(account.Hex()) + ":" + strings.ToLower(vault.Hex())
}

func eulerRewardRowID(chainID ChainID, account, vault, reward common.Address) string {
	return eulerPositionRowID(chainID, account, vault) + ":" + strings.ToLower(reward.Hex())
}

// eulerOwnerWhere selects the rows of every account sharing owner's prefix on a chain.
func eulerOwnerWhere(owner common.Address) func(ChainID) indexWhere {
	return func(chainID ChainID) indexWhere {
		return indexWhere{Prefix: eulerOwnerRowPrefix(chainID, owner), Terms: []indexTerm{
			{"chainId", int(chainID)}, {"ownerPrefix", eulerOwnerPrefix(owner)},
		}}
	}
}

type eulerPositionRow struct {
	ChainID     int    `json:"chainId"`
	OwnerPrefix string `json:"ownerPrefix"`
	Account     string `json:"account"`
	Vault       string `json:"vault"`
	VaultKind   string `json:"vaultKind"`
}

func eulerPositionStream(owner common.Address) *indexStream[eulerPositionRow, eulerPositionRef] {
	prefix := eulerOwnerPrefix(owner)
	return &indexStream[eulerPositionRow, eulerPositionRef]{
		Entity: "eulerPositionRefs", MaxRows: eulerMaxPositionRefs, Where: eulerOwnerWhere(owner),
		Decode: func(chainID ChainID, id string, row eulerPositionRow) (eulerPositionRef, error) {
			if row.ChainID != int(chainID) || row.OwnerPrefix != prefix ||
				!common.IsHexAddress(row.Account) || !common.IsHexAddress(row.Vault) {
				return eulerPositionRef{}, fmt.Errorf("GraphQL returned malformed Euler position row %q", id)
			}
			account := common.HexToAddress(row.Account)
			vault := common.HexToAddress(row.Vault)
			kind := eulerVaultKind(row.VaultKind)
			if eulerOwnerPrefix(account) != prefix || !validEulerVaultKind(kind) ||
				id != eulerPositionRowID(chainID, account, vault) {
				return eulerPositionRef{}, fmt.Errorf("GraphQL returned foreign Euler position row %q", id)
			}
			return eulerPositionRef{Account: account, Vault: vault, Kind: kind}, nil
		},
	}
}

type eulerRewardRow struct {
	ChainID     int    `json:"chainId"`
	OwnerPrefix string `json:"ownerPrefix"`
	Account     string `json:"account"`
	Vault       string `json:"vault"`
	Reward      string `json:"reward"`
}

// eulerReward is a reward token an account's position in a vault is enrolled for.
type eulerReward struct {
	Account common.Address
	Vault   common.Address
	Reward  common.Address
}

func eulerRewardStream(owner common.Address) *indexStream[eulerRewardRow, eulerReward] {
	prefix := eulerOwnerPrefix(owner)
	return &indexStream[eulerRewardRow, eulerReward]{
		Entity: "eulerRewardTokens", MaxRows: eulerMaxRewardRefs, Where: eulerOwnerWhere(owner),
		Decode: func(chainID ChainID, id string, row eulerRewardRow) (eulerReward, error) {
			if row.ChainID != int(chainID) || row.OwnerPrefix != prefix ||
				!common.IsHexAddress(row.Account) || !common.IsHexAddress(row.Vault) || !common.IsHexAddress(row.Reward) {
				return eulerReward{}, fmt.Errorf("GraphQL returned malformed Euler reward row %q", id)
			}
			reward := eulerReward{
				Account: common.HexToAddress(row.Account),
				Vault:   common.HexToAddress(row.Vault),
				Reward:  common.HexToAddress(row.Reward),
			}
			if eulerOwnerPrefix(reward.Account) != prefix ||
				id != eulerRewardRowID(chainID, reward.Account, reward.Vault, reward.Reward) {
				return eulerReward{}, fmt.Errorf("GraphQL returned foreign Euler reward row %q", id)
			}
			return reward, nil
		},
	}
}

// PositionRefs returns the position refs of every account sharing owner's prefix on block's
// chain, each with the reward tokens it is enrolled for, in row order.
func (i *eulerIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]eulerPositionRef, error) {
	positions, rewards := eulerPositionStream(owner), eulerRewardStream(owner)
	if _, err := i.index.read(ctx, client, block, positions, rewards); err != nil {
		return nil, err
	}
	enrolled := make(map[[2]common.Address][]common.Address)
	for _, reward := range rewards.Rows() {
		key := [2]common.Address{reward.Account, reward.Vault}
		enrolled[key] = append(enrolled[key], reward.Reward)
	}
	refs := positions.Rows()
	for index := range refs {
		tokens := enrolled[[2]common.Address{refs[index].Account, refs[index].Vault}]
		sort.Slice(tokens, func(left, right int) bool {
			return strings.ToLower(tokens[left].Hex()) < strings.ToLower(tokens[right].Hex())
		})
		refs[index].RewardTokens = tokens
	}
	return refs, nil
}
