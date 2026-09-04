package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	uniswapIndexerPageSize  = 500
	uniswapCheckpointMaxLag = 15 * time.Minute
	uniswapBackfillMaxLag   = 7*24*time.Hour + 30*time.Minute
)

type uniswapGeneration string

const (
	uniswapV3 uniswapGeneration = "v3"
	uniswapV4 uniswapGeneration = "v4"
)

type uniswapIndexerDefinition struct {
	version        uniswapGeneration
	maxIndexedNFTs int
}

var uniswapIndexerDefinitions = map[uniswapGeneration]uniswapIndexerDefinition{
	uniswapV3: {
		version:        uniswapV3,
		maxIndexedNFTs: 4_096,
	},
	uniswapV4: {
		version:        uniswapV4,
		maxIndexedNFTs: 512,
	},
}

type uniswapIndexedNFT struct {
	TokenID *big.Int
	Manager common.Address
}

type uniswapIndexedNFTs struct {
	CheckpointBlock uint64
	NFTs            []uniswapIndexedNFT
}

type uniswapIndexer struct {
	api     *sentioAPIClient
	configs map[uniswapGeneration]SentioIndexerConfig
}

func newUniswapIndexer(
	v3Config SentioIndexerConfig,
	v4Config SentioIndexerConfig,
) *uniswapIndexer {
	return &uniswapIndexer{
		api: newSentioAPIClient(),
		configs: map[uniswapGeneration]SentioIndexerConfig{
			uniswapV3: v3Config,
			uniswapV4: v4Config,
		},
	}
}

func (i *uniswapIndexer) chainStatuses(
	ctx context.Context,
	definition uniswapIndexerDefinition,
) (map[ChainID]sentioChainStatus, error) {
	var chains []ChainID
	switch definition.version {
	case uniswapV3:
		chains = deploymentChains(uniswapV3Deployments)
	case uniswapV4:
		chains = deploymentChains(uniswapV4Deployments)
	default:
		return nil, fmt.Errorf("unsupported Uniswap generation %q", definition.version)
	}
	return i.api.chainStatuses(ctx, i.configs[definition.version], chains, false)
}

const uniswapWalletQuery = `
query WalletPositions(
  $prefix: String!
  $after: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  positions(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { id_starts_with: $prefix, id_gt: $after, balance_not: 0 }
  ) {
    id
    chainId
    owner
    tokenId
    manager
    balance
  }
}`

type uniswapGraphQLResponse struct {
	Data struct {
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Positions []struct {
			ID      string `json:"id"`
			ChainID int    `json:"chainId"`
			Owner   string `json:"owner"`
			TokenID string `json:"tokenId"`
			Manager string `json:"manager"`
			Balance string `json:"balance"`
		} `json:"positions"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type uniswapGraphQLPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	Positions       []uniswapIndexedNFT
	IDs             []string
}

func uniswapPositionRowID(
	chainID ChainID,
	owner common.Address,
	manager common.Address,
	tokenID *big.Int,
) string {
	return fmt.Sprintf(
		"%d:%s:%s:%s",
		chainID,
		strings.ToLower(owner.Hex()),
		strings.ToLower(manager.Hex()),
		tokenID.String(),
	)
}

func uniswapExpectedManager(
	version uniswapGeneration,
	chainID ChainID,
) (common.Address, bool) {
	switch version {
	case uniswapV3:
		deployment, exists := uniswapV3Deployments[chainID]
		return deployment.Manager, exists
	case uniswapV4:
		deployment, exists := uniswapV4Deployments[chainID]
		return deployment.Manager, exists
	default:
		return common.Address{}, false
	}
}

func (i *uniswapIndexer) graphqlPage(
	ctx context.Context,
	definition uniswapIndexerDefinition,
	chainID ChainID,
	owner common.Address,
	prefix string,
	after string,
	first int,
	block uint64,
) (uniswapGraphQLPage, error) {
	var payload uniswapGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.configs[definition.version].GraphQLURL,
		map[string]any{
			"query": uniswapWalletQuery,
			"variables": map[string]any{
				"prefix":     prefix,
				"after":      after,
				"checkpoint": strconv.FormatUint(uint64(chainID), 10),
				"first":      first,
				"block":      strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return uniswapGraphQLPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return uniswapGraphQLPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	if len(payload.Data.Checkpoints) != 1 {
		return uniswapGraphQLPage{}, fmt.Errorf(
			"GraphQL returned %d checkpoints",
			len(payload.Data.Checkpoints),
		)
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return uniswapGraphQLPage{}, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return uniswapGraphQLPage{}, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	page := uniswapGraphQLPage{
		CheckpointBlock: checkpointBlock,
		CheckpointMS:    checkpointMS,
		Positions:       make([]uniswapIndexedNFT, 0, len(payload.Data.Positions)),
		IDs:             make([]string, 0, len(payload.Data.Positions)),
	}
	expectedOwner := strings.ToLower(owner.Hex())
	expectedManager, exists := uniswapExpectedManager(definition.version, chainID)
	if !exists {
		return uniswapGraphQLPage{}, fmt.Errorf(
			"Uniswap %s has no deployment on chain %d",
			definition.version,
			chainID,
		)
	}
	for _, row := range payload.Data.Positions {
		if row.ChainID != int(chainID) || row.Owner != expectedOwner || row.ID <= after {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		tokenID, ok := new(big.Int).SetString(row.TokenID, 10)
		if !ok || tokenID.Sign() < 0 || tokenID.BitLen() > 256 || row.TokenID != tokenID.String() {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		if !common.IsHexAddress(row.Manager) {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		manager := common.HexToAddress(row.Manager)
		if row.Manager != strings.ToLower(manager.Hex()) || manager != expectedManager {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		balance, ok := new(big.Int).SetString(row.Balance, 10)
		if !ok || row.Balance != balance.String() || balance.Cmp(big.NewInt(1)) != 0 {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		if row.ID != uniswapPositionRowID(chainID, owner, manager, tokenID) ||
			!strings.HasPrefix(row.ID, prefix) {
			return uniswapGraphQLPage{}, fmt.Errorf("GraphQL returned malformed position row %q", row.ID)
		}
		page.IDs = append(page.IDs, row.ID)
		page.Positions = append(page.Positions, uniswapIndexedNFT{
			TokenID: tokenID,
			Manager: manager,
		})
	}
	return page, nil
}

func validateUniswapCheckpoint(block BlockRef, page uniswapGraphQLPage) error {
	if page.CheckpointBlock > block.Number {
		return fmt.Errorf(
			"indexer checkpoint %d is ahead of pinned block %d",
			page.CheckpointBlock,
			block.Number,
		)
	}
	checkpointSeconds := page.CheckpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp > checkpointSeconds {
		lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
		maximumLag := uniswapCheckpointMaxLag
		if block.Fixed {
			maximumLag = uniswapBackfillMaxLag
		}
		if lag > maximumLag {
			return fmt.Errorf("indexer checkpoint is stale by %s", lag)
		}
	}
	return nil
}

func (i *uniswapIndexer) indexedNFTs(
	ctx context.Context,
	version uniswapGeneration,
	block BlockRef,
	account common.Address,
) (uniswapIndexedNFTs, error) {
	definition, exists := uniswapIndexerDefinitions[version]
	if !exists {
		return uniswapIndexedNFTs{}, fmt.Errorf("unknown Uniswap generation %q", version)
	}

	sentioQueryMu.Lock()
	defer sentioQueryMu.Unlock()
	statuses, err := i.chainStatuses(ctx, definition)
	if err != nil {
		return uniswapIndexedNFTs{}, err
	}
	status, exists := statuses[block.ChainID]
	if !exists {
		return uniswapIndexedNFTs{}, fmt.Errorf("processor status omitted chain %d", block.ChainID)
	}
	if status.State != "PROCESSING_LATEST" {
		return uniswapIndexedNFTs{}, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID,
			status.State,
			status.ProcessedBlock,
			status.EstimatedLatest,
		)
	}

	prefix := fmt.Sprintf("%d:%s:", block.ChainID, strings.ToLower(account.Hex()))
	after := prefix
	queryBlock := block.Number
	result := make([]uniswapIndexedNFT, 0)
	var checkpointBlock uint64
	var checkpointMS uint64
	firstRequest := true
	for len(result) <= definition.maxIndexedNFTs {
		first := min(uniswapIndexerPageSize, definition.maxIndexedNFTs+1-len(result))
		requestedBlock := queryBlock
		page, err := i.graphqlPage(
			ctx,
			definition,
			block.ChainID,
			account,
			prefix,
			after,
			first,
			queryBlock,
		)
		if err != nil {
			return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s index query: %w", version, err)
		}
		if err := validateUniswapCheckpoint(block, page); err != nil {
			return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s: %w", version, err)
		}
		if checkpointBlock == 0 {
			checkpointBlock = page.CheckpointBlock
			checkpointMS = page.CheckpointMS
			queryBlock = checkpointBlock
		} else if page.CheckpointBlock != checkpointBlock || page.CheckpointMS != checkpointMS {
			return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s pagination changed checkpoint", version)
		}
		for index, id := range page.IDs {
			if id <= after {
				return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s rows are not ordered", version)
			}
			after = id
			result = append(result, page.Positions[index])
		}
		if len(page.Positions) > first {
			return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s exceeded GraphQL page size", version)
		}
		if len(result) > definition.maxIndexedNFTs {
			break
		}
		if firstRequest && len(page.Positions) == first && requestedBlock != checkpointBlock {
			// Restart at the timer checkpoint so a multi-page response never mixes snapshots.
			result = result[:0]
			after = prefix
			firstRequest = false
			continue
		}
		firstRequest = false
		if len(page.Positions) < first {
			return uniswapIndexedNFTs{CheckpointBlock: checkpointBlock, NFTs: result}, nil
		}
	}
	return uniswapIndexedNFTs{}, fmt.Errorf(
		"Uniswap %s account has more than %d indexed positions",
		version,
		definition.maxIndexedNFTs,
	)
}
