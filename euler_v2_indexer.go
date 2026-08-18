package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	eulerIndexerPageSize       = 500
	eulerMaxPositionRefs       = 4_096
	eulerMaxRewardRefs         = 16_384
	eulerMaxDynamicVaults      = 4_096
	eulerMaxRPCTailBlocks      = 2_048
	eulerManifestSnapshot      = 25_641_358
	eulerIndexerLiveMaxLag     = 15 * time.Minute
	eulerIndexerBackfillMaxLag = 7*24*time.Hour + 30*time.Minute
)

type eulerVaultKind string

const (
	eulerEVault     eulerVaultKind = "evault"
	eulerEarn       eulerVaultKind = "euler-earn"
	eulerSecuritize eulerVaultKind = "securitize"
)

type eulerVaultRef struct {
	Address      common.Address
	Kind         eulerVaultKind
	CreatedBlock uint64
}

type eulerPositionRef struct {
	Account      common.Address
	Vault        common.Address
	Kind         eulerVaultKind
	RewardTokens []common.Address
}

//go:embed euler_v2_vaults.json
var eulerVaultManifestJSON []byte

type rawEulerVaultManifest struct {
	SchemaVersion int `json:"schemaVersion"`
	ChainID       int `json:"chainId"`
	Snapshot      struct {
		BlockNumber string `json:"blockNumber"`
	} `json:"snapshot"`
	VaultCount int `json:"vaultCount"`
	Vaults     []struct {
		Address      string `json:"address"`
		Kind         string `json:"kind"`
		CreatedBlock string `json:"createdBlock"`
	} `json:"vaults"`
}

func validEulerVaultKind(kind eulerVaultKind) bool {
	return kind == eulerEVault || kind == eulerEarn || kind == eulerSecuritize
}

func mustEulerVaultManifest() []eulerVaultRef {
	var raw rawEulerVaultManifest
	if err := json.Unmarshal(eulerVaultManifestJSON, &raw); err != nil {
		panic(fmt.Errorf("parse Euler V2 vault manifest: %w", err))
	}
	if raw.SchemaVersion != 1 || raw.ChainID != int(Ethereum) || raw.VaultCount != len(raw.Vaults) ||
		raw.Snapshot.BlockNumber != strconv.FormatUint(eulerManifestSnapshot, 10) {
		panic("Euler V2 vault manifest header is invalid")
	}
	result := make([]eulerVaultRef, 0, len(raw.Vaults))
	seen := make(map[common.Address]struct{}, len(raw.Vaults))
	for _, item := range raw.Vaults {
		if !common.IsHexAddress(item.Address) {
			panic("Euler V2 vault manifest contains an invalid address")
		}
		address := common.HexToAddress(item.Address)
		if _, exists := seen[address]; exists {
			panic("Euler V2 vault manifest contains a duplicate address")
		}
		seen[address] = struct{}{}
		kind := eulerVaultKind(item.Kind)
		if !validEulerVaultKind(kind) {
			panic("Euler V2 vault manifest contains an invalid kind")
		}
		createdBlock, err := strconv.ParseUint(item.CreatedBlock, 10, 64)
		if err != nil || createdBlock == 0 || createdBlock > eulerManifestSnapshot {
			panic("Euler V2 vault manifest contains an invalid creation block")
		}
		result = append(result, eulerVaultRef{Address: address, Kind: kind, CreatedBlock: createdBlock})
	}
	return result
}

var eulerStaticVaults = mustEulerVaultManifest()

func eulerStaticVaultsForChain(chainID ChainID) []eulerVaultRef {
	if chainID == Ethereum {
		return eulerStaticVaults
	}
	return nil
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
  $positionAfter: ID!
  $rewardAfter: ID!
  $vaultAfter: ID!
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
    where: { chainId: $chainId, ownerPrefix: $ownerPrefix, id_gt: $positionAfter }
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
    where: { chainId: $chainId, ownerPrefix: $ownerPrefix, id_gt: $rewardAfter }
  ) {
    id
    chainId
    ownerPrefix
    account
    vault
    reward
  }
  eulerVaults(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, id_gt: $vaultAfter }
  ) {
    id
    chainId
    vault
    vaultKind
    createdBlock
  }
}`

type eulerGraphQLResponse struct {
	Data struct {
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Positions []struct {
			ID, OwnerPrefix, Account, Vault, VaultKind string
			ChainID                                    int `json:"chainId"`
		} `json:"eulerPositionRefs"`
		Rewards []struct {
			ID, OwnerPrefix, Account, Vault, Reward string
			ChainID                                 int `json:"chainId"`
		} `json:"eulerRewardTokens"`
		Vaults []struct {
			ID, Vault, VaultKind, CreatedBlock string
			ChainID                            int `json:"chainId"`
		} `json:"eulerVaults"`
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
	Vaults          []eulerVaultRef
	VaultIDs        []string
}

func eulerOwnerPrefix(account common.Address) string {
	return strings.ToLower(account.Hex())[:40]
}

func eulerPositionRowID(chainID ChainID, account, vault common.Address) string {
	return fmt.Sprintf(
		"%d:%s:%s:%s",
		chainID,
		eulerOwnerPrefix(account),
		strings.ToLower(account.Hex()),
		strings.ToLower(vault.Hex()),
	)
}

func eulerRewardRowID(chainID ChainID, account, vault, reward common.Address) string {
	return eulerPositionRowID(chainID, account, vault) + ":" + strings.ToLower(reward.Hex())
}

func eulerVaultRowID(chainID ChainID, vault common.Address) string {
	return strconv.FormatUint(uint64(chainID), 10) + ":" + strings.ToLower(vault.Hex())
}

func (i *eulerIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	owner common.Address,
	positionAfter, rewardAfter, vaultAfter string,
	block uint64,
) (eulerGraphQLPage, error) {
	var payload eulerGraphQLResponse
	err := i.api.doJSON(ctx, http.MethodPost, i.config.GraphQLURL, map[string]any{
		"query": eulerWalletQuery,
		"variables": map[string]any{
			"chainId":       int(chainID),
			"ownerPrefix":   eulerOwnerPrefix(owner),
			"positionAfter": positionAfter, "rewardAfter": rewardAfter, "vaultAfter": vaultAfter,
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
	if len(payload.Data.Checkpoints) != 1 {
		return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned %d checkpoints", len(payload.Data.Checkpoints))
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return eulerGraphQLPage{}, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return eulerGraphQLPage{}, fmt.Errorf("invalid checkpoint timestamp: %w", err)
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
	for _, row := range payload.Data.Vaults {
		if row.ChainID != int(chainID) || !common.IsHexAddress(row.Vault) || row.ID <= vaultAfter {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned malformed Euler vault row %q", row.ID)
		}
		vault := common.HexToAddress(row.Vault)
		kind := eulerVaultKind(row.VaultKind)
		created, err := strconv.ParseUint(row.CreatedBlock, 10, 64)
		minimumCreatedBlock := eulerV2ChainConfigs[chainID].ActivationBlock
		if chainID == Ethereum {
			minimumCreatedBlock = eulerManifestSnapshot + 1
		}
		if err != nil || created < minimumCreatedBlock || !validEulerVaultKind(kind) ||
			row.ID != eulerVaultRowID(chainID, vault) {
			return eulerGraphQLPage{}, fmt.Errorf("GraphQL returned malformed Euler vault row %q", row.ID)
		}
		page.Vaults = append(page.Vaults, eulerVaultRef{Address: vault, Kind: kind, CreatedBlock: created})
		page.VaultIDs = append(page.VaultIDs, row.ID)
	}
	return page, nil
}

func validateEulerCheckpoint(block BlockRef, page eulerGraphQLPage) error {
	if page.CheckpointBlock > block.Number {
		return fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", page.CheckpointBlock, block.Number)
	}
	checkpointSeconds := page.CheckpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp <= checkpointSeconds {
		return nil
	}
	lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
	maximumLag := eulerIndexerLiveMaxLag
	if block.Fixed {
		maximumLag = eulerIndexerBackfillMaxLag
	}
	if lag > maximumLag {
		return fmt.Errorf("indexer checkpoint is stale by %s", lag)
	}
	return nil
}

type eulerIndexedSnapshot struct {
	Block     uint64
	Positions []eulerPositionRef
	Vaults    []eulerVaultRef
}

func (i *eulerIndexer) indexedSnapshot(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
) (eulerIndexedSnapshot, error) {
	sentioQueryMu.Lock()
	defer sentioQueryMu.Unlock()
	statuses, err := i.api.chainStatuses(ctx, i.config, []ChainID{block.ChainID}, false)
	if err != nil {
		return eulerIndexedSnapshot{}, err
	}
	status, exists := statuses[block.ChainID]
	if !exists {
		return eulerIndexedSnapshot{}, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
	}
	if status.State != "PROCESSING_LATEST" {
		return eulerIndexedSnapshot{}, fmt.Errorf(
			"indexer backfill is incomplete: state=%s processed=%d estimatedLatest=%d",
			status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	indexedBlock := min(block.Number, status.ProcessedBlock)
	if block.Number-indexedBlock > eulerMaxRPCTailBlocks {
		statuses, err = i.api.chainStatuses(ctx, i.config, []ChainID{block.ChainID}, true)
		if err != nil {
			return eulerIndexedSnapshot{}, err
		}
		status, exists = statuses[block.ChainID]
		if !exists {
			return eulerIndexedSnapshot{}, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
		}
		indexedBlock = min(block.Number, status.ProcessedBlock)
		if status.State != "PROCESSING_LATEST" || block.Number-indexedBlock > eulerMaxRPCTailBlocks {
			return eulerIndexedSnapshot{}, fmt.Errorf("indexer RPC tail exceeds %d blocks", eulerMaxRPCTailBlocks)
		}
	}
	positions := make(map[string]eulerPositionRef)
	rewards := make(map[string]map[common.Address]struct{})
	vaults := make(map[common.Address]eulerVaultRef)
	positionAfter, rewardAfter, vaultAfter := "", "", ""
	positionDone, rewardDone, vaultDone := false, false, false
	for !positionDone || !rewardDone || !vaultDone {
		page, pageErr := i.graphqlPage(
			ctx, block.ChainID, owner, positionAfter, rewardAfter, vaultAfter, indexedBlock,
		)
		if pageErr != nil {
			return eulerIndexedSnapshot{}, pageErr
		}
		if err := validateEulerCheckpoint(block, page); err != nil {
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
		for _, vault := range page.Vaults {
			vaults[vault.Address] = vault
		}
		if len(positions) > eulerMaxPositionRefs || len(vaults) > eulerMaxDynamicVaults {
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
		vaultDone = len(page.VaultIDs) < eulerIndexerPageSize
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
		if !vaultDone {
			next := page.VaultIDs[len(page.VaultIDs)-1]
			if next <= vaultAfter {
				return eulerIndexedSnapshot{}, fmt.Errorf("Euler vault pagination did not advance")
			}
			vaultAfter = next
		}
	}
	result := eulerIndexedSnapshot{Block: indexedBlock}
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
	for _, vault := range vaults {
		result.Vaults = append(result.Vaults, vault)
	}
	return result, nil
}

var (
	eulerTransferTopic          = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	eulerBorrowTopic            = crypto.Keccak256Hash([]byte("Borrow(address,uint256)"))
	eulerRepayTopic             = crypto.Keccak256Hash([]byte("Repay(address,uint256)"))
	eulerProxyCreatedTopic      = crypto.Keccak256Hash([]byte("ProxyCreated(address,bool,address,bytes)"))
	eulerEarnCreatedTopic       = crypto.Keccak256Hash([]byte("CreateEulerEarn(address,address,address,uint256,address,string,string,bytes32)"))
	eulerSecuritizeCreatedTopic = crypto.Keccak256Hash([]byte("ContractDeployed(address,address,uint256)"))
)

func eulerSubaccountTopics(owner common.Address) []common.Hash {
	result := make([]common.Hash, 256)
	base := owner.Bytes()
	for suffix := 0; suffix < 256; suffix++ {
		account := append([]byte(nil), base...)
		account[19] = byte(suffix)
		result[suffix] = common.BytesToHash(account)
	}
	return result
}

func eulerAddressFromTopic(topic common.Hash) common.Address {
	return common.BytesToAddress(topic.Bytes()[12:])
}

func (i *eulerIndexer) applyRPCTail(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	snapshot eulerIndexedSnapshot,
) ([]eulerPositionRef, error) {
	positions := make(map[string]eulerPositionRef, len(snapshot.Positions))
	for _, ref := range snapshot.Positions {
		positions[eulerPositionRowID(block.ChainID, ref.Account, ref.Vault)] = ref
	}
	if snapshot.Block >= block.Number {
		return snapshot.Positions, nil
	}
	staticVaults := eulerStaticVaultsForChain(block.ChainID)
	vaults := make(map[common.Address]eulerVaultRef, len(staticVaults)+len(snapshot.Vaults))
	for _, vault := range append(append([]eulerVaultRef(nil), staticVaults...), snapshot.Vaults...) {
		if vault.CreatedBlock <= block.Number {
			vaults[vault.Address] = vault
		}
	}
	chain, supported := eulerV2ChainConfigs[block.ChainID]
	if !supported {
		return nil, fmt.Errorf("Euler V2 is not configured on chain %d", block.ChainID)
	}
	factoryConfigs := []struct {
		address common.Address
		topic   common.Hash
		kind    eulerVaultKind
	}{
		{chain.EVaultFactory, eulerProxyCreatedTopic, eulerEVault},
		{chain.EulerEarnFactory, eulerEarnCreatedTopic, eulerEarn},
		{chain.SecuritizeFactory, eulerSecuritizeCreatedTopic, eulerSecuritize},
	}
	for _, factory := range factoryConfigs {
		if factory.address == (common.Address{}) {
			continue
		}
		logs, err := client.Logs(ctx, snapshot.Block+1, block.Number, []common.Address{factory.address}, [][]common.Hash{{factory.topic}})
		if err != nil {
			return nil, fmt.Errorf("Euler factory RPC tail: %w", err)
		}
		for _, event := range logs {
			if len(event.Topics) < 2 {
				return nil, fmt.Errorf("Euler factory RPC tail returned malformed event")
			}
			vault := eulerAddressFromTopic(event.Topics[1])
			vaults[vault] = eulerVaultRef{Address: vault, Kind: factory.kind, CreatedBlock: uint64(event.BlockNumber)}
		}
	}
	addresses := make([]common.Address, 0, len(vaults))
	for address := range vaults {
		addresses = append(addresses, address)
	}
	for _, ref := range positions {
		if _, exists := vaults[ref.Vault]; !exists {
			return nil, fmt.Errorf("Euler position index references a vault absent from the factory index")
		}
	}
	if len(addresses) == 0 {
		return nil, nil
	}
	accounts := eulerSubaccountTopics(owner)
	type logQuery struct {
		topics [][]common.Hash
		kind   string
	}
	queries := []logQuery{
		{topics: [][]common.Hash{{eulerTransferTopic}, accounts}, kind: "transfer-from"},
		{topics: [][]common.Hash{{eulerTransferTopic}, nil, accounts}, kind: "transfer-to"},
		{topics: [][]common.Hash{{eulerBorrowTopic}, accounts}, kind: "borrow"},
		{topics: [][]common.Hash{{eulerRepayTopic}, accounts}, kind: "repay"},
	}
	for _, query := range queries {
		logs, err := client.Logs(ctx, snapshot.Block+1, block.Number, addresses, query.topics)
		if err != nil {
			return nil, fmt.Errorf("Euler position RPC tail: %w", err)
		}
		for _, event := range logs {
			vault, exists := vaults[event.Address]
			if !exists {
				return nil, fmt.Errorf("Euler RPC tail returned an unknown vault")
			}
			var account common.Address
			switch query.kind {
			case "transfer-from":
				if len(event.Topics) != 3 {
					return nil, fmt.Errorf("Euler RPC tail returned malformed Transfer")
				}
				account = eulerAddressFromTopic(event.Topics[1])
			case "transfer-to":
				if len(event.Topics) != 3 {
					return nil, fmt.Errorf("Euler RPC tail returned malformed Transfer")
				}
				account = eulerAddressFromTopic(event.Topics[2])
			default:
				if len(event.Topics) != 2 {
					return nil, fmt.Errorf("Euler RPC tail returned malformed debt event")
				}
				account = eulerAddressFromTopic(event.Topics[1])
			}
			if eulerOwnerPrefix(account) != eulerOwnerPrefix(owner) {
				return nil, fmt.Errorf("Euler RPC tail returned a foreign subaccount")
			}
			key := eulerPositionRowID(block.ChainID, account, event.Address)
			if existing, exists := positions[key]; exists {
				vault.Kind = existing.Kind
				vault.Address = existing.Vault
				positions[key] = existing
			} else {
				positions[key] = eulerPositionRef{Account: account, Vault: event.Address, Kind: vault.Kind}
			}
		}
	}
	result := make([]eulerPositionRef, 0, len(positions))
	for _, ref := range positions {
		result = append(result, ref)
	}
	if len(result) > eulerMaxPositionRefs {
		return nil, fmt.Errorf("Euler position refs exceed %d", eulerMaxPositionRefs)
	}
	sort.Slice(result, func(left, right int) bool {
		leftID := eulerPositionRowID(block.ChainID, result[left].Account, result[left].Vault)
		rightID := eulerPositionRowID(block.ChainID, result[right].Account, result[right].Vault)
		return leftID < rightID
	})
	return result, nil
}

func (i *eulerIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]eulerPositionRef, error) {
	snapshot, err := i.indexedSnapshot(ctx, block, owner)
	if err != nil {
		return nil, err
	}
	return i.applyRPCTail(ctx, client, block, owner, snapshot)
}
