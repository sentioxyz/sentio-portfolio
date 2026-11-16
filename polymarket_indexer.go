package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// polymarketIndexer reads Polymarket's index: Position rows keyed by wallet and outcome token, and
// OutcomeToken rows naming each token's condition and outcome. It is on one chain, so it does not
// probe.
//
// A Position row stays after its position closes. It carries the net of every transfer of the
// token in and out of the wallet since the index's start, and, from the first transfer out, the
// wallet's balance just before the start; both are zero only for a position opened and closed since
// the start. Only the other rows are read, so a trader's thousands of closed positions cost
// nothing. That trusts the index to have summed each transfer, as reading every row trusts it to
// have seen each one; the amount is still the contract's balance at the pin.
//
// Its processor writes no IndexerCheckpoint. Deposit wallets are created every few blocks, and
// the processor writes a Wallet row for each at its block, so the newest Wallet row certifies a
// read in its place (indexLatestRow). Before the first wallet the index's start stands in.
type polymarketIndexer struct {
	index *sentioIndex
}

func newPolymarketIndexer(config SentioIndexerConfig) *polymarketIndexer {
	return &polymarketIndexer{index: &sentioIndex{
		name:   "polymarket",
		api:    newSentioAPIClient(),
		config: config,
		chains: []ChainID{Polygon},
		start:  func(ChainID) uint64 { return polymarketIndexStart },
		latest: &indexLatestRow{Entity: "wallets", BlockField: "createdBlock", SecondsField: "createdAt"},
	}}
}

type polymarketPositionRow struct {
	Wallet  string `json:"wallet"`
	TokenID string `json:"tokenId"`
}

// polymarketPositionPrefix starts the id of every Position row of account: the lowercase wallet
// and a hyphen, then the token id.
func polymarketPositionPrefix(account common.Address) string {
	return strings.ToLower(account.Hex()) + "-"
}

func polymarketPositionStream(account common.Address) *indexStream[polymarketPositionRow, *big.Int] {
	prefix := polymarketPositionPrefix(account)
	return &indexStream[polymarketPositionRow, *big.Int]{
		Entity: "positions", Account: account, MaxRows: polymarketMaxIndexedPositions,
		Where: func(ChainID) indexWhere {
			return indexWhere{Prefix: prefix, Terms: []indexTerm{
				{"wallet", strings.ToLower(account.Hex())},
				// A closed position keeps its row, so an active trader's rows run to the tens of
				// thousands; only a row that may still hold something names a token to read.
				{"or", indexAnyOf{{"balance_not", "0"}, {"seedBalance_not", "0"}}},
			}}
		},
		Decode: func(_ ChainID, id string, row polymarketPositionRow) (*big.Int, error) {
			tokenID, ok := polymarketParseTokenID(row.TokenID)
			if !ok || row.Wallet != strings.ToLower(account.Hex()) || id != prefix+row.TokenID {
				return nil, fmt.Errorf("GraphQL returned malformed Polymarket position row %q", id)
			}
			return tokenID, nil
		},
	}
}

type polymarketOutcomeTokenRow struct {
	ConditionID  string `json:"conditionId"`
	OutcomeIndex int    `json:"outcomeIndex"`
	IndexSet     int64  `json:"indexSet"`
}

func polymarketOutcomeTokenStream(ids []*big.Int) *indexStream[polymarketOutcomeTokenRow, polymarketToken] {
	wanted := make(map[string]*big.Int, len(ids))
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := wanted[id.String()]; !duplicate {
			wanted[id.String()] = id
			values = append(values, id.String())
		}
	}
	return &indexStream[polymarketOutcomeTokenRow, polymarketToken]{
		Entity: "outcomeTokens", MaxRows: len(values),
		Where: func(ChainID) indexWhere {
			return indexWhere{Terms: []indexTerm{{"id_in", values}}}
		},
		Decode: func(_ ChainID, id string, row polymarketOutcomeTokenRow) (polymarketToken, error) {
			tokenID, requested := wanted[id]
			if !requested {
				return polymarketToken{}, fmt.Errorf("GraphQL returned unrequested Polymarket outcome token %q", id)
			}
			if !polymarketIsConditionID(row.ConditionID) {
				return polymarketToken{}, fmt.Errorf("GraphQL returned malformed Polymarket outcome token %q", id)
			}
			return polymarketToken{
				ID: tokenID, ConditionID: common.HexToHash(row.ConditionID),
				OutcomeIndex: row.OutcomeIndex, IndexSet: row.IndexSet,
			}, nil
		},
	}
}

func (i *polymarketIndexer) TokenIDs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) (polymarketRead, error) {
	positions := polymarketPositionStream(account)
	at, err := i.index.read(ctx, client, block, positions)
	if err != nil {
		return polymarketRead{}, fmt.Errorf("Polymarket index query: %w", err)
	}
	return polymarketRead{
		IndexerBlock: at.QueryBlock,
		TokenIDs:     positions.Rows(),
		Tokens: func(ctx context.Context, ids []*big.Int) (map[string]polymarketToken, error) {
			stream := polymarketOutcomeTokenStream(ids)
			if err := at.read(ctx, stream); err != nil {
				return nil, err
			}
			tokens := make(map[string]polymarketToken, len(stream.Rows()))
			for _, token := range stream.Rows() {
				tokens[token.ID.String()] = token
			}
			return tokens, nil
		},
	}, nil
}

// polymarketParseTokenID reads a token id the way the index writes it: a uint256 in canonical
// decimal.
func polymarketParseTokenID(value string) (*big.Int, bool) {
	id, ok := new(big.Int).SetString(value, 10)
	if !ok || id.Sign() < 0 || id.BitLen() > 256 || id.String() != value {
		return nil, false
	}
	return id, true
}

// polymarketIsConditionID reports whether value is a condition id as the index writes it: 32
// bytes of lowercase hex.
func polymarketIsConditionID(value string) bool {
	hex, found := strings.CutPrefix(value, "0x")
	if !found || len(hex) != 64 || strings.ToLower(hex) != hex {
		return false
	}
	_, ok := new(big.Int).SetString(hex, 16)
	return ok
}
