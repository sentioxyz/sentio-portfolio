package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	ownerTokenPageSize = 500
	ownerTokenMaxRefs  = 4_096
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

// PositionRefs returns the tokens of contracts the index records owner holding on block's chain,
// with a lag note when the index trails a live pin by more than indexMaxLag. A token the owner
// gave up after the indexed block is still among them, so callers check ownership at the pin.
func (i *ownerTokenIndexer) PositionRefs(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) ([]ownerTokenRef, *indexLagNote, error) {
	if len(contracts) == 0 {
		return nil, nil, nil
	}
	release, err := lockSentioLane(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	_, indexedBlock, err := indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, forceRefresh)
	})
	if err != nil {
		return nil, nil, err
	}
	allowed := make(map[common.Address]struct{}, len(contracts))
	for _, contract := range contracts {
		allowed[contract] = struct{}{}
	}
	refsByKey := make(map[string]ownerTokenRef)
	var lag *indexLagNote
	after := ""
	for {
		page, pageErr := i.graphqlPage(ctx, block.ChainID, owner, after, indexedBlock, allowed)
		if pageErr != nil {
			return nil, nil, pageErr
		}
		if lag, err = checkIndexCheckpoint(block, page.CheckpointBlock, page.CheckpointMS); err != nil {
			return nil, nil, err
		}
		for _, ref := range page.Refs {
			refsByKey[ownerTokenRefKey(ref.Contract, ref.TokenID)] = ref
		}
		if len(refsByKey) > ownerTokenMaxRefs {
			return nil, nil, fmt.Errorf("indexer returned more than %d owner-token rows", ownerTokenMaxRefs)
		}
		if len(page.RowIDs) < ownerTokenPageSize {
			break
		}
		next := page.RowIDs[len(page.RowIDs)-1]
		if next <= after {
			return nil, nil, fmt.Errorf("indexer pagination did not advance")
		}
		after = next
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
	return refs, lag, nil
}
