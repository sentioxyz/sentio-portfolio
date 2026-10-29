package portfolio

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	listaMaxIndexedMarkets = 4_096
	listaMaxIndexedVaults  = 4_096
)

type listaPositionRefs struct {
	IndexerBlock uint64
	MarketIDs    []common.Hash
	Vaults       []common.Address
}

type listaPositionIndexer interface {
	PositionRefs(
		ctx context.Context,
		client *RPCClient,
		block BlockRef,
		account common.Address,
		deployment listaMoolahDeployment,
	) (listaPositionRefs, error)
}

// listaIndexer reads Lista's Moolah index, which mirrors Morpho Blue's. It spans too few chains
// for the presence probe to pay for itself.
type listaIndexer struct {
	index *sentioIndex
}

func newListaIndexer(config SentioIndexerConfig) *listaIndexer {
	return &listaIndexer{index: &sentioIndex{
		name:   "lista",
		api:    newSentioAPIClient(),
		config: config,
		chains: []ChainID{Ethereum, BSC},
		start:  func(chainID ChainID) uint64 { return listaMoolahDeployments[chainID].ActivationBlock },
	}}
}

type listaVaultRow struct {
	ChainID int    `json:"chainId"`
	Account string `json:"account"`
	Vault   string `json:"vault"`
}

func listaVaultStream(account common.Address) *indexStream[listaVaultRow, common.Address] {
	return &indexStream[listaVaultRow, common.Address]{
		Entity: "listaVaultPositionRefs", Account: account, MaxRows: listaMaxIndexedVaults,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{
				{"chainId", int(chainID)}, {"account", strings.ToLower(account.Hex())},
			}}
		},
		Decode: func(chainID ChainID, id string, row listaVaultRow) (common.Address, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) || !common.IsHexAddress(row.Vault) {
				return common.Address{}, fmt.Errorf("GraphQL returned malformed vault row %q", id)
			}
			vault := common.HexToAddress(row.Vault)
			if id != indexAccountPrefix(chainID, account)+strings.ToLower(vault.Hex()) {
				return common.Address{}, fmt.Errorf("GraphQL returned foreign vault row %q", id)
			}
			return vault, nil
		},
	}
}

func dedupeListaRefs(indexed listaPositionRefs) (listaPositionRefs, error) {
	marketSet := make(map[common.Hash]struct{}, len(indexed.MarketIDs))
	for _, marketID := range indexed.MarketIDs {
		marketSet[marketID] = struct{}{}
	}
	indexed.MarketIDs = indexed.MarketIDs[:0]
	for marketID := range marketSet {
		indexed.MarketIDs = append(indexed.MarketIDs, marketID)
	}
	sort.Slice(indexed.MarketIDs, func(i, j int) bool {
		return indexed.MarketIDs[i].Hex() < indexed.MarketIDs[j].Hex()
	})
	vaultSet := make(map[common.Address]struct{}, len(indexed.Vaults))
	for _, vault := range indexed.Vaults {
		vaultSet[vault] = struct{}{}
	}
	indexed.Vaults = indexed.Vaults[:0]
	for vault := range vaultSet {
		indexed.Vaults = append(indexed.Vaults, vault)
	}
	sort.Slice(indexed.Vaults, func(i, j int) bool {
		return strings.ToLower(indexed.Vaults[i].Hex()) < strings.ToLower(indexed.Vaults[j].Hex())
	})
	if len(indexed.MarketIDs) > listaMaxIndexedMarkets || len(indexed.Vaults) > listaMaxIndexedVaults {
		return listaPositionRefs{}, fmt.Errorf("Lista position identifiers exceed account bounds")
	}
	return indexed, nil
}

func (i *listaIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment listaMoolahDeployment,
) (listaPositionRefs, error) {
	feeRecipientValues, err := client.Call(
		ctx, block, deployment.Address, listaMoolahABI, "feeRecipient",
	)
	if err != nil {
		return listaPositionRefs{}, fmt.Errorf("Lista fee recipient at pinned block: %w", err)
	}
	feeRecipient, err := AddressAt(feeRecipientValues, 0)
	if err != nil {
		return listaPositionRefs{}, fmt.Errorf("Lista fee recipient at pinned block: %w", err)
	}
	markets := moolahMarketStream("listaMarketPositionRefs", account, listaMaxIndexedMarkets)
	vaults := listaVaultStream(account)
	streams := []indexStreamer{markets, vaults}
	// The fee recipient holds a position in every market charging a fee.
	feeMarkets := moolahFeeMarketStream("listaFeeMarkets")
	if feeRecipient != (common.Address{}) && feeRecipient == account {
		streams = append(streams, feeMarkets)
	}
	at, err := i.index.read(ctx, client, block, streams...)
	if err != nil {
		return listaPositionRefs{}, fmt.Errorf("Lista index query: %w", err)
	}
	return dedupeListaRefs(listaPositionRefs{
		IndexerBlock: at.QueryBlock,
		MarketIDs:    append(markets.Rows(), activeFeeMarkets(feeMarkets.Rows())...),
		Vaults:       vaults.Rows(),
	})
}
