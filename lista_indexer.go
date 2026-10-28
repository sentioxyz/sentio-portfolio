package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	listaIndexerPageSize   = 500
	listaMaxIndexedMarkets = 4_096
	listaMaxIndexedVaults  = 4_096
)

type listaPositionRefs struct {
	IndexerBlock uint64
	MarketIDs    []common.Hash
	Vaults       []common.Address
	// Lag notes a live read that trails its pin by more than indexMaxLag.
	Lag *indexLagNote
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

type listaIndexer struct {
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newListaIndexer(config SentioIndexerConfig) *listaIndexer {
	return &listaIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: []ChainID{Ethereum, BSC},
	}
}

const listaWalletQuery = `
query ListaWalletPositions(
  $chainId: Int!
  $account: String!
  $idPrefix: String!
  $marketAfter: ID!
  $vaultAfter: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  listaMarketPositionRefs(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, account: $account, id_starts_with: $idPrefix, id_gt: $marketAfter }
  ) {
    id
    chainId
    account
    marketId
  }
  listaVaultPositionRefs(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, account: $account, id_starts_with: $idPrefix, id_gt: $vaultAfter }
  ) {
    id
    chainId
    account
    vault
  }
}`

type listaGraphQLResponse struct {
	Data struct {
		Checkpoints []indexerCheckpointRow `json:"indexerCheckpoints"`
		Markets     []struct {
			ID       string `json:"id"`
			ChainID  int    `json:"chainId"`
			Account  string `json:"account"`
			MarketID string `json:"marketId"`
		} `json:"listaMarketPositionRefs"`
		Vaults []struct {
			ID      string `json:"id"`
			ChainID int    `json:"chainId"`
			Account string `json:"account"`
			Vault   string `json:"vault"`
		} `json:"listaVaultPositionRefs"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type listaGraphQLPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	MarketIDs       []common.Hash
	MarketRowIDs    []string
	Vaults          []common.Address
	VaultRowIDs     []string
}

func listaRowPrefix(chainID ChainID, account common.Address) string {
	return fmt.Sprintf("%d:%s:", chainID, strings.ToLower(account.Hex()))
}

func (i *listaIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	account common.Address,
	marketAfter string,
	vaultAfter string,
	block uint64,
	start indexStart,
) (listaGraphQLPage, error) {
	prefix := listaRowPrefix(chainID, account)
	var payload listaGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": listaWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "account": strings.ToLower(account.Hex()), "idPrefix": prefix,
				"marketAfter": marketAfter, "vaultAfter": vaultAfter,
				"checkpoint": strconv.FormatUint(uint64(chainID), 10),
				"first":      listaIndexerPageSize, "block": strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return listaGraphQLPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return listaGraphQLPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	checkpointBlock, checkpointMS, err := start.checkpoint(ctx, payload.Data.Checkpoints)
	if err != nil {
		return listaGraphQLPage{}, err
	}
	page := listaGraphQLPage{CheckpointBlock: checkpointBlock, CheckpointMS: checkpointMS}
	for _, row := range payload.Data.Markets {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
			!strings.HasPrefix(row.ID, prefix) || row.ID <= marketAfter ||
			!strings.HasPrefix(row.MarketID, "0x") || len(row.MarketID) != 66 {
			return listaGraphQLPage{}, fmt.Errorf("GraphQL returned malformed market row %q", row.ID)
		}
		marketID := common.HexToHash(row.MarketID)
		if row.ID != prefix+strings.ToLower(marketID.Hex()) {
			return listaGraphQLPage{}, fmt.Errorf("GraphQL returned foreign market row %q", row.ID)
		}
		page.MarketIDs = append(page.MarketIDs, marketID)
		page.MarketRowIDs = append(page.MarketRowIDs, row.ID)
	}
	for _, row := range payload.Data.Vaults {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
			!strings.HasPrefix(row.ID, prefix) || row.ID <= vaultAfter || !common.IsHexAddress(row.Vault) {
			return listaGraphQLPage{}, fmt.Errorf("GraphQL returned malformed vault row %q", row.ID)
		}
		vault := common.HexToAddress(row.Vault)
		if row.ID != prefix+strings.ToLower(vault.Hex()) {
			return listaGraphQLPage{}, fmt.Errorf("GraphQL returned foreign vault row %q", row.ID)
		}
		page.Vaults = append(page.Vaults, vault)
		page.VaultRowIDs = append(page.VaultRowIDs, row.ID)
	}
	return page, nil
}

const listaFeeMarketsQuery = `
query ListaFeeMarkets(
  $chainId: Int!
  $after: ID!
  $first: Int!
  $block: BigInt!
) {
  listaFeeMarkets(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { chainId: $chainId, id_gt: $after }
  ) {
    id
    chainId
    marketId
    fee
  }
}`

type listaFeeMarketsGraphQLResponse struct {
	Data struct {
		Markets []struct {
			ID       string `json:"id"`
			ChainID  int    `json:"chainId"`
			MarketID string `json:"marketId"`
			Fee      string `json:"fee"`
		} `json:"listaFeeMarkets"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (i *listaIndexer) graphqlFeeMarketPage(
	ctx context.Context,
	chainID ChainID,
	after string,
	block uint64,
) (morphoFeeMarketPage, error) {
	var payload listaFeeMarketsGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": listaFeeMarketsQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "after": after,
				"first": listaIndexerPageSize, "block": strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return morphoFeeMarketPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return morphoFeeMarketPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	prefix := morphoFeeMarketRowPrefix(chainID)
	page := morphoFeeMarketPage{RowIDs: make([]string, 0, len(payload.Data.Markets))}
	for _, row := range payload.Data.Markets {
		if row.ChainID != int(chainID) || !strings.HasPrefix(row.ID, prefix) || row.ID <= after ||
			!strings.HasPrefix(row.MarketID, "0x") || len(row.MarketID) != 66 {
			return morphoFeeMarketPage{}, fmt.Errorf("GraphQL returned malformed fee-market row %q", row.ID)
		}
		marketID := common.HexToHash(row.MarketID)
		if row.ID != prefix+strings.ToLower(marketID.Hex()) {
			return morphoFeeMarketPage{}, fmt.Errorf("GraphQL returned foreign fee-market row %q", row.ID)
		}
		fee, valid := new(big.Int).SetString(row.Fee, 10)
		if !valid || fee.Sign() < 0 {
			return morphoFeeMarketPage{}, fmt.Errorf("GraphQL returned invalid fee for row %q", row.ID)
		}
		page.RowIDs = append(page.RowIDs, row.ID)
		if fee.Sign() > 0 {
			page.ActiveMarketIDs = append(page.ActiveMarketIDs, marketID)
		}
	}
	return page, nil
}

func (i *listaIndexer) indexedRefs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
	includeFeeMarkets bool,
	start indexStart,
) (listaPositionRefs, error) {
	_, queryBlock, err := indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, forceRefresh)
	})
	if err != nil {
		return listaPositionRefs{}, err
	}
	prefix := listaRowPrefix(block.ChainID, account)
	marketAfter := prefix
	vaultAfter := prefix
	marketDone := false
	vaultDone := false
	result := listaPositionRefs{IndexerBlock: queryBlock}
	var checkpointBlock uint64
	var checkpointMS uint64
	firstPage := true
	for !marketDone || !vaultDone {
		page, pageErr := i.graphqlPage(
			ctx, block.ChainID, account, marketAfter, vaultAfter, queryBlock, start,
		)
		if pageErr != nil {
			return listaPositionRefs{}, pageErr
		}
		if firstPage {
			if result.Lag, err = checkIndexCheckpoint(block, page.CheckpointBlock, page.CheckpointMS); err != nil {
				return listaPositionRefs{}, err
			}
			checkpointBlock, checkpointMS = page.CheckpointBlock, page.CheckpointMS
			firstPage = false
		} else if page.CheckpointBlock != checkpointBlock || page.CheckpointMS != checkpointMS {
			return listaPositionRefs{}, fmt.Errorf("GraphQL pagination changed checkpoint")
		}
		if !marketDone {
			for index, rowID := range page.MarketRowIDs {
				if rowID <= marketAfter {
					return listaPositionRefs{}, fmt.Errorf("market rows are not ordered")
				}
				marketAfter = rowID
				result.MarketIDs = append(result.MarketIDs, page.MarketIDs[index])
			}
			marketDone = len(page.MarketRowIDs) < listaIndexerPageSize
		}
		if !vaultDone {
			for index, rowID := range page.VaultRowIDs {
				if rowID <= vaultAfter {
					return listaPositionRefs{}, fmt.Errorf("vault rows are not ordered")
				}
				vaultAfter = rowID
				result.Vaults = append(result.Vaults, page.Vaults[index])
			}
			vaultDone = len(page.VaultRowIDs) < listaIndexerPageSize
		}
		if len(result.MarketIDs) > listaMaxIndexedMarkets {
			return listaPositionRefs{}, fmt.Errorf("account has more than %d indexed Lista markets", listaMaxIndexedMarkets)
		}
		if len(result.Vaults) > listaMaxIndexedVaults {
			return listaPositionRefs{}, fmt.Errorf("account has more than %d indexed Lista vaults", listaMaxIndexedVaults)
		}
	}
	if includeFeeMarkets {
		feeAfter := morphoFeeMarketRowPrefix(block.ChainID)
		feeRows := 0
		for {
			page, pageErr := i.graphqlFeeMarketPage(ctx, block.ChainID, feeAfter, queryBlock)
			if pageErr != nil {
				return listaPositionRefs{}, pageErr
			}
			for _, rowID := range page.RowIDs {
				if rowID <= feeAfter {
					return listaPositionRefs{}, fmt.Errorf("fee-market rows are not ordered")
				}
				feeAfter = rowID
			}
			feeRows += len(page.RowIDs)
			if feeRows > morphoMaxFeeMarketRows {
				return listaPositionRefs{}, fmt.Errorf(
					"chain has more than %d indexed Lista fee markets", morphoMaxFeeMarketRows,
				)
			}
			result.MarketIDs = append(result.MarketIDs, page.ActiveMarketIDs...)
			if len(page.RowIDs) < listaIndexerPageSize {
				break
			}
		}
	}
	return dedupeListaRefs(result)
}

// dedupeListaRefs sorts the identifiers a read collected and drops the repeats: a fee market can
// also be one the account holds a position in.
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
	includeCurrentFeeMarkets := feeRecipient != (common.Address{}) && feeRecipient == account
	indexed, err := func() (listaPositionRefs, error) {
		release, err := lockSentioLane(ctx)
		if err != nil {
			return listaPositionRefs{}, err
		}
		defer release()
		return i.indexedRefs(
			ctx, block, account, includeCurrentFeeMarkets,
			indexStart{client: client, block: deployment.ActivationBlock},
		)
	}()
	if err != nil {
		return listaPositionRefs{}, fmt.Errorf("Lista index query: %w", err)
	}
	return indexed, nil
}
