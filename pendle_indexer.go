package portfolio

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	// pendleTokenLookupBatch bounds the tokens named in one registration lookup. One fewer than a
	// page, so each batch is answered by one request.
	pendleTokenLookupBatch = indexPageSize - 1
	// pendleMarketLookupBatch bounds the PTs named in one market lookup. An account holding
	// hundreds of PTs would otherwise build a single query longer than the endpoint accepts.
	pendleMarketLookupBatch = 100
	pendleMaxPositionRefs   = 4_096
)

type pendleIndexer struct {
	index *sentioIndex
}

func newPendleIndexer(config SentioIndexerConfig) *pendleIndexer {
	return &pendleIndexer{index: &sentioIndex{
		name:   "pendle",
		api:    newSentioAPIClient(),
		config: config,
		chains: deploymentChains(pendleChainConfigs),
		probe:  true,
		start:  func(chainID ChainID) uint64 { return pendleChainConfigs[chainID].ActivationBlock },
	}}
}

func pendleTokenRowID(chainID ChainID, token common.Address) string {
	return fmt.Sprintf("%d:%s", chainID, strings.ToLower(token.Hex()))
}

type pendleRefRow struct {
	ChainID int    `json:"chainId"`
	Account string `json:"account"`
	Token   string `json:"token"`
	Kind    string `json:"kind"`
}

// pendleHeldToken is a token the position index records the account holding, and its kind.
type pendleHeldToken struct {
	Token common.Address
	Kind  pendleTokenKind
}

func pendleRefStream(account common.Address) *indexStream[pendleRefRow, pendleHeldToken] {
	return &indexStream[pendleRefRow, pendleHeldToken]{
		Entity: "pendlePositionRefs", Account: account, MaxRows: pendleMaxPositionRefs,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{
				{"chainId", int(chainID)}, {"account", strings.ToLower(account.Hex())},
			}}
		},
		Decode: func(chainID ChainID, id string, row pendleRefRow) (pendleHeldToken, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
				!common.IsHexAddress(row.Token) || !validPendleTokenKind(pendleTokenKind(row.Kind)) {
				return pendleHeldToken{}, fmt.Errorf("GraphQL returned malformed Pendle position row %q", id)
			}
			token := common.HexToAddress(row.Token)
			if id != indexAccountPrefix(chainID, account)+strings.ToLower(token.Hex()) {
				return pendleHeldToken{}, fmt.Errorf("GraphQL returned foreign Pendle position row %q", id)
			}
			return pendleHeldToken{Token: token, Kind: pendleTokenKind(row.Kind)}, nil
		},
	}
}

type pendleTokenRow struct {
	ChainID      int    `json:"chainId"`
	Address      string `json:"address"`
	Kind         string `json:"kind"`
	PT           string `json:"pt"`
	SY           string `json:"sy"`
	Expiry       string `json:"expiry"`
	CreatedBlock string `json:"createdBlock"`
}

// pendleTokenStream resolves the factory registration of tokens, created no later than the read's
// block.
func pendleTokenStream(chainID ChainID, tokens []common.Address, queryBlock uint64) *indexStream[pendleTokenRow, pendlePositionRef] {
	ids := make([]string, 0, len(tokens))
	for _, token := range tokens {
		ids = append(ids, pendleTokenRowID(chainID, token))
	}
	sort.Strings(ids)
	return &indexStream[pendleTokenRow, pendlePositionRef]{
		Entity: "pendleTokens", MaxRows: len(ids),
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: fmt.Sprintf("%d:", chainID), Terms: []indexTerm{{"id_in", ids}}}
		},
		Decode: func(chainID ChainID, id string, row pendleTokenRow) (pendlePositionRef, error) {
			if row.ChainID != int(chainID) || !common.IsHexAddress(row.Address) ||
				!validPendleTokenKind(pendleTokenKind(row.Kind)) || !common.IsHexAddress(row.PT) {
				return pendlePositionRef{}, fmt.Errorf("GraphQL returned malformed Pendle token row %q", id)
			}
			address := common.HexToAddress(row.Address)
			if id != pendleTokenRowID(chainID, address) {
				return pendlePositionRef{}, fmt.Errorf("GraphQL returned foreign Pendle token row %q", id)
			}
			createdBlock, err := strconv.ParseUint(row.CreatedBlock, 10, 64)
			if err != nil || createdBlock == 0 || createdBlock > queryBlock {
				return pendlePositionRef{}, fmt.Errorf("GraphQL returned invalid creation block for Pendle token %q", id)
			}
			expiry := uint64(0)
			if row.Expiry != "" {
				if expiry, err = strconv.ParseUint(row.Expiry, 10, 64); err != nil {
					return pendlePositionRef{}, fmt.Errorf("GraphQL returned invalid expiry for Pendle token %q", id)
				}
			}
			sy := common.Address{}
			if row.SY != "" {
				if !common.IsHexAddress(row.SY) {
					return pendlePositionRef{}, fmt.Errorf("GraphQL returned invalid SY for Pendle token %q", id)
				}
				sy = common.HexToAddress(row.SY)
			}
			return pendlePositionRef{
				Token: address, Kind: pendleTokenKind(row.Kind), PT: common.HexToAddress(row.PT),
				SY: sy, Expiry: expiry, CreatedBlock: createdBlock,
			}, nil
		},
	}
}

type pendleMarketRow struct {
	ChainID int    `json:"chainId"`
	Address string `json:"address"`
	Kind    string `json:"kind"`
	PT      string `json:"pt"`
}

// pendleMarketKey is a market the index recorded for a PT.
type pendleMarketKey struct {
	PT     common.Address
	Market common.Address
}

// pendleMarketStream finds the markets created for pts, each lowercase hex. Every row is checked
// back against the PTs asked for: a GraphQL endpoint drops a filter it does not understand and
// answers as if it had not been given, which for this query would be every market on the chain
// rather than an empty page — the failure mode that made the Euler schema mismatch silent.
func pendleMarketStream(pts []string, maxRows int) *indexStream[pendleMarketRow, pendleMarketKey] {
	requested := make(map[string]struct{}, len(pts))
	for _, pt := range pts {
		requested[pt] = struct{}{}
	}
	return &indexStream[pendleMarketRow, pendleMarketKey]{
		Entity: "pendleTokens", MaxRows: maxRows,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: fmt.Sprintf("%d:", chainID), Terms: []indexTerm{
				{"chainId", int(chainID)}, {"kind", string(pendleLP)}, {"pt_in", pts},
			}}
		},
		Decode: func(chainID ChainID, id string, row pendleMarketRow) (pendleMarketKey, error) {
			if row.ChainID != int(chainID) || pendleTokenKind(row.Kind) != pendleLP ||
				!common.IsHexAddress(row.Address) || !common.IsHexAddress(row.PT) {
				return pendleMarketKey{}, fmt.Errorf("GraphQL returned malformed Pendle market row %q", id)
			}
			market := common.HexToAddress(row.Address)
			if id != pendleTokenRowID(chainID, market) {
				return pendleMarketKey{}, fmt.Errorf("GraphQL returned foreign Pendle market row %q", id)
			}
			if _, wanted := requested[strings.ToLower(row.PT)]; !wanted {
				return pendleMarketKey{}, fmt.Errorf("GraphQL returned a Pendle market for unrequested PT %q", row.PT)
			}
			return pendleMarketKey{PT: common.HexToAddress(row.PT), Market: market}, nil
		},
	}
}

// prefetchPresence asks every Pendle chain at once whether the account holds any PT, YT or LP
// position, so a scan queries only the chains where it does.
func (i *pendleIndexer) prefetchPresence(ctx context.Context, account common.Address) {
	i.index.prefetchPresence(ctx, pendleRefStream(account))
}

// PositionRefs returns the account's position refs on block's chain, and where the index answered,
// for the market lookup valuation makes later.
//
// It resolves the factory registration of every token the account holds, which doubles as a
// consistency check: a position row whose token no Pendle factory registered means the two indexes
// disagree, and the scan must fail rather than report an unattributed balance.
func (i *pendleIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]pendlePositionRef, indexAt, error) {
	if _, supported := pendleChainConfigs[block.ChainID]; !supported {
		return nil, indexAt{}, fmt.Errorf("Pendle is not configured on chain %d", block.ChainID)
	}
	held := pendleRefStream(account)
	at, err := i.index.read(ctx, client, block, held)
	if err != nil {
		return nil, indexAt{}, fmt.Errorf("Pendle index query: %w", err)
	}
	tokens := make([]common.Address, 0, len(held.Rows()))
	for _, ref := range held.Rows() {
		tokens = append(tokens, ref.Token)
	}
	registered := make(map[common.Address]pendlePositionRef, len(tokens))
	for batch := range slices.Chunk(tokens, pendleTokenLookupBatch) {
		lookup := pendleTokenStream(block.ChainID, batch, at.QueryBlock)
		if err := at.read(ctx, lookup); err != nil {
			return nil, indexAt{}, fmt.Errorf("Pendle index query: %w", err)
		}
		for _, ref := range lookup.Rows() {
			registered[ref.Token] = ref
		}
	}
	refs := make([]pendlePositionRef, 0, len(tokens))
	for _, heldToken := range held.Rows() {
		ref, exists := registered[heldToken.Token]
		if !exists {
			return nil, indexAt{}, fmt.Errorf(
				"Pendle position index references token %s absent from the factory index", heldToken.Token,
			)
		}
		if ref.Kind != heldToken.Kind {
			return nil, indexAt{}, fmt.Errorf("Pendle indexes disagree on the kind of token %s", heldToken.Token)
		}
		refs = append(refs, ref)
	}
	return refs, at, nil
}

// MarketsForPT groups the markets the index recorded for each of the given PTs, in the index's
// own id order so a caller choosing between them chooses deterministically. It reads where the
// position refs were read, so the markets and the refs come from one snapshot of the index.
func (i *pendleIndexer) MarketsForPT(
	ctx context.Context,
	at indexAt,
	pts []common.Address,
) (map[common.Address][]common.Address, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	if at.index != i.index {
		return nil, fmt.Errorf("Pendle market lookup outside a Pendle index read")
	}
	requested := make(map[string]struct{}, len(pts))
	for _, pt := range pts {
		requested[strings.ToLower(pt.Hex())] = struct{}{}
	}
	keys := make([]string, 0, len(requested))
	for key := range requested {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	markets := make(map[common.Address][]common.Address, len(keys))
	total := 0
	for batch := range slices.Chunk(keys, pendleMarketLookupBatch) {
		lookup := pendleMarketStream(batch, pendleMaxPositionRefs)
		if err := at.read(ctx, lookup); err != nil {
			return nil, err
		}
		for _, row := range lookup.Rows() {
			markets[row.PT] = append(markets[row.PT], row.Market)
		}
		if total += len(lookup.Rows()); total > pendleMaxPositionRefs {
			return nil, fmt.Errorf("GraphQL returned more than %d Pendle markets", pendleMaxPositionRefs)
		}
	}
	return markets, nil
}
