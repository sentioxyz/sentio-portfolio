package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const ownerTokenMaxRefs = 4_096

type ownerTokenRef struct {
	Contract common.Address
	TokenID  *big.Int
}

// ownerTokenIndexer reads which tokens of a set of contracts the index records an owner holding.
// Its rows are keyed by contract and token, not by owner, so no id prefix bounds them and no probe
// can speak for them.
type ownerTokenIndexer struct {
	index *sentioIndex
}

func newOwnerTokenIndexer(config SentioIndexerConfig, requiredChains []ChainID) *ownerTokenIndexer {
	return &ownerTokenIndexer{index: &sentioIndex{
		name:   "owner-token",
		api:    newSentioAPIClient(),
		config: config,
		chains: append([]ChainID(nil), requiredChains...),
	}}
}

func ownerTokenRowID(chainID ChainID, contract common.Address, tokenID *big.Int) string {
	return fmt.Sprintf("%d:%s:%s", chainID, strings.ToLower(contract.Hex()), tokenID.String())
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

type ownerTokenRow struct {
	ChainID  int    `json:"chainId"`
	Owner    string `json:"owner"`
	Contract string `json:"contract"`
	TokenID  string `json:"tokenId"`
}

// ownerContractsWhere selects the rows of owner on a chain that belong to one of allowed. The ids
// of an index keyed by contract do not carry the owner, so no prefix bounds them.
func ownerContractsWhere(owner common.Address, allowed map[common.Address]struct{}) func(ChainID) indexWhere {
	contracts := sortedAllowedAddresses(allowed)
	return func(chainID ChainID) indexWhere {
		return indexWhere{Terms: []indexTerm{
			{"chainId", int(chainID)}, {"owner", strings.ToLower(owner.Hex())}, {"contract_in", contracts},
		}}
	}
}

func allowedContracts(contracts []common.Address) map[common.Address]struct{} {
	allowed := make(map[common.Address]struct{}, len(contracts))
	for _, contract := range contracts {
		allowed[contract] = struct{}{}
	}
	return allowed
}

func ownerTokenStream(owner common.Address, contracts []common.Address) *indexStream[ownerTokenRow, ownerTokenRef] {
	allowed := allowedContracts(contracts)
	return &indexStream[ownerTokenRow, ownerTokenRef]{
		Entity: "portfolioOwnerTokenRefs", MaxRows: ownerTokenMaxRefs, Where: ownerContractsWhere(owner, allowed),
		Decode: func(chainID ChainID, id string, row ownerTokenRow) (ownerTokenRef, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Owner, owner.Hex()) || !common.IsHexAddress(row.Contract) {
				return ownerTokenRef{}, fmt.Errorf("GraphQL returned malformed owner-token row %q", id)
			}
			contract := common.HexToAddress(row.Contract)
			if _, exists := allowed[contract]; !exists {
				return ownerTokenRef{}, fmt.Errorf("GraphQL returned unexpected token contract")
			}
			tokenID, valid := new(big.Int).SetString(row.TokenID, 10)
			if !valid || tokenID.Sign() < 0 || id != ownerTokenRowID(chainID, contract, tokenID) {
				return ownerTokenRef{}, fmt.Errorf("GraphQL returned malformed owner-token row %q", id)
			}
			return ownerTokenRef{Contract: contract, TokenID: tokenID}, nil
		},
	}
}

// PositionRefs returns the tokens of contracts the index records owner holding on block's chain. A
// token the owner gave up after the indexed block is still among them, so callers check ownership
// at the pin.
func (i *ownerTokenIndexer) PositionRefs(
	ctx context.Context,
	block BlockRef,
	owner common.Address,
	contracts []common.Address,
) ([]ownerTokenRef, error) {
	if len(contracts) == 0 {
		return nil, nil
	}
	tokens := ownerTokenStream(owner, contracts)
	if _, err := i.index.read(ctx, nil, block, tokens); err != nil {
		return nil, err
	}
	refs := tokens.Rows()
	sort.Slice(refs, func(left, right int) bool {
		if refs[left].Contract != refs[right].Contract {
			return strings.ToLower(refs[left].Contract.Hex()) < strings.ToLower(refs[right].Contract.Hex())
		}
		return refs[left].TokenID.Cmp(refs[right].TokenID) < 0
	})
	return refs, nil
}
