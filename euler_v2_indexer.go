package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	eulerIndexerPageSize = 500
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

type eulerIndexer struct {
	api    *sentioAPIClient
	config SentioIndexerConfig
}

func newEulerIndexer(config SentioIndexerConfig) *eulerIndexer {
	return &eulerIndexer{api: newSentioAPIClient(), config: config}
}

const eulerWalletQuery = `
query EulerWalletPositions(
  $chainId: Int!
  $ownerPrefix: String!
  $idPrefix: String!
  $positionAfter: ID!
  $rewardAfter: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  eulerPositionRefs(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, ownerPrefix: $ownerPrefix, id_starts_with: $idPrefix, id_gt: $positionAfter }
  ) {
    id
    chainId
    ownerPrefix
    account
    vault
    vaultKind
  }
  eulerRewardTokens(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, ownerPrefix: $ownerPrefix, id_starts_with: $idPrefix, id_gt: $rewardAfter }
  ) {
    id
    chainId
    ownerPrefix
    account
    vault
    reward
  }
}`

type eulerGraphQLResponse struct {
	Data struct {
		Checkpoints []indexerCheckpointRow `json:"indexerCheckpoints"`
		Positions   []struct {
			ID, OwnerPrefix, Account, Vault, VaultKind string
			ChainID                                    int `json:"chainId"`
		} `json:"eulerPositionRefs"`
		Rewards []struct {
			ID, OwnerPrefix, Account, Vault, Reward string
			ChainID                                 int `json:"chainId"`
		} `json:"eulerRewardTokens"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type eulerGraphQLPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	Positions       []eulerPositionRef
	PositionIDs     []string
	Rewards         map[string][]common.Address
	RewardIDs       []string
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

func (i *eulerIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	owner common.Address,
	positionAfter, rewardAfter string,
	block uint64,
	start indexStart,
) (eulerGraphQLPage, error) {
	var payload eulerGraphQLResponse
	err := i.api.doJSON(ctx, http.MethodPost, i.config.GraphQLURL, map[string]any{
		"query": eulerWalletQuery,
		"variables": map[string]any{
			"chainId":       int(chainID),
			"ownerPrefix":   eulerOwnerPrefix(owner),
			"idPrefix":      eulerOwnerRowPrefix(chainID, owner),
			"positionAfter": positionAfter, "rewardAfter": rewardAfter,
			"checkpoint": strconv.FormatUint(uint64(chainID), 10), "first": eulerIndexerPageSize,
			"block": strconv.FormatUint(block, 10),
		},
	}, &payload)
	if err != nil {
		return eulerGraphQLPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return eulerGraphQLPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	checkpointBlock, checkpointMS, err := start.checkpoint(ctx, payload.Data.Checkpoints)
	if err != nil {
		return eulerGraphQLPage{}, err
	}
	prefix := eulerOwnerPrefix(owner)
	page := eulerGraphQLPage{
		CheckpointBlock: checkpointBlock, CheckpointMS: checkpointMS,
		Rewards: make(map[string][]common.Address),
	}
	for _, row := range payload.Data.Positions {
		if row.ChainID != int(chainID) || row.OwnerPrefix != prefix ||
			!common.IsHexAddress(row.Account) || !common.IsHexAddress(row.Vault) || row.ID <= positionAfter {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned malformed Euler position row %q", row.ID)
		}
		account := common.HexToAddress(row.Account)
		vault := common.HexToAddress(row.Vault)
		kind := eulerVaultKind(row.VaultKind)
		if eulerOwnerPrefix(account) != prefix || !validEulerVaultKind(kind) ||
			row.ID != eulerPositionRowID(chainID, account, vault) {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned foreign Euler position row %q", row.ID)
		}
		page.Positions = append(page.Positions, eulerPositionRef{Account: account, Vault: vault, Kind: kind})
		page.PositionIDs = append(page.PositionIDs, row.ID)
	}
	for _, row := range payload.Data.Rewards {
		if row.ChainID != int(chainID) || row.OwnerPrefix != prefix ||
			!common.IsHexAddress(row.Account) || !common.IsHexAddress(row.Vault) ||
			!common.IsHexAddress(row.Reward) || row.ID <= rewardAfter {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned malformed Euler reward row %q", row.ID)
		}
		account := common.HexToAddress(row.Account)
		vault := common.HexToAddress(row.Vault)
		reward := common.HexToAddress(row.Reward)
		if eulerOwnerPrefix(account) != prefix || row.ID != eulerRewardRowID(chainID, account, vault, reward) {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned foreign Euler reward row %q", row.ID)
		}
		key := strings.ToLower(account.Hex()) + ":" + strings.ToLower(vault.Hex())
		page.Rewards[key] = append(page.Rewards[key], reward)
		page.RewardIDs = append(page.RewardIDs, row.ID)
	}
	return page, nil
}

type eulerIndexedSnapshot struct {
	Block     uint64
	Positions []eulerPositionRef
	// Lag notes a live read that trails its pin by more than indexMaxLag.
	Lag *indexLagNote
}

func (i *eulerIndexer) indexedSnapshot(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
	start indexStart,
) (eulerIndexedSnapshot, error) {
	release, err := lockSentioLane(ctx)
	if err != nil {
		return eulerIndexedSnapshot{}, err
	}
	defer release()
	_, indexedBlock, err := indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return i.api.chainStatuses(ctx, i.config, []ChainID{block.ChainID}, forceRefresh)
	})
	if err != nil {
		return eulerIndexedSnapshot{}, err
	}
	result := eulerIndexedSnapshot{Block: indexedBlock}
	positions := make(map[string]eulerPositionRef)
	rewards := make(map[string]map[common.Address]struct{})
	positionAfter, rewardAfter := "", ""
	positionDone, rewardDone := false, false
	for !positionDone || !rewardDone {
		page, pageErr := i.graphqlPage(
			ctx, block.ChainID, owner, positionAfter, rewardAfter, indexedBlock, start,
		)
		if pageErr != nil {
			return eulerIndexedSnapshot{}, pageErr
		}
		if result.Lag, err = checkIndexCheckpoint(block, page.CheckpointBlock, page.CheckpointMS); err != nil {
			return eulerIndexedSnapshot{}, err
		}
		for _, ref := range page.Positions {
			positions[eulerPositionRowID(block.ChainID, ref.Account, ref.Vault)] = ref
		}
		for key, tokens := range page.Rewards {
			if rewards[key] == nil {
				rewards[key] = make(map[common.Address]struct{})
			}
			for _, reward := range tokens {
				rewards[key][reward] = struct{}{}
			}
		}
		if len(positions) > eulerMaxPositionRefs {
			return eulerIndexedSnapshot{}, fmt.Errorf("Euler indexer result exceeds its safety bound")
		}
		rewardCount := 0
		for _, entries := range rewards {
			rewardCount += len(entries)
		}
		if rewardCount > eulerMaxRewardRefs {
			return eulerIndexedSnapshot{}, fmt.Errorf("Euler reward index exceeds its safety bound")
		}
		positionDone = len(page.PositionIDs) < eulerIndexerPageSize
		rewardDone = len(page.RewardIDs) < eulerIndexerPageSize
		if !positionDone {
			next := page.PositionIDs[len(page.PositionIDs)-1]
			if next <= positionAfter {
				return eulerIndexedSnapshot{}, fmt.Errorf("Euler position pagination did not advance")
			}
			positionAfter = next
		}
		if !rewardDone {
			next := page.RewardIDs[len(page.RewardIDs)-1]
			if next <= rewardAfter {
				return eulerIndexedSnapshot{}, fmt.Errorf("Euler reward pagination did not advance")
			}
			rewardAfter = next
		}
	}
	for _, ref := range positions {
		key := strings.ToLower(ref.Account.Hex()) + ":" + strings.ToLower(ref.Vault.Hex())
		for reward := range rewards[key] {
			ref.RewardTokens = append(ref.RewardTokens, reward)
		}
		sort.Slice(ref.RewardTokens, func(left, right int) bool {
			return strings.ToLower(ref.RewardTokens[left].Hex()) < strings.ToLower(ref.RewardTokens[right].Hex())
		})
		result.Positions = append(result.Positions, ref)
	}
	sort.Slice(result.Positions, func(left, right int) bool {
		leftID := eulerPositionRowID(block.ChainID, result.Positions[left].Account, result.Positions[left].Vault)
		rightID := eulerPositionRowID(block.ChainID, result.Positions[right].Account, result.Positions[right].Vault)
		return leftID < rightID
	})
	return result, nil
}

// PositionRefs returns the position refs of every account sharing owner's prefix on block's
// chain, with a lag note when the index trails a live pin by more than indexMaxLag.
func (i *eulerIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]eulerPositionRef, *indexLagNote, error) {
	start := indexStart{client: client, block: eulerV2ChainConfigs[block.ChainID].ActivationBlock}
	snapshot, err := i.indexedSnapshot(ctx, block, owner, start)
	if err != nil {
		return nil, nil, err
	}
	return snapshot.Positions, snapshot.Lag, nil
}
