package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	ownerTokenPageSize        = 500
	ownerTokenMaxRefs         = 4_096
	ownerTokenMaxRPCTailBlock = 2_048
	ownerTokenLiveMaxLag      = 15 * time.Minute
	ownerTokenBackfillMaxLag  = 7*24*time.Hour + 30*time.Minute
)

var (
	erc721TransferTopic        = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	erc1155TransferSingleTopic = crypto.Keccak256Hash([]byte("TransferSingle(address,address,address,uint256,uint256)"))
	erc1155TransferBatchTopic  = crypto.Keccak256Hash([]byte("TransferBatch(address,address,address,uint256[],uint256[])"))
	erc1155TransferABI         = MustABI(`[
      {"type":"event","name":"TransferBatch","anonymous":false,"inputs":[
        {"name":"operator","type":"address","indexed":true},
        {"name":"from","type":"address","indexed":true},
        {"name":"to","type":"address","indexed":true},
        {"name":"ids","type":"uint256[]","indexed":false},
        {"name":"values","type":"uint256[]","indexed":false}
      ]}
    ]`)
)

type ownerTokenStandard uint8

const (
	ownerTokenERC721 ownerTokenStandard = iota
	ownerTokenERC1155
)

type ownerTokenRef struct {
	Contract common.Address
	TokenID  *big.Int
}

type ownerTokenIndexer struct {
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newOwnerTokenIndexer(config SentioIndexerConfig, requiredChains []ChainID) *ownerTokenIndexer {
	return &ownerTokenIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: append([]ChainID(nil), requiredChains...),
	}
}

const ownerTokenWalletQuery = `
query PortfolioOwnerTokens(
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
  portfolioOwnerTokenRefs(
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
    tokenId
  }
}`

type ownerTokenGraphQLResponse struct {
	Data struct {
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Refs []struct {
			ID       string `json:"id"`
			ChainID  int    `json:"chainId"`
			Owner    string `json:"owner"`
			Contract string `json:"contract"`
			TokenID  string `json:"tokenId"`
		} `json:"portfolioOwnerTokenRefs"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type ownerTokenGraphQLPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	Refs            []ownerTokenRef
	RowIDs          []string
}

func ownerTokenRowID(chainID ChainID, contract common.Address, tokenID *big.Int) string {
	return fmt.Sprintf("%d:%s:%s", chainID, strings.ToLower(contract.Hex()), tokenID.String())
}

func (i *ownerTokenIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	owner common.Address,
	after string,
	block uint64,
	allowed map[common.Address]struct{},
) (ownerTokenGraphQLPage, error) {
	var payload ownerTokenGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": ownerTokenWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "owner": strings.ToLower(owner.Hex()),
				"contracts": sortedAllowedAddresses(allowed),
				"after":     after, "checkpoint": strconv.FormatUint(uint64(chainID), 10),
				"first": ownerTokenPageSize, "block": strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return ownerTokenGraphQLPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return ownerTokenGraphQLPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	if len(payload.Data.Checkpoints) != 1 {
		return ownerTokenGraphQLPage{}, fmt.Errorf(
			"GraphQL returned %d checkpoints",
			len(payload.Data.Checkpoints),
		)
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return ownerTokenGraphQLPage{}, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return ownerTokenGraphQLPage{}, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	page := ownerTokenGraphQLPage{CheckpointBlock: checkpointBlock, CheckpointMS: checkpointMS}
	for _, row := range payload.Data.Refs {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Owner, owner.Hex()) ||
			!common.IsHexAddress(row.Contract) || row.ID <= after {
			return ownerTokenGraphQLPage{}, fmt.Errorf("GraphQL returned malformed owner-token row %q", row.ID)
		}
		contract := common.HexToAddress(row.Contract)
		if _, exists := allowed[contract]; !exists {
			return ownerTokenGraphQLPage{}, fmt.Errorf("GraphQL returned unexpected token contract")
		}
		tokenID, valid := new(big.Int).SetString(row.TokenID, 10)
		if !valid || tokenID.Sign() < 0 || row.ID != ownerTokenRowID(chainID, contract, tokenID) {
			return ownerTokenGraphQLPage{}, fmt.Errorf("GraphQL returned malformed owner-token row %q", row.ID)
		}
		page.Refs = append(page.Refs, ownerTokenRef{Contract: contract, TokenID: tokenID})
		page.RowIDs = append(page.RowIDs, row.ID)
	}
	return page, nil
}

func validateOwnerTokenCheckpoint(block BlockRef, page ownerTokenGraphQLPage) error {
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
	maximumLag := ownerTokenLiveMaxLag
	if block.Fixed {
		maximumLag = ownerTokenBackfillMaxLag
	}
	if lag > maximumLag {
		return fmt.Errorf("indexer checkpoint is stale by %s", lag)
	}
	return nil
}

func ownerTokenRefKey(contract common.Address, tokenID *big.Int) string {
	return strings.ToLower(contract.Hex()) + ":" + tokenID.String()
}

func sortedAllowedAddresses(allowed map[common.Address]struct{}) []string {
	addresses := make([]string, 0, len(allowed))
	for address := range allowed {
		addresses = append(addresses, strings.ToLower(address.Hex()))
	}
	sort.Strings(addresses)
	return addresses
}

func addressFromIndexedTopic(topic common.Hash) common.Address {
	return common.BytesToAddress(topic.Bytes()[12:])
}

func (i *ownerTokenIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) ([]ownerTokenRef, error) {
	return i.positionRefs(ctx, client, block, owner, contracts, ownerTokenERC721)
}

func (i *ownerTokenIndexer) PositionRefsERC1155(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) ([]ownerTokenRef, error) {
	return i.positionRefs(ctx, client, block, owner, contracts, ownerTokenERC1155)
}

func (i *ownerTokenIndexer) positionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
	standard ownerTokenStandard,
) ([]ownerTokenRef, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	sentioQueryMu.Lock()
	queryLocked := true
	defer func() {
		if queryLocked {
			sentioQueryMu.Unlock()
		}
	}()
	statuses, err := i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, false)
	if err != nil {
		return nil, err
	}
	status, exists := statuses[block.ChainID]
	if !exists {
		return nil, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
	}
	if status.State != "PROCESSING_LATEST" {
		return nil, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID, status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	indexedBlock := min(block.Number, status.ProcessedBlock)
	if block.Number-indexedBlock > ownerTokenMaxRPCTailBlock {
		statuses, err = i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, true)
		if err != nil {
			return nil, err
		}
		status, exists = statuses[block.ChainID]
		if !exists {
			return nil, fmt.Errorf("indexer status omitted chain %d", block.ChainID)
		}
		indexedBlock = min(block.Number, status.ProcessedBlock)
		if status.State != "PROCESSING_LATEST" || block.Number-indexedBlock > ownerTokenMaxRPCTailBlock {
			return nil, fmt.Errorf(
				"indexer RPC tail exceeds %d blocks for chain %d",
				ownerTokenMaxRPCTailBlock,
				block.ChainID,
			)
		}
	}
	allowed := make(map[common.Address]struct{}, len(contracts))
	for _, contract := range contracts {
		allowed[contract] = struct{}{}
	}
	refsByKey := make(map[string]ownerTokenRef)
	after := ""
	for {
		page, pageErr := i.graphqlPage(ctx, block.ChainID, owner, after, indexedBlock, allowed)
		if pageErr != nil {
			return nil, pageErr
		}
		if err := validateOwnerTokenCheckpoint(block, page); err != nil {
			return nil, err
		}
		for _, ref := range page.Refs {
			refsByKey[ownerTokenRefKey(ref.Contract, ref.TokenID)] = ref
		}
		if len(refsByKey) > ownerTokenMaxRefs {
			return nil, fmt.Errorf("indexer returned more than %d owner-token rows", ownerTokenMaxRefs)
		}
		if len(page.RowIDs) < ownerTokenPageSize {
			break
		}
		next := page.RowIDs[len(page.RowIDs)-1]
		if next <= after {
			return nil, fmt.Errorf("indexer pagination did not advance")
		}
		after = next
	}
	sentioQueryMu.Unlock()
	queryLocked = false
	if indexedBlock < block.Number {
		topics := []common.Hash{erc721TransferTopic}
		if standard == ownerTokenERC1155 {
			topics = []common.Hash{erc1155TransferSingleTopic, erc1155TransferBatchTopic}
		}
		logs, logsErr := client.Logs(
			ctx,
			indexedBlock+1,
			block.Number,
			contracts,
			[][]common.Hash{topics},
		)
		if logsErr != nil {
			return nil, fmt.Errorf("read owner-token RPC tail: %w", logsErr)
		}
		sortRPCLogs(logs)
		for _, event := range logs {
			if _, exists := allowed[event.Address]; !exists {
				return nil, fmt.Errorf("owner-token RPC tail returned unexpected contract")
			}
			if standard == ownerTokenERC721 {
				if len(event.Topics) != 4 || event.Topics[0] != erc721TransferTopic {
					return nil, fmt.Errorf("owner-token RPC tail returned malformed ERC721 Transfer event")
				}
				from := addressFromIndexedTopic(event.Topics[1])
				to := addressFromIndexedTopic(event.Topics[2])
				tokenID := new(big.Int).SetBytes(event.Topics[3].Bytes())
				applyOwnerTokenTransfer(refsByKey, event.Address, tokenID, from, to, owner)
				continue
			}
			if err := applyERC1155TailLog(refsByKey, event, owner); err != nil {
				return nil, err
			}
		}
	}
	if len(refsByKey) > ownerTokenMaxRefs {
		return nil, fmt.Errorf("owner-token result contains more than %d rows", ownerTokenMaxRefs)
	}
	refs := make([]ownerTokenRef, 0, len(refsByKey))
	for _, ref := range refsByKey {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool {
		if refs[left].Contract != refs[right].Contract {
			return strings.ToLower(refs[left].Contract.Hex()) < strings.ToLower(refs[right].Contract.Hex())
		}
		return refs[left].TokenID.Cmp(refs[right].TokenID) < 0
	})
	return refs, nil
}

// applyERC1155TailLog folds one ERC-1155 transfer from the RPC tail into refs.
//
// The tokens read through this path are non-fungible, so a transfer that moves ownership moves
// exactly one unit. A zero-unit transfer is nonetheless legal and appears in real history —
// Ether.fi's membership NFT emits them — and it changes no owner, so it is skipped. Failing on
// one would drop every position on the account rather than ignore an event that says nothing.
// Above one unit the token is not the non-fungible this indexer assumes, and the scan fails.
func applyERC1155TailLog(
	refs map[string]ownerTokenRef,
	event rpcLog,
	owner common.Address,
) error {
	if len(event.Topics) != 4 {
		return fmt.Errorf("owner-token RPC tail returned malformed ERC1155 Transfer event")
	}
	from := addressFromIndexedTopic(event.Topics[2])
	to := addressFromIndexedTopic(event.Topics[3])
	switch event.Topics[0] {
	case erc1155TransferSingleTopic:
		if len(event.Data) != 64 {
			return fmt.Errorf("owner-token RPC tail returned malformed TransferSingle data")
		}
		tokenID := new(big.Int).SetBytes(event.Data[:32])
		value := new(big.Int).SetBytes(event.Data[32:])
		if value.Cmp(big.NewInt(1)) > 0 {
			return fmt.Errorf("owner-token RPC tail returned a non-unique TransferSingle value")
		}
		if value.Sign() == 0 {
			return nil
		}
		applyOwnerTokenTransfer(refs, event.Address, tokenID, from, to, owner)
	case erc1155TransferBatchTopic:
		values, decodeErr := erc1155TransferABI.Events["TransferBatch"].Inputs.NonIndexed().Unpack(event.Data)
		if decodeErr != nil || len(values) != 2 {
			return fmt.Errorf("owner-token RPC tail returned malformed TransferBatch data")
		}
		ids, idsOK := values[0].([]*big.Int)
		amounts, amountsOK := values[1].([]*big.Int)
		if !idsOK || !amountsOK || len(ids) != len(amounts) {
			return fmt.Errorf("owner-token RPC tail returned malformed TransferBatch arrays")
		}
		for index, tokenID := range ids {
			amount := amounts[index]
			if tokenID == nil || tokenID.Sign() < 0 || amount == nil || amount.Sign() < 0 ||
				amount.Cmp(big.NewInt(1)) > 0 {
				return fmt.Errorf("owner-token RPC tail returned invalid TransferBatch item")
			}
			if amount.Sign() == 0 {
				continue
			}
			applyOwnerTokenTransfer(refs, event.Address, tokenID, from, to, owner)
		}
	default:
		return fmt.Errorf("owner-token RPC tail returned unexpected ERC1155 event")
	}
	return nil
}

func applyOwnerTokenTransfer(
	refs map[string]ownerTokenRef,
	contract common.Address,
	tokenID *big.Int,
	from common.Address,
	to common.Address,
	owner common.Address,
) {
	key := ownerTokenRefKey(contract, tokenID)
	if from == owner && to != owner {
		delete(refs, key)
	}
	if to == owner {
		refs[key] = ownerTokenRef{Contract: contract, TokenID: new(big.Int).Set(tokenID)}
	}
}
