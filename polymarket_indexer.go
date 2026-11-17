package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// polymarketIndexer reads Polymarket's index: Position rows keyed by wallet and outcome token, and
// OutcomeToken rows naming each token's condition and outcome. It is on one chain, so it does not
// probe.
//
// A Position row stays after its position closes. It carries the net of every transfer of the
// token in and out of the wallet since the index's start (balance), and, from the first transfer
// out (seeded), the wallet's balance just before the start (seedBalance); once seeded, their sum
// is the wallet's balance. Before its first transfer out a row's balance before the start is not
// known, so the row names a token to read whatever its net: shares held before the start and
// topped up by a remainder since are still held. A seeded row names one only when its sum is
// above the dust floor. The server selects the rows that can (a net above the floor, a balance
// before the start, or no transfer out yet) and the adapter drops the seeded rest. Market making
// leaves an active wallet tens of thousands of positions closed to a remainder, which cost nothing
// this way. That trusts the index to have summed each transfer, as reading every row trusts it to
// have seen each one; the amount is still the contract's balance at the pin.
//
// Its processor writes no IndexerCheckpoint. Deposit wallets are created every few blocks, and
// the processor writes a Wallet row for each at its block, so the newest Wallet row certifies a
// read in its place (indexLatestRow). Before the first wallet the index's start stands in.
//
// A lost bet's share stays held until redeemed, so a trader who never redeems holds thousands of
// tokens, each read, then looked up. The server answers about a thousand rows a second to one
// request, too slow for an interactive scan, so the index is read concurrently
// (sentioIndex.concurrent): the wallet's rows in one stream per leading digit of the token id, in
// large pages, and the lookup in batches of ids.
type polymarketIndexer struct {
	index *sentioIndex
}

func newPolymarketIndexer(config SentioIndexerConfig) *polymarketIndexer {
	return &polymarketIndexer{index: &sentioIndex{
		name:       "polymarket",
		api:        newSentioAPIClient(),
		config:     config,
		chains:     []ChainID{Polygon},
		start:      func(ChainID) uint64 { return polymarketIndexStart },
		latest:     &indexLatestRow{Entity: "wallets", BlockField: "createdBlock", SecondsField: "createdAt"},
		concurrent: true,
	}}
}

type polymarketPositionRow struct {
	Wallet      string `json:"wallet"`
	TokenID     string `json:"tokenId"`
	Balance     string `json:"balance"`
	SeedBalance string `json:"seedBalance"`
	Seeded      bool   `json:"seeded"`
}

// polymarketCandidate is a token a Position row names, and whether the wallet may hold more than
// the dust floor of it.
type polymarketCandidate struct {
	id   *big.Int
	held bool
}

const (
	// polymarketIndexPageSize is the rows one page of a position stream asks for. A page of five
	// thousand returns in about three and a half seconds, ten pages of five hundred in eight.
	polymarketIndexPageSize = 5_000
	// polymarketLookupBatch is the ids one outcome token lookup names; each batch is one request.
	polymarketLookupBatch = 1_000
)

// polymarketPositionPrefix starts the id of every Position row of account: the lowercase wallet
// and a hyphen, then the token id.
func polymarketPositionPrefix(account common.Address) string {
	return strings.ToLower(account.Hex()) + "-"
}

// polymarketPositionStreams read account's Position rows, one stream per leading digit of the
// token id, so the streams together select every row the wallet prefix does whose token id starts
// with a digit. A token id is a decimal integer, so no other row names one.
func polymarketPositionStreams(account common.Address) []*indexStream[polymarketPositionRow, polymarketCandidate] {
	streams := make([]*indexStream[polymarketPositionRow, polymarketCandidate], 0, 10)
	for digit := '0'; digit <= '9'; digit++ {
		streams = append(streams, polymarketPositionStream(account, string(digit)))
	}
	return streams
}

// polymarketPositionStream reads account's Position rows whose token id starts with lead.
func polymarketPositionStream(account common.Address, lead string) *indexStream[polymarketPositionRow, polymarketCandidate] {
	prefix := polymarketPositionPrefix(account)
	dust := big.NewInt(polymarketDustUnits)
	return &indexStream[polymarketPositionRow, polymarketCandidate]{
		Entity: "positions", Account: account, MaxRows: polymarketMaxIndexedPositions,
		PageSize: polymarketIndexPageSize,
		Where: func(ChainID) indexWhere {
			return indexWhere{Prefix: prefix + lead, Terms: []indexTerm{
				{"wallet", strings.ToLower(account.Hex())},
				// A closed position keeps its row. A seeded row with no balance before the start
				// holds its net since, so only one above the dust floor can hold a share worth
				// reading; a row with a balance before the start is selected, and its sum decides; a
				// row not yet seeded may hold shares from before the start, so it is selected too.
				{"or", indexAnyOf{
					{"balance_gt", strconv.Itoa(polymarketDustUnits)}, {"seedBalance_not", "0"}, {"seeded", false},
				}},
			}}
		},
		Decode: func(_ ChainID, id string, row polymarketPositionRow) (polymarketCandidate, error) {
			tokenID, ok := polymarketParseTokenID(row.TokenID)
			balance, balanceOK := new(big.Int).SetString(row.Balance, 10)
			seed, seedOK := new(big.Int).SetString(row.SeedBalance, 10)
			if !ok || !balanceOK || !seedOK || row.Wallet != strings.ToLower(account.Hex()) || id != prefix+row.TokenID {
				return polymarketCandidate{}, fmt.Errorf("GraphQL returned malformed Polymarket position row %q", id)
			}
			held := !row.Seeded || new(big.Int).Add(balance, seed).Cmp(dust) > 0
			return polymarketCandidate{id: tokenID, held: held}, nil
		},
	}
}

type polymarketOutcomeTokenRow struct {
	ConditionID  string `json:"conditionId"`
	OutcomeIndex int    `json:"outcomeIndex"`
	IndexSet     int64  `json:"indexSet"`
}

// polymarketOutcomeTokenStreams look ids up, polymarketLookupBatch to a stream.
func polymarketOutcomeTokenStreams(ids []*big.Int) []*indexStream[polymarketOutcomeTokenRow, polymarketToken] {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]*big.Int, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id.String()]; !duplicate {
			seen[id.String()] = struct{}{}
			unique = append(unique, id)
		}
	}
	streams := make([]*indexStream[polymarketOutcomeTokenRow, polymarketToken], 0, (len(unique)+polymarketLookupBatch-1)/polymarketLookupBatch)
	for start := 0; start < len(unique); start += polymarketLookupBatch {
		streams = append(streams, polymarketOutcomeTokenStream(unique[start:min(start+polymarketLookupBatch, len(unique))]))
	}
	return streams
}

// polymarketOutcomeTokenStream looks up ids, which are distinct, in one page.
func polymarketOutcomeTokenStream(ids []*big.Int) *indexStream[polymarketOutcomeTokenRow, polymarketToken] {
	wanted := make(map[string]*big.Int, len(ids))
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		wanted[id.String()] = id
		values = append(values, id.String())
	}
	return &indexStream[polymarketOutcomeTokenRow, polymarketToken]{
		// A page one past the ids holds every row they name and shows there is no other.
		Entity: "outcomeTokens", MaxRows: len(values), PageSize: len(values) + 1,
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
	positions := polymarketPositionStreams(account)
	streams := make([]indexStreamer, len(positions))
	for index, stream := range positions {
		streams[index] = stream
	}
	at, err := i.index.read(ctx, client, block, streams...)
	if err != nil {
		return polymarketRead{}, fmt.Errorf("Polymarket index query: %w", err)
	}
	// The streams follow one another in id order, so their rows do too.
	tokenIDs := make([]*big.Int, 0)
	rows := 0
	for _, stream := range positions {
		rows += len(stream.Rows())
		for _, candidate := range stream.Rows() {
			if candidate.held {
				tokenIDs = append(tokenIDs, candidate.id)
			}
		}
	}
	if rows > polymarketMaxIndexedPositions {
		return polymarketRead{}, fmt.Errorf("Polymarket index query: index holds more than %d positions rows", polymarketMaxIndexedPositions)
	}
	return polymarketRead{
		IndexerBlock: at.QueryBlock,
		TokenIDs:     tokenIDs,
		Tokens: func(ctx context.Context, ids []*big.Int) (map[string]polymarketToken, error) {
			lookups := polymarketOutcomeTokenStreams(ids)
			if len(lookups) == 0 {
				return map[string]polymarketToken{}, nil
			}
			streams := make([]indexStreamer, len(lookups))
			for index, stream := range lookups {
				streams[index] = stream
			}
			if err := at.read(ctx, streams...); err != nil {
				return nil, err
			}
			tokens := make(map[string]polymarketToken, len(ids))
			for _, stream := range lookups {
				for _, token := range stream.Rows() {
					tokens[token.ID.String()] = token
				}
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
