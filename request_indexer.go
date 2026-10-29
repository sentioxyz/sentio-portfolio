package portfolio

import (
	"context"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const accountRequestMaxRefs = 4_096

type accountRequestRef struct {
	Contract common.Address
	Key      string
}

// accountRequestSnapshot is the requests the index records open for an account as of Block. A
// request settled after Block is still among them, so callers skip one that reads empty at the pin.
type accountRequestSnapshot struct {
	Block uint64
	Refs  []accountRequestRef
}

// accountRequestIndexer reads the requests a set of contracts holds open for an owner. Its rows are
// keyed by contract and request, not by owner, so no id prefix bounds them and no probe can speak
// for them.
type accountRequestIndexer struct {
	index *sentioIndex
}

func newAccountRequestIndexer(
	config SentioIndexerConfig,
	requiredChains []ChainID,
) *accountRequestIndexer {
	return &accountRequestIndexer{index: &sentioIndex{
		name:   "account-request",
		api:    newSentioAPIClient(),
		config: config,
		chains: append([]ChainID(nil), requiredChains...),
	}}
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

type accountRequestRow struct {
	ChainID    int    `json:"chainId"`
	Owner      string `json:"owner"`
	Contract   string `json:"contract"`
	RequestKey string `json:"requestKey"`
}

func accountRequestStream(owner common.Address, contracts []common.Address) *indexStream[accountRequestRow, accountRequestRef] {
	allowed := allowedContracts(contracts)
	return &indexStream[accountRequestRow, accountRequestRef]{
		Entity: "portfolioAccountRequestRefs", MaxRows: accountRequestMaxRefs, Where: ownerContractsWhere(owner, allowed),
		Decode: func(chainID ChainID, id string, row accountRequestRow) (accountRequestRef, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Owner, owner.Hex()) ||
				!common.IsHexAddress(row.Contract) || !validAccountRequestKey(row.RequestKey) {
				return accountRequestRef{}, fmt.Errorf("GraphQL returned malformed account-request row %q", id)
			}
			contract := common.HexToAddress(row.Contract)
			if _, exists := allowed[contract]; !exists {
				return accountRequestRef{}, fmt.Errorf("GraphQL returned unexpected request contract")
			}
			if id != accountRequestRowID(chainID, contract, row.RequestKey) {
				return accountRequestRef{}, fmt.Errorf("GraphQL returned malformed account-request row %q", id)
			}
			return accountRequestRef{Contract: contract, Key: row.RequestKey}, nil
		},
	}
}

// IndexedRefs returns the requests the index records open for owner on block's chain, in contract
// and key order: the order of their ids.
func (i *accountRequestIndexer) IndexedRefs(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) (accountRequestSnapshot, error) {
	if len(contracts) == 0 {
		return accountRequestSnapshot{Block: block.Number}, nil
	}
	requests := accountRequestStream(owner, contracts)
	at, err := i.index.read(ctx, nil, block, requests)
	if err != nil {
		return accountRequestSnapshot{}, err
	}
	return accountRequestSnapshot{Block: at.QueryBlock, Refs: requests.Rows()}, nil
}
