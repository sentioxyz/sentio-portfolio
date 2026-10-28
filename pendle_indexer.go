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
	pendleIndexerPageSize = 500
	// pendleMarketLookupBatch bounds the PTs named in one market lookup. An account holding
	// hundreds of PTs would otherwise build a single query longer than the endpoint accepts.
	pendleMarketLookupBatch = 100
	pendleMaxPositionRefs   = 4_096
)

type pendleIndexer struct {
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newPendleIndexer(config SentioIndexerConfig) *pendleIndexer {
	return &pendleIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: deploymentChains(pendleChainConfigs),
	}
}

const pendleWalletQuery = `
query PendleWalletPositions(
  $chainId: Int!
  $account: String!
  $idPrefix: String!
  $after: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  pendlePositionRefs(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, account: $account, id_starts_with: $idPrefix, id_gt: $after }
  ) {
    id
    chainId
    account
    token
    kind
  }
}`

const pendleTokensQuery = `
query PendleTokens($ids: [ID!]!, $first: Int!, $block: BigInt!) {
  pendleTokens(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { id_in: $ids }
  ) {
    id
    chainId
    address
    kind
    pt
    sy
    expiry
    createdBlock
  }
}`

// pendleMarketsQuery finds the markets created for a set of PTs. A directly held PT points at no
// market on-chain and Pendle allows several per PT, so the index — which records the PT each
// market was created for — is the only way to reach the market that publishes its implied rate.
const pendleMarketsQuery = `
query PendleMarketsForPT(
  $pts: [String!]!
  $chainId: Int!
  $after: ID!
  $first: Int!
  $block: BigInt!
) {
  pendleTokens(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, kind: "lp", pt_in: $pts, id_gt: $after }
  ) {
    id
    chainId
    address
    kind
    pt
  }
}`

type pendleMarketsResponse struct {
	Data struct {
		Markets []struct {
			ID      string `json:"id"`
			ChainID int    `json:"chainId"`
			Address string `json:"address"`
			Kind    string `json:"kind"`
			PT      string `json:"pt"`
		} `json:"pendleTokens"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type pendleWalletResponse struct {
	Data struct {
		Checkpoints []indexerCheckpointRow `json:"indexerCheckpoints"`
		Refs        []struct {
			ID      string `json:"id"`
			ChainID int    `json:"chainId"`
			Account string `json:"account"`
			Token   string `json:"token"`
			Kind    string `json:"kind"`
		} `json:"pendlePositionRefs"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type pendleTokensResponse struct {
	Data struct {
		Tokens []struct {
			ID           string `json:"id"`
			ChainID      int    `json:"chainId"`
			Address      string `json:"address"`
			Kind         string `json:"kind"`
			PT           string `json:"pt"`
			SY           string `json:"sy"`
			Expiry       string `json:"expiry"`
			CreatedBlock string `json:"createdBlock"`
		} `json:"pendleTokens"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type pendleWalletPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	Tokens          []common.Address
	Kinds           []pendleTokenKind
	RowIDs          []string
}

func pendleRefRowPrefix(chainID ChainID, account common.Address) string {
	return fmt.Sprintf("%d:%s:", chainID, strings.ToLower(account.Hex()))
}

func pendleTokenRowID(chainID ChainID, token common.Address) string {
	return fmt.Sprintf("%d:%s", chainID, strings.ToLower(token.Hex()))
}

func pendleGraphQLError(messages []string) error {
	return fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
}

func (i *pendleIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	account common.Address,
	after string,
	block uint64,
	start indexStart,
) (pendleWalletPage, error) {
	prefix := pendleRefRowPrefix(chainID, account)
	var payload pendleWalletResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": pendleWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "account": strings.ToLower(account.Hex()), "idPrefix": prefix,
				"after":      after,
				"checkpoint": strconv.FormatUint(uint64(chainID), 10),
				"first":      pendleIndexerPageSize, "block": strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return pendleWalletPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return pendleWalletPage{}, pendleGraphQLError(messages)
	}
	checkpointBlock, checkpointMS, err := start.checkpoint(ctx, payload.Data.Checkpoints)
	if err != nil {
		return pendleWalletPage{}, err
	}
	page := pendleWalletPage{CheckpointBlock: checkpointBlock, CheckpointMS: checkpointMS}
	for _, row := range payload.Data.Refs {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
			!strings.HasPrefix(row.ID, prefix) || row.ID <= after || !common.IsHexAddress(row.Token) ||
			!validPendleTokenKind(pendleTokenKind(row.Kind)) {
			return pendleWalletPage{}, fmt.Errorf("GraphQL returned malformed Pendle position row %q", row.ID)
		}
		token := common.HexToAddress(row.Token)
		if row.ID != prefix+strings.ToLower(token.Hex()) {
			return pendleWalletPage{}, fmt.Errorf("GraphQL returned foreign Pendle position row %q", row.ID)
		}
		page.Tokens = append(page.Tokens, token)
		page.Kinds = append(page.Kinds, pendleTokenKind(row.Kind))
		page.RowIDs = append(page.RowIDs, row.ID)
	}
	return page, nil
}

// graphqlTokens resolves the factory registration of the tokens an account holds. It doubles as
// a consistency check: a position row whose token was never registered by a Pendle factory means
// the two indexes disagree, and the scan must fail rather than report an unattributed balance.
func (i *pendleIndexer) graphqlTokens(
	ctx context.Context,
	chainID ChainID,
	tokens []common.Address,
	block uint64,
) (map[common.Address]pendlePositionRef, error) {
	registered := make(map[common.Address]pendlePositionRef, len(tokens))
	for start := 0; start < len(tokens); start += pendleIndexerPageSize {
		end := min(start+pendleIndexerPageSize, len(tokens))
		ids := make([]string, 0, end-start)
		for _, token := range tokens[start:end] {
			ids = append(ids, pendleTokenRowID(chainID, token))
		}
		var payload pendleTokensResponse
		err := i.api.doJSON(
			ctx,
			http.MethodPost,
			i.config.GraphQLURL,
			map[string]any{
				"query": pendleTokensQuery,
				"variables": map[string]any{
					"ids": ids, "first": pendleIndexerPageSize,
					"block": strconv.FormatUint(block, 10),
				},
			},
			&payload,
		)
		if err != nil {
			return nil, err
		}
		if len(payload.Errors) > 0 {
			messages := make([]string, 0, len(payload.Errors))
			for _, graphQLError := range payload.Errors {
				messages = append(messages, graphQLError.Message)
			}
			return nil, pendleGraphQLError(messages)
		}
		for _, row := range payload.Data.Tokens {
			if row.ChainID != int(chainID) || !common.IsHexAddress(row.Address) ||
				!validPendleTokenKind(pendleTokenKind(row.Kind)) || !common.IsHexAddress(row.PT) {
				return nil, fmt.Errorf("GraphQL returned malformed Pendle token row %q", row.ID)
			}
			address := common.HexToAddress(row.Address)
			if row.ID != pendleTokenRowID(chainID, address) {
				return nil, fmt.Errorf("GraphQL returned foreign Pendle token row %q", row.ID)
			}
			createdBlock, parseErr := strconv.ParseUint(row.CreatedBlock, 10, 64)
			if parseErr != nil || createdBlock == 0 || createdBlock > block {
				return nil, fmt.Errorf("GraphQL returned invalid creation block for Pendle token %q", row.ID)
			}
			expiry := uint64(0)
			if row.Expiry != "" {
				expiry, parseErr = strconv.ParseUint(row.Expiry, 10, 64)
				if parseErr != nil {
					return nil, fmt.Errorf("GraphQL returned invalid expiry for Pendle token %q", row.ID)
				}
			}
			sy := common.Address{}
			if row.SY != "" {
				if !common.IsHexAddress(row.SY) {
					return nil, fmt.Errorf("GraphQL returned invalid SY for Pendle token %q", row.ID)
				}
				sy = common.HexToAddress(row.SY)
			}
			registered[address] = pendlePositionRef{
				Token: address, Kind: pendleTokenKind(row.Kind), PT: common.HexToAddress(row.PT),
				SY: sy, Expiry: expiry, CreatedBlock: createdBlock,
			}
		}
	}
	return registered, nil
}

// MarketsForPT groups the markets the index recorded for each of the given PTs, in the index's
// own id order so a caller choosing between them chooses deterministically.
//
// Every row is checked back against the PTs that were asked for. A GraphQL endpoint drops a
// filter it does not understand and answers as if it had not been given, which for this query
// would be every market on the chain rather than an empty page — the failure mode that made the
// Euler schema mismatch silent. Rejecting a foreign row turns that into a loud error.
func (i *pendleIndexer) MarketsForPT(
	ctx context.Context,
	block BlockRef,
	pts []common.Address,
) (map[common.Address][]common.Address, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	// Valuation asks for markets after PositionRefs has released the lane, so this lookup takes
	// its own slot: every indexer request counts against the shared API key's bound.
	release, err := lockSentioLane(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	requested := make(map[string]common.Address, len(pts))
	for _, pt := range pts {
		requested[strings.ToLower(pt.Hex())] = pt
	}
	keys := make([]string, 0, len(requested))
	for key := range requested {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	markets := make(map[common.Address][]common.Address, len(keys))
	total := 0
	for start := 0; start < len(keys); start += pendleMarketLookupBatch {
		end := min(start+pendleMarketLookupBatch, len(keys))
		batch := keys[start:end]
		after := ""
		for {
			var payload pendleMarketsResponse
			err := i.api.doJSON(
				ctx,
				http.MethodPost,
				i.config.GraphQLURL,
				map[string]any{
					"query": pendleMarketsQuery,
					"variables": map[string]any{
						"pts": batch, "chainId": int(block.ChainID), "after": after,
						"first": pendleIndexerPageSize,
						"block": strconv.FormatUint(block.Number, 10),
					},
				},
				&payload,
			)
			if err != nil {
				return nil, err
			}
			if len(payload.Errors) > 0 {
				messages := make([]string, 0, len(payload.Errors))
				for _, graphQLError := range payload.Errors {
					messages = append(messages, graphQLError.Message)
				}
				return nil, pendleGraphQLError(messages)
			}
			rows := payload.Data.Markets
			for _, row := range rows {
				if row.ChainID != int(block.ChainID) || pendleTokenKind(row.Kind) != pendleLP ||
					!common.IsHexAddress(row.Address) || !common.IsHexAddress(row.PT) ||
					row.ID <= after {
					return nil, fmt.Errorf("GraphQL returned malformed Pendle market row %q", row.ID)
				}
				market := common.HexToAddress(row.Address)
				if row.ID != pendleTokenRowID(block.ChainID, market) {
					return nil, fmt.Errorf("GraphQL returned foreign Pendle market row %q", row.ID)
				}
				pt, wanted := requested[strings.ToLower(row.PT)]
				if !wanted {
					return nil, fmt.Errorf(
						"GraphQL returned a Pendle market for unrequested PT %q", row.PT,
					)
				}
				markets[pt] = append(markets[pt], market)
				total++
				if total > pendleMaxPositionRefs {
					return nil, fmt.Errorf("GraphQL returned more than %d Pendle markets", pendleMaxPositionRefs)
				}
			}
			if len(rows) < pendleIndexerPageSize {
				break
			}
			after = rows[len(rows)-1].ID
		}
	}
	return markets, nil
}

type pendleIndexedSnapshot struct {
	Block uint64
	Refs  []pendlePositionRef
	// Lag notes a live read that trails its pin by more than indexMaxLag.
	Lag *indexLagNote
}

// presenceProbe asks every Pendle chain at once whether the account holds any PT, YT or LP
// position, so a scan queries only the chains where it does.
func (i *pendleIndexer) presenceProbe(account common.Address) presenceProbe {
	owner := strings.ToLower(account.Hex())
	return presenceProbe{
		name:           "pendle",
		config:         i.config,
		requiredChains: i.requiredChains,
		fields: []presenceField{{
			entity: "pendlePositionRefs",
			where: func(chainID ChainID) string {
				return fmt.Sprintf("chainId: %d, account: \"%s\", id_starts_with: \"%s\"", chainID, owner, pendleRefRowPrefix(chainID, account))
			},
		}},
	}
}

func (i *pendleIndexer) prefetchPresence(ctx context.Context, account common.Address) {
	i.api.prefetchPresence(ctx, i.presenceProbe(account), strings.ToLower(account.Hex()))
}

func (i *pendleIndexer) indexedRefs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
	start indexStart,
) (pendleIndexedSnapshot, error) {
	statuses, queryBlock, err := indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, forceRefresh)
	})
	if err != nil {
		return pendleIndexedSnapshot{}, err
	}
	if proof, empty := i.api.chainProvenEmpty(ctx, i.presenceProbe(account), block, strings.ToLower(account.Hex()), statuses); empty {
		// The proof may predate this status read; it is the block the answer holds at.
		return pendleIndexedSnapshot{Block: proof.QueryBlock, Refs: make([]pendlePositionRef, 0)}, nil
	}
	// Only the pages and the token lookup take a lane slot: the status read needs none, and a
	// chain the probe proved empty sends nothing.
	release, err := lockSentioLane(ctx)
	if err != nil {
		return pendleIndexedSnapshot{}, err
	}
	defer release()
	after := pendleRefRowPrefix(block.ChainID, account)
	tokens := make([]common.Address, 0)
	kinds := make([]pendleTokenKind, 0)
	var checkpointBlock, checkpointMS uint64
	var lag *indexLagNote
	firstPage := true
	for {
		page, pageErr := i.graphqlPage(ctx, block.ChainID, account, after, queryBlock, start)
		if pageErr != nil {
			return pendleIndexedSnapshot{}, pageErr
		}
		if firstPage {
			note, validationErr := checkIndexCheckpoint(block, page.CheckpointBlock, page.CheckpointMS)
			if validationErr != nil {
				return pendleIndexedSnapshot{}, validationErr
			}
			lag = note
			checkpointBlock, checkpointMS = page.CheckpointBlock, page.CheckpointMS
			firstPage = false
		} else if page.CheckpointBlock != checkpointBlock || page.CheckpointMS != checkpointMS {
			return pendleIndexedSnapshot{}, fmt.Errorf("GraphQL pagination changed checkpoint")
		}
		for index, rowID := range page.RowIDs {
			if rowID <= after {
				return pendleIndexedSnapshot{}, fmt.Errorf("Pendle position rows are not ordered")
			}
			after = rowID
			tokens = append(tokens, page.Tokens[index])
			kinds = append(kinds, page.Kinds[index])
		}
		if len(tokens) > pendleMaxPositionRefs {
			return pendleIndexedSnapshot{}, fmt.Errorf(
				"account has more than %d indexed Pendle positions", pendleMaxPositionRefs,
			)
		}
		if len(page.RowIDs) < pendleIndexerPageSize {
			break
		}
	}
	registered, err := i.graphqlTokens(ctx, block.ChainID, tokens, queryBlock)
	if err != nil {
		return pendleIndexedSnapshot{}, err
	}
	refs := make([]pendlePositionRef, 0, len(tokens))
	for index, token := range tokens {
		ref, exists := registered[token]
		if !exists {
			return pendleIndexedSnapshot{}, fmt.Errorf(
				"Pendle position index references token %s absent from the factory index", token,
			)
		}
		if ref.Kind != kinds[index] {
			return pendleIndexedSnapshot{}, fmt.Errorf(
				"Pendle indexes disagree on the kind of token %s", token,
			)
		}
		refs = append(refs, ref)
	}
	return pendleIndexedSnapshot{Block: queryBlock, Refs: refs, Lag: lag}, nil
}

// PositionRefs returns the account's position refs on block's chain, with a lag note when the
// index trails a live pin by more than indexMaxLag.
func (i *pendleIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]pendlePositionRef, *indexLagNote, error) {
	chain, supported := pendleChainConfigs[block.ChainID]
	if !supported {
		return nil, nil, fmt.Errorf("Pendle is not configured on chain %d", block.ChainID)
	}
	i.api.awaitPresence(ctx, i.presenceProbe(account), strings.ToLower(account.Hex()))
	indexed, err := i.indexedRefs(ctx, block, account, indexStart{client: client, block: chain.ActivationBlock})
	if err != nil {
		return nil, nil, fmt.Errorf("Pendle index query: %w", err)
	}
	return indexed.Refs, indexed.Lag, nil
}
