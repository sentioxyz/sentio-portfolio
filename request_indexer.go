package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	accountRequestPageSize       = 500
	accountRequestMaxRefs        = 4_096
	accountRequestMaxRPCTail     = 2_048
	accountRequestLiveMaxLag     = 15 * time.Minute
	accountRequestBackfillMaxLag = 7*24*time.Hour + 30*time.Minute
)

type accountRequestRef struct {
	Contract common.Address
	Key      string
}

type accountRequestSnapshot struct {
	Block uint64
	Refs  []accountRequestRef
}

type accountRequestIndexer struct {
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newAccountRequestIndexer(
	config SentioIndexerConfig,
	requiredChains []ChainID,
) *accountRequestIndexer {
	return &accountRequestIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: append([]ChainID(nil), requiredChains...),
	}
}

const accountRequestWalletQuery = `
query PortfolioAccountRequests(
  $chainId: Int!
  $owner: String!
  $contracts: [String!]!
  $after: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  portfolioAccountRequestRefs(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, owner: $owner, contract_in: $contracts, id_gt: $after }
  ) {
    id
    chainId
    owner
    contract
    requestKey
  }
}`

type accountRequestGraphQLResponse struct {
	Data struct {
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Refs []struct {
			ID         string `json:"id"`
			ChainID    int    `json:"chainId"`
			Owner      string `json:"owner"`
			Contract   string `json:"contract"`
			RequestKey string `json:"requestKey"`
		} `json:"portfolioAccountRequestRefs"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func accountRequestRowID(chainID ChainID, contract common.Address, key string) string {
	return fmt.Sprintf("%d:%s:%s", chainID, strings.ToLower(contract.Hex()), key)
}

func validAccountRequestKey(key string) bool {
	if key == "" || len(key) > 66 || strings.TrimSpace(key) != key || strings.ToLower(key) != key {
		return false
	}
	if strings.HasPrefix(key, "0x") {
		if len(key) != 66 {
			return false
		}
		for _, character := range key[2:] {
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return false
			}
		}
		return true
	}
	if len(key) > 1 && key[0] == '0' {
		return false
	}
	for _, character := range key {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (i *accountRequestIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	owner common.Address,
	after string,
	block uint64,
	allowed map[common.Address]struct{},
) (accountRequestSnapshot, []string, uint64, error) {
	var payload accountRequestGraphQLResponse
	err := i.api.doJSON(ctx, http.MethodPost, i.config.GraphQLURL, map[string]any{
		"query": accountRequestWalletQuery,
		"variables": map[string]any{
			"chainId": int(chainID), "owner": strings.ToLower(owner.Hex()),
			"contracts": sortedAllowedAddresses(allowed),
			"after":     after, "checkpoint": strconv.FormatUint(uint64(chainID), 10),
			"first": accountRequestPageSize, "block": strconv.FormatUint(block, 10),
		},
	}, &payload)
	if err != nil {
		return accountRequestSnapshot{}, nil, 0, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return accountRequestSnapshot{}, nil, 0, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	if len(payload.Data.Checkpoints) != 1 {
		return accountRequestSnapshot{}, nil, 0, fmt.Errorf(
			"GraphQL returned %d checkpoints", len(payload.Data.Checkpoints),
		)
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return accountRequestSnapshot{}, nil, 0, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return accountRequestSnapshot{}, nil, 0, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	snapshot := accountRequestSnapshot{Block: checkpointBlock}
	rowIDs := make([]string, 0, len(payload.Data.Refs))
	for _, row := range payload.Data.Refs {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Owner, owner.Hex()) ||
			!common.IsHexAddress(row.Contract) || row.ID <= after ||
			!validAccountRequestKey(row.RequestKey) {
			return accountRequestSnapshot{}, nil, 0, fmt.Errorf(
				"GraphQL returned malformed account-request row %q", row.ID,
			)
		}
		contract := common.HexToAddress(row.Contract)
		if _, exists := allowed[contract]; !exists {
			return accountRequestSnapshot{}, nil, 0, fmt.Errorf("GraphQL returned unexpected request contract")
		}
		if row.ID != accountRequestRowID(chainID, contract, row.RequestKey) {
			return accountRequestSnapshot{}, nil, 0, fmt.Errorf(
				"GraphQL returned malformed account-request row %q", row.ID,
			)
		}
		snapshot.Refs = append(snapshot.Refs, accountRequestRef{Contract: contract, Key: row.RequestKey})
		rowIDs = append(rowIDs, row.ID)
	}
	return snapshot, rowIDs, checkpointMS, nil
}

func validateAccountRequestCheckpoint(block BlockRef, indexedBlock, checkpointMS uint64) error {
	if indexedBlock > block.Number {
		return fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", indexedBlock, block.Number)
	}
	checkpointSeconds := checkpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp <= checkpointSeconds {
		return nil
	}
	lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
	maximumLag := accountRequestLiveMaxLag
	if block.Fixed {
		maximumLag = accountRequestBackfillMaxLag
	}
	if lag > maximumLag {
		return fmt.Errorf("indexer checkpoint is stale by %s", lag)
	}
	return nil
}

func accountRequestRefKey(ref accountRequestRef) string {
	return strings.ToLower(ref.Contract.Hex()) + ":" + ref.Key
}

func (i *accountRequestIndexer) IndexedRefs(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) (accountRequestSnapshot, error) {
	if len(contracts) == 0 {
		return accountRequestSnapshot{Block: block.Number}, nil
	}
	allowed := make(map[common.Address]struct{}, len(contracts))
	for _, contract := range contracts {
		allowed[contract] = struct{}{}
	}
	sentioQueryMu.Lock()
	defer sentioQueryMu.Unlock()
	statuses, err := i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, false)
	if err != nil {
		return accountRequestSnapshot{}, err
	}
	status, exists := statuses[block.ChainID]
	if !exists {
		return accountRequestSnapshot{}, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
	}
	if status.State != "PROCESSING_LATEST" {
		return accountRequestSnapshot{}, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID, status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	indexedBlock := min(block.Number, status.ProcessedBlock)
	if block.Number-indexedBlock > accountRequestMaxRPCTail {
		statuses, err = i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, true)
		if err != nil {
			return accountRequestSnapshot{}, err
		}
		status, exists = statuses[block.ChainID]
		if !exists {
			return accountRequestSnapshot{}, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
		}
		indexedBlock = min(block.Number, status.ProcessedBlock)
		if status.State != "PROCESSING_LATEST" || block.Number-indexedBlock > accountRequestMaxRPCTail {
			return accountRequestSnapshot{}, fmt.Errorf(
				"indexer RPC tail exceeds %d blocks for chain %d", accountRequestMaxRPCTail, block.ChainID,
			)
		}
	}
	refs := make(map[string]accountRequestRef)
	after := ""
	for {
		page, rowIDs, checkpointMS, pageErr := i.graphqlPage(
			ctx, block.ChainID, owner, after, indexedBlock, allowed,
		)
		if pageErr != nil {
			return accountRequestSnapshot{}, pageErr
		}
		if page.Block > indexedBlock {
			return accountRequestSnapshot{}, fmt.Errorf(
				"indexer checkpoint %d is ahead of query block %d", page.Block, indexedBlock,
			)
		}
		if err := validateAccountRequestCheckpoint(block, page.Block, checkpointMS); err != nil {
			return accountRequestSnapshot{}, err
		}
		for _, ref := range page.Refs {
			refs[accountRequestRefKey(ref)] = ref
		}
		if len(refs) > accountRequestMaxRefs {
			return accountRequestSnapshot{}, fmt.Errorf(
				"indexer returned more than %d account-request rows", accountRequestMaxRefs,
			)
		}
		if len(rowIDs) < accountRequestPageSize {
			break
		}
		next := rowIDs[len(rowIDs)-1]
		if next <= after {
			return accountRequestSnapshot{}, fmt.Errorf("indexer pagination did not advance")
		}
		after = next
	}
	result := accountRequestSnapshot{Block: indexedBlock, Refs: make([]accountRequestRef, 0, len(refs))}
	for _, ref := range refs {
		result.Refs = append(result.Refs, ref)
	}
	sort.Slice(result.Refs, func(left, right int) bool {
		return accountRequestRefKey(result.Refs[left]) < accountRequestRefKey(result.Refs[right])
	})
	return result, nil
}
