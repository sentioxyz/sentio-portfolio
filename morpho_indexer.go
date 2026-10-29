package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	morphoMaxIndexedMarkets = 4_096
	morphoMaxFeeMarketRows  = 8_192
	morphoMaxIndexedVaults  = 4_096
)

type morphoVaultVersion string

const (
	morphoVaultV1 morphoVaultVersion = "v1"
	morphoVaultV2 morphoVaultVersion = "v2"
)

type morphoVaultRef struct {
	Address common.Address
	Version morphoVaultVersion
}

type morphoPositionRefs struct {
	IndexerBlock uint64
	MarketIDs    []common.Hash
	Vaults       []morphoVaultRef
}

type morphoPositionIndexer interface {
	PositionRefs(
		ctx context.Context,
		client *RPCClient,
		block BlockRef,
		account common.Address,
		deployment morphoDeployment,
	) (morphoPositionRefs, error)
}

type morphoIndexer struct {
	index *sentioIndex
}

func newMorphoIndexer(config SentioIndexerConfig) *morphoIndexer {
	return &morphoIndexer{index: &sentioIndex{
		name:   "morpho-blue",
		api:    newSentioAPIClient(),
		config: config,
		chains: deploymentChains(morphoDeployments),
		probe:  true,
		start:  func(chainID ChainID) uint64 { return morphoDeployments[chainID].Window.ActivationBlock },
	}}
}

// moolahMarketRow is a market position ref of a Morpho Blue-shaped index: Morpho's own, and the
// Moolah fork Lista runs. Its id is the account's row prefix and the market ID.
type moolahMarketRow struct {
	ChainID  int    `json:"chainId"`
	Account  string `json:"account"`
	MarketID string `json:"marketId"`
}

// moolahMarketStream reads the markets account holds a position in.
func moolahMarketStream(entity string, account common.Address, maxRows int) *indexStream[moolahMarketRow, common.Hash] {
	return &indexStream[moolahMarketRow, common.Hash]{
		Entity: entity, Account: account, MaxRows: maxRows,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{
				{"chainId", int(chainID)}, {"account", strings.ToLower(account.Hex())},
			}}
		},
		Decode: func(chainID ChainID, id string, row moolahMarketRow) (common.Hash, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) || !validHash(row.MarketID) {
				return common.Hash{}, fmt.Errorf("GraphQL returned malformed market row %q", id)
			}
			marketID := common.HexToHash(row.MarketID)
			if id != indexAccountPrefix(chainID, account)+strings.ToLower(marketID.Hex()) {
				return common.Hash{}, fmt.Errorf("GraphQL returned foreign market row %q", id)
			}
			return marketID, nil
		},
	}
}

// moolahFeeMarketRow is a market whose fee accrues to the core contract's fee recipient. Its id is
// the chain and the market ID: the rows are the chain's, not an account's.
type moolahFeeMarketRow struct {
	ChainID  int    `json:"chainId"`
	MarketID string `json:"marketId"`
	Fee      string `json:"fee"`
}

// moolahFeeMarket is a fee market and whether it charges a fee at all.
type moolahFeeMarket struct {
	MarketID common.Hash
	Active   bool
}

// moolahFeeMarketStream reads every fee market of the chain, which the fee recipient holds a fee
// position in. The rows are not an account's, so a read that includes them is never proven empty.
func moolahFeeMarketStream(entity string) *indexStream[moolahFeeMarketRow, moolahFeeMarket] {
	return &indexStream[moolahFeeMarketRow, moolahFeeMarket]{
		Entity: entity, MaxRows: morphoMaxFeeMarketRows,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: fmt.Sprintf("%d:", chainID), Terms: []indexTerm{{"chainId", int(chainID)}}}
		},
		Decode: func(chainID ChainID, id string, row moolahFeeMarketRow) (moolahFeeMarket, error) {
			if row.ChainID != int(chainID) || !validHash(row.MarketID) {
				return moolahFeeMarket{}, fmt.Errorf("GraphQL returned malformed fee-market row %q", id)
			}
			marketID := common.HexToHash(row.MarketID)
			if id != fmt.Sprintf("%d:%s", chainID, strings.ToLower(marketID.Hex())) {
				return moolahFeeMarket{}, fmt.Errorf("GraphQL returned foreign fee-market row %q", id)
			}
			fee, valid := new(big.Int).SetString(row.Fee, 10)
			if !valid || fee.Sign() < 0 {
				return moolahFeeMarket{}, fmt.Errorf("GraphQL returned invalid fee for row %q", id)
			}
			return moolahFeeMarket{MarketID: marketID, Active: fee.Sign() > 0}, nil
		},
	}
}

// activeFeeMarkets are the fee markets that charge a fee.
func activeFeeMarkets(markets []moolahFeeMarket) []common.Hash {
	active := make([]common.Hash, 0, len(markets))
	for _, market := range markets {
		if market.Active {
			active = append(active, market.MarketID)
		}
	}
	return active
}

func validHash(value string) bool {
	return strings.HasPrefix(value, "0x") && len(value) == 66
}

type morphoVaultRow struct {
	ChainID int    `json:"chainId"`
	Account string `json:"account"`
	Vault   string `json:"vault"`
	Version string `json:"version"`
}

func morphoVaultStream(account common.Address) *indexStream[morphoVaultRow, morphoVaultRef] {
	return &indexStream[morphoVaultRow, morphoVaultRef]{
		Entity: "morphoVaultPositionRefs", Account: account, MaxRows: morphoMaxIndexedVaults,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{
				{"chainId", int(chainID)}, {"account", strings.ToLower(account.Hex())},
			}}
		},
		Decode: func(chainID ChainID, id string, row morphoVaultRow) (morphoVaultRef, error) {
			if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) || !common.IsHexAddress(row.Vault) {
				return morphoVaultRef{}, fmt.Errorf("GraphQL returned malformed vault row %q", id)
			}
			vault := common.HexToAddress(row.Vault)
			if id != indexAccountPrefix(chainID, account)+strings.ToLower(vault.Hex()) {
				return morphoVaultRef{}, fmt.Errorf("GraphQL returned foreign vault row %q", id)
			}
			version := morphoVaultVersion(row.Version)
			if version != morphoVaultV1 && version != morphoVaultV2 {
				return morphoVaultRef{}, fmt.Errorf("GraphQL returned invalid vault version %q", row.Version)
			}
			return morphoVaultRef{Address: vault, Version: version}, nil
		},
	}
}

// prefetchPresence asks every Morpho chain at once whether the account holds any market or vault
// position, so a scan queries only the chains where it does.
func (i *morphoIndexer) prefetchPresence(ctx context.Context, account common.Address) {
	i.index.prefetchPresence(ctx,
		moolahMarketStream("morphoMarketPositionRefs", account, morphoMaxIndexedMarkets), morphoVaultStream(account))
}

// dedupeMorphoRefs sorts the identifiers a read collected and drops the repeats: a fee market can
// also be one the account holds a position in.
func dedupeMorphoRefs(indexed morphoPositionRefs) (morphoPositionRefs, error) {
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
	vaultMap := make(map[common.Address]morphoVaultRef, len(indexed.Vaults))
	for _, vault := range indexed.Vaults {
		if existing, exists := vaultMap[vault.Address]; exists && existing.Version != vault.Version {
			return morphoPositionRefs{}, fmt.Errorf("index disagrees with itself on vault %s generation", vault.Address)
		}
		vaultMap[vault.Address] = vault
	}
	indexed.Vaults = indexed.Vaults[:0]
	for _, vault := range vaultMap {
		indexed.Vaults = append(indexed.Vaults, vault)
	}
	sort.Slice(indexed.Vaults, func(i, j int) bool {
		return strings.ToLower(indexed.Vaults[i].Address.Hex()) < strings.ToLower(indexed.Vaults[j].Address.Hex())
	})
	if len(indexed.MarketIDs) > morphoMaxIndexedMarkets || len(indexed.Vaults) > morphoMaxIndexedVaults {
		return morphoPositionRefs{}, fmt.Errorf("Morpho position identifiers exceed account bounds")
	}
	return indexed, nil
}

func (i *morphoIndexer) PositionRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment morphoDeployment,
) (morphoPositionRefs, error) {
	feeRecipientValues, err := client.Call(
		ctx,
		block,
		deployment.Morpho,
		morphoCoreABI,
		"feeRecipient",
	)
	if err != nil {
		return morphoPositionRefs{}, fmt.Errorf("Morpho fee recipient at pinned block: %w", err)
	}
	feeRecipient, err := AddressAt(feeRecipientValues, 0)
	if err != nil {
		return morphoPositionRefs{}, fmt.Errorf("Morpho fee recipient at pinned block: %w", err)
	}
	return i.refs(ctx, client, block, account, feeRecipient != (common.Address{}) && feeRecipient == account)
}

// refs reads the markets and vaults account holds a position in on block's chain, and the chain's
// fee markets when the account is their fee recipient.
func (i *morphoIndexer) refs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	feeRecipient bool,
) (morphoPositionRefs, error) {
	markets := moolahMarketStream("morphoMarketPositionRefs", account, morphoMaxIndexedMarkets)
	vaults := morphoVaultStream(account)
	streams := []indexStreamer{markets, vaults}
	// The fee recipient holds a position in every market charging a fee.
	feeMarkets := moolahFeeMarketStream("morphoFeeMarkets")
	if feeRecipient {
		streams = append(streams, feeMarkets)
	}
	at, err := i.index.read(ctx, client, block, streams...)
	if err != nil {
		return morphoPositionRefs{}, fmt.Errorf("Morpho index query: %w", err)
	}
	return dedupeMorphoRefs(morphoPositionRefs{
		IndexerBlock: at.QueryBlock,
		MarketIDs:    append(markets.Rows(), activeFeeMarkets(feeMarkets.Rows())...),
		Vaults:       vaults.Rows(),
	})
}
