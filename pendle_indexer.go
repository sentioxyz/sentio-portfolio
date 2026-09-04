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
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	pendleIndexerPageSize = 500
	// pendleMarketLookupBatch bounds the PTs named in one market lookup. An account holding
	// hundreds of PTs would otherwise build a single query longer than the endpoint accepts.
	pendleMarketLookupBatch     = 100
	pendleMaxPositionRefs       = 4_096
	pendleMaxTailContracts      = 256
	pendleMaxRPCTailBlocks      = 2_048
	pendleIndexerLiveMaxLag     = 15 * time.Minute
	pendleIndexerBackfillMaxLag = 7*24*time.Hour + 30*time.Minute
)

var pendleTransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

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
    where: { chainId: $chainId, account: $account, id_gt: $after }
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
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Refs []struct {
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
) (pendleWalletPage, error) {
	var payload pendleWalletResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": pendleWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "account": strings.ToLower(account.Hex()),
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
	if len(payload.Data.Checkpoints) != 1 {
		return pendleWalletPage{}, fmt.Errorf("GraphQL returned %d checkpoints", len(payload.Data.Checkpoints))
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return pendleWalletPage{}, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return pendleWalletPage{}, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	prefix := pendleRefRowPrefix(chainID, account)
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

func validatePendleCheckpoint(block BlockRef, page pendleWalletPage) error {
	if page.CheckpointBlock > block.Number {
		return fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", page.CheckpointBlock, block.Number)
	}
	checkpointSeconds := page.CheckpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp > checkpointSeconds {
		lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
		maximumLag := pendleIndexerLiveMaxLag
		if block.Fixed {
			maximumLag = pendleIndexerBackfillMaxLag
		}
		if lag > maximumLag {
			return fmt.Errorf("indexer checkpoint is stale by %s", lag)
		}
	}
	return nil
}

type pendleIndexedSnapshot struct {
	Block uint64
	Refs  []pendlePositionRef
}

func (i *pendleIndexer) indexedRefs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
) (pendleIndexedSnapshot, error) {
	statuses, err := i.api.chainStatuses(ctx, i.config, i.requiredChains, false)
	if err != nil {
		return pendleIndexedSnapshot{}, err
	}
	status := statuses[block.ChainID]
	if status.State != "PROCESSING_LATEST" {
		return pendleIndexedSnapshot{}, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID, status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	if block.Number > status.ProcessedBlock && block.Number-status.ProcessedBlock > pendleMaxRPCTailBlocks {
		statuses, err = i.api.chainStatuses(ctx, i.config, i.requiredChains, true)
		if err != nil {
			return pendleIndexedSnapshot{}, err
		}
		status = statuses[block.ChainID]
	}
	if status.State != "PROCESSING_LATEST" ||
		(block.Number > status.ProcessedBlock && block.Number-status.ProcessedBlock > pendleMaxRPCTailBlocks) {
		return pendleIndexedSnapshot{}, fmt.Errorf(
			"indexer RPC tail exceeds %d blocks for chain %d: processed=%d target=%d",
			pendleMaxRPCTailBlocks, block.ChainID, status.ProcessedBlock, block.Number,
		)
	}
	queryBlock := min(block.Number, status.ProcessedBlock)
	after := pendleRefRowPrefix(block.ChainID, account)
	tokens := make([]common.Address, 0)
	kinds := make([]pendleTokenKind, 0)
	var checkpointBlock, checkpointMS uint64
	firstPage := true
	for {
		page, pageErr := i.graphqlPage(ctx, block.ChainID, account, after, queryBlock)
		if pageErr != nil {
			return pendleIndexedSnapshot{}, pageErr
		}
		if firstPage {
			if validationErr := validatePendleCheckpoint(block, page); validationErr != nil {
				return pendleIndexedSnapshot{}, validationErr
			}
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
	return pendleIndexedSnapshot{Block: queryBlock, Refs: refs}, nil
}

// pendleTailCandidates collects the contracts that moved a balance to or from the account since
// the indexer checkpoint. Most are unrelated ERC20s; classification against the factories is
// what keeps them out.
func pendleTailCandidates(
	ctx context.Context,
	client *RPCClient,
	fromBlock uint64,
	toBlock uint64,
	account common.Address,
) ([]common.Address, error) {
	if fromBlock > toBlock {
		return nil, nil
	}
	accountTopic := common.BytesToHash(common.LeftPadBytes(account.Bytes(), 32))
	filters := [][][]common.Hash{
		{{pendleTransferTopic}, {accountTopic}},
		{{pendleTransferTopic}, nil, {accountTopic}},
	}
	seen := make(map[common.Address]struct{})
	for _, topics := range filters {
		logs, err := client.Logs(ctx, fromBlock, toBlock, nil, topics)
		if err != nil {
			return nil, fmt.Errorf("Pendle transfer RPC tail: %w", err)
		}
		for _, log := range logs {
			seen[log.Address] = struct{}{}
			if len(seen) > pendleMaxTailContracts {
				return nil, fmt.Errorf(
					"RPC tail contains more than %d transfer contracts", pendleMaxTailContracts,
				)
			}
		}
	}
	result := make([]common.Address, 0, len(seen))
	for address := range seen {
		result = append(result, address)
	}
	sort.Slice(result, func(left, right int) bool {
		return strings.ToLower(result[left].Hex()) < strings.ToLower(result[right].Hex())
	})
	return result, nil
}

// pendleClassifyTail decides which tail candidates are really Pendle tokens. PT, YT and market
// contracts all name their creator through factory(), which narrows the field to the configured
// generations in one call each; the factory is then asked to confirm, because a contract can
// claim any creator it likes but cannot make a real factory vouch for it. That confirmation is
// also what keeps a fork's tokens out.
func pendleClassifyTail(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	candidates []common.Address,
	chain pendleChainConfig,
) ([]pendlePositionRef, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	yieldFactories := make(map[common.Address]struct{}, len(chain.Generations))
	marketFactories := make(map[common.Address]struct{}, len(chain.Generations))
	for _, generation := range chain.Generations {
		if generation.YieldContractFactoryWindow.ActiveAt(block.Number) {
			yieldFactories[generation.YieldContractFactory] = struct{}{}
		}
		if generation.MarketFactoryWindow.ActiveAt(block.Number) {
			marketFactories[generation.MarketFactory] = struct{}{}
		}
	}
	if len(yieldFactories) == 0 && len(marketFactories) == 0 {
		return nil, nil
	}
	// An unrelated ERC20 has no factory() at all, so the call reverts and the candidate drops.
	claims := make([]ContractCall, len(candidates))
	for index, candidate := range candidates {
		claims[index] = ContractCall{
			Contract: candidate, ABI: pendleTokenFactoryABI, Method: "factory",
		}
	}
	claimRows, err := client.ParallelCallsAllowFailure(ctx, block, claims)
	if err != nil {
		return nil, fmt.Errorf("Pendle factory claims: %w", err)
	}
	type probe struct {
		candidate common.Address
		kind      pendleTokenKind
	}
	calls := make([]ContractCall, 0, len(candidates)*2)
	probes := make([]probe, 0, cap(calls))
	for index, row := range claimRows {
		if row.Error != nil {
			continue
		}
		claimed, decodeErr := AddressAt(row.Values, 0)
		if decodeErr != nil {
			continue
		}
		candidate := candidates[index]
		if _, isYieldFactory := yieldFactories[claimed]; isYieldFactory {
			calls = append(calls,
				ContractCall{
					Contract: claimed, ABI: pendleYieldContractFactoryABI,
					Method: "isPT", Args: []any{candidate},
				},
				ContractCall{
					Contract: claimed, ABI: pendleYieldContractFactoryABI,
					Method: "isYT", Args: []any{candidate},
				},
			)
			probes = append(probes, probe{candidate, pendlePT}, probe{candidate, pendleYT})
			continue
		}
		if _, isMarketFactory := marketFactories[claimed]; isMarketFactory {
			calls = append(calls, ContractCall{
				Contract: claimed, ABI: pendleMarketFactoryABI,
				Method: "isValidMarket", Args: []any{candidate},
			})
			probes = append(probes, probe{candidate, pendleLP})
		}
	}
	if len(calls) == 0 {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle factory classification: %w", err)
	}
	owned := make(map[common.Address]pendleTokenKind)
	for index, row := range rows {
		claimed, decodeErr := BoolAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle factory classification: %w", decodeErr)
		}
		if !claimed {
			continue
		}
		if existing, seen := owned[probes[index].candidate]; seen && existing != probes[index].kind {
			return nil, fmt.Errorf(
				"Pendle factories disagree on the kind of token %s", probes[index].candidate,
			)
		}
		owned[probes[index].candidate] = probes[index].kind
	}
	classified := make([]common.Address, 0, len(owned))
	for candidate := range owned {
		classified = append(classified, candidate)
	}
	sort.Slice(classified, func(left, right int) bool {
		return strings.ToLower(classified[left].Hex()) < strings.ToLower(classified[right].Hex())
	})
	return pendleResolveTailRefs(ctx, client, block, classified, owned)
}

// pendleResolveTailRefs reads the maturity a tail token belongs to. A market reports its own
// triple through readTokens; PT and YT both expose SY() and expiry(), and only a YT names its
// sibling through PT().
func pendleResolveTailRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	classified []common.Address,
	kinds map[common.Address]pendleTokenKind,
) ([]pendlePositionRef, error) {
	refs := make([]pendlePositionRef, 0, len(classified))
	for _, token := range classified {
		kind := kinds[token]
		ref := pendlePositionRef{Token: token, Kind: kind}
		if kind == pendleLP {
			values, err := client.Call(ctx, block, token, pendleMarketABI, "readTokens")
			if err != nil {
				return nil, fmt.Errorf("Pendle market %s tokens: %w", token, err)
			}
			sy, decodeErr := AddressAt(values, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Pendle market %s tokens: %w", token, decodeErr)
			}
			principal, decodeErr := AddressAt(values, 1)
			if decodeErr != nil {
				return nil, fmt.Errorf("Pendle market %s tokens: %w", token, decodeErr)
			}
			ref.SY, ref.PT = sy, principal
			refs = append(refs, ref)
			continue
		}
		calls := []ContractCall{
			{Contract: token, ABI: pendlePrincipalTokenABI, Method: "SY"},
			{Contract: token, ABI: pendlePrincipalTokenABI, Method: "expiry"},
		}
		if kind == pendleYT {
			calls = append(calls, ContractCall{
				Contract: token, ABI: pendleYieldTokenABI, Method: "PT",
			})
		}
		rows, err := client.ParallelCalls(ctx, block, calls)
		if err != nil {
			return nil, fmt.Errorf("Pendle token %s maturity: %w", token, err)
		}
		sy, decodeErr := AddressAt(rows[0], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle token %s SY: %w", token, decodeErr)
		}
		expiry, decodeErr := BigIntAt(rows[1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle token %s expiry: %w", token, decodeErr)
		}
		if !expiry.IsUint64() {
			return nil, fmt.Errorf("Pendle token %s reported an out-of-range expiry", token)
		}
		ref.SY, ref.Expiry, ref.PT = sy, expiry.Uint64(), token
		if kind == pendleYT {
			principal, principalErr := AddressAt(rows[2], 0)
			if principalErr != nil {
				return nil, fmt.Errorf("Pendle YT %s principal: %w", token, principalErr)
			}
			ref.PT = principal
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func mergePendleRefs(indexed []pendlePositionRef, tail []pendlePositionRef) ([]pendlePositionRef, error) {
	merged := make(map[common.Address]pendlePositionRef, len(indexed)+len(tail))
	for _, ref := range indexed {
		merged[ref.Token] = ref
	}
	for _, ref := range tail {
		if _, exists := merged[ref.Token]; exists {
			continue
		}
		merged[ref.Token] = ref
	}
	if len(merged) > pendleMaxPositionRefs {
		return nil, fmt.Errorf("Pendle position identifiers exceed account bounds")
	}
	refs := make([]pendlePositionRef, 0, len(merged))
	for _, ref := range merged {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(left, right int) bool {
		return strings.ToLower(refs[left].Token.Hex()) < strings.ToLower(refs[right].Token.Hex())
	})
	return refs, nil
}

func (i *pendleIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]pendlePositionRef, error) {
	chain, supported := pendleChainConfigs[block.ChainID]
	if !supported {
		return nil, fmt.Errorf("Pendle is not configured on chain %d", block.ChainID)
	}
	indexed, err := func() (pendleIndexedSnapshot, error) {
		sentioQueryMu.Lock()
		defer sentioQueryMu.Unlock()
		return i.indexedRefs(ctx, block, account)
	}()
	if err != nil {
		return nil, fmt.Errorf("Pendle index query: %w", err)
	}
	if indexed.Block >= block.Number {
		return indexed.Refs, nil
	}
	candidates, err := pendleTailCandidates(ctx, client, indexed.Block+1, block.Number, account)
	if err != nil {
		return nil, err
	}
	tail, err := pendleClassifyTail(ctx, client, block, candidates, chain)
	if err != nil {
		return nil, err
	}
	return mergePendleRefs(indexed.Refs, tail)
}
