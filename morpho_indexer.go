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
	morphoIndexerPageSize   = 500
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
	// Lag notes a live read that trails its pin by more than indexMaxLag.
	Lag *indexLagNote
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
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newMorphoIndexer(config SentioIndexerConfig) *morphoIndexer {
	return &morphoIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: deploymentChains(morphoDeployments),
	}
}

const morphoWalletQuery = `
query MorphoWalletPositions(
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
  morphoMarketPositionRefs(
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
  morphoVaultPositionRefs(
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
    version
  }
}`

type morphoGraphQLResponse struct {
	Data struct {
		Checkpoints []indexerCheckpointRow `json:"indexerCheckpoints"`
		Markets     []struct {
			ID       string `json:"id"`
			ChainID  int    `json:"chainId"`
			Account  string `json:"account"`
			MarketID string `json:"marketId"`
		} `json:"morphoMarketPositionRefs"`
		Vaults []struct {
			ID      string `json:"id"`
			ChainID int    `json:"chainId"`
			Account string `json:"account"`
			Vault   string `json:"vault"`
			Version string `json:"version"`
		} `json:"morphoVaultPositionRefs"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type morphoGraphQLPage struct {
	CheckpointBlock uint64
	CheckpointMS    uint64
	MarketIDs       []common.Hash
	MarketRowIDs    []string
	Vaults          []morphoVaultRef
	VaultRowIDs     []string
}

const morphoFeeMarketsQuery = `
query MorphoFeeMarkets(
  $chainId: Int!
  $after: ID!
  $first: Int!
  $block: BigInt!
) {
  morphoFeeMarkets(
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

type morphoFeeMarketsGraphQLResponse struct {
	Data struct {
		Markets []struct {
			ID       string `json:"id"`
			ChainID  int    `json:"chainId"`
			MarketID string `json:"marketId"`
			Fee      string `json:"fee"`
		} `json:"morphoFeeMarkets"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type morphoFeeMarketPage struct {
	RowIDs          []string
	ActiveMarketIDs []common.Hash
}

func morphoFeeMarketRowPrefix(chainID ChainID) string {
	return fmt.Sprintf("%d:", chainID)
}

func (i *morphoIndexer) graphqlFeeMarketPage(
	ctx context.Context,
	chainID ChainID,
	after string,
	block uint64,
) (morphoFeeMarketPage, error) {
	var payload morphoFeeMarketsGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": morphoFeeMarketsQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "after": after,
				"first": morphoIndexerPageSize, "block": strconv.FormatUint(block, 10),
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

func morphoRowPrefix(chainID ChainID, account common.Address) string {
	return fmt.Sprintf("%d:%s:", chainID, strings.ToLower(account.Hex()))
}

func (i *morphoIndexer) graphqlPage(
	ctx context.Context,
	chainID ChainID,
	account common.Address,
	marketAfter string,
	vaultAfter string,
	block uint64,
	start indexStart,
) (morphoGraphQLPage, error) {
	prefix := morphoRowPrefix(chainID, account)
	var payload morphoGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": morphoWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "account": strings.ToLower(account.Hex()), "idPrefix": prefix,
				"marketAfter": marketAfter, "vaultAfter": vaultAfter,
				"checkpoint": strconv.FormatUint(uint64(chainID), 10),
				"first":      morphoIndexerPageSize, "block": strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return morphoGraphQLPage{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, graphQLError := range payload.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return morphoGraphQLPage{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	checkpointBlock, checkpointMS, err := start.checkpoint(ctx, payload.Data.Checkpoints)
	if err != nil {
		return morphoGraphQLPage{}, err
	}
	page := morphoGraphQLPage{CheckpointBlock: checkpointBlock, CheckpointMS: checkpointMS}
	for _, row := range payload.Data.Markets {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
			!strings.HasPrefix(row.ID, prefix) || row.ID <= marketAfter ||
			!strings.HasPrefix(row.MarketID, "0x") || len(row.MarketID) != 66 {
			return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned malformed market row %q", row.ID)
		}
		marketID := common.HexToHash(row.MarketID)
		if row.ID != prefix+strings.ToLower(marketID.Hex()) {
			return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned foreign market row %q", row.ID)
		}
		page.MarketIDs = append(page.MarketIDs, marketID)
		page.MarketRowIDs = append(page.MarketRowIDs, row.ID)
	}
	for _, row := range payload.Data.Vaults {
		if row.ChainID != int(chainID) || !strings.EqualFold(row.Account, account.Hex()) ||
			!strings.HasPrefix(row.ID, prefix) || row.ID <= vaultAfter || !common.IsHexAddress(row.Vault) {
			return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned malformed vault row %q", row.ID)
		}
		vault := common.HexToAddress(row.Vault)
		if row.ID != prefix+strings.ToLower(vault.Hex()) {
			return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned foreign vault row %q", row.ID)
		}
		version := morphoVaultVersion(row.Version)
		if version != morphoVaultV1 && version != morphoVaultV2 {
			return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned invalid vault version %q", row.Version)
		}
		page.Vaults = append(page.Vaults, morphoVaultRef{Address: vault, Version: version})
		page.VaultRowIDs = append(page.VaultRowIDs, row.ID)
	}
	return page, nil
}

// presenceProbe asks every Morpho chain at once whether the account holds any market or vault
// position, so a scan queries only the chains where it does.
func (i *morphoIndexer) presenceProbe(account common.Address) presenceProbe {
	owner := strings.ToLower(account.Hex())
	where := func(chainID ChainID) string {
		return fmt.Sprintf("chainId: %d, account: \"%s\", id_starts_with: \"%s\"", chainID, owner, morphoRowPrefix(chainID, account))
	}
	return presenceProbe{
		name:           "morpho-blue",
		config:         i.config,
		requiredChains: i.requiredChains,
		fields: []presenceField{
			{entity: "morphoMarketPositionRefs", where: where},
			{entity: "morphoVaultPositionRefs", where: where},
		},
	}
}

func (i *morphoIndexer) prefetchPresence(ctx context.Context, account common.Address) {
	i.api.prefetchPresence(ctx, i.presenceProbe(account), strings.ToLower(account.Hex()))
}

func (i *morphoIndexer) indexedRefs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
	includeFeeMarkets bool,
	start indexStart,
) (morphoPositionRefs, error) {
	statuses, queryBlock, err := indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return i.api.chainStatusesForScan(ctx, i.config, i.requiredChains, block.ChainID, forceRefresh)
	})
	if err != nil {
		return morphoPositionRefs{}, err
	}
	result := morphoPositionRefs{IndexerBlock: queryBlock}
	// The fee-market surface is keyed by chain, not account, so only an account-only query can be
	// answered by the probe.
	if !includeFeeMarkets {
		proof, empty := i.api.chainProvenEmpty(ctx, i.presenceProbe(account), block, strings.ToLower(account.Hex()), statuses)
		if empty {
			// The proof may predate this status read; it is the block the answer holds at.
			result.IndexerBlock = proof.QueryBlock
			return dedupeMorphoRefs(result)
		}
	}
	// Only the pages take a lane slot: the status read needs none, and a chain the probe proved
	// empty sends nothing.
	release, err := lockSentioLane(ctx)
	if err != nil {
		return morphoPositionRefs{}, err
	}
	defer release()
	prefix := morphoRowPrefix(block.ChainID, account)
	marketAfter := prefix
	vaultAfter := prefix
	marketDone := false
	vaultDone := false
	var checkpointBlock uint64
	var checkpointMS uint64
	firstPage := true
	for !marketDone || !vaultDone {
		page, pageErr := i.graphqlPage(
			ctx, block.ChainID, account, marketAfter, vaultAfter, queryBlock, start,
		)
		if pageErr != nil {
			return morphoPositionRefs{}, pageErr
		}
		if firstPage {
			if result.Lag, err = checkIndexCheckpoint(block, page.CheckpointBlock, page.CheckpointMS); err != nil {
				return morphoPositionRefs{}, err
			}
			checkpointBlock, checkpointMS = page.CheckpointBlock, page.CheckpointMS
			firstPage = false
		} else if page.CheckpointBlock != checkpointBlock || page.CheckpointMS != checkpointMS {
			return morphoPositionRefs{}, fmt.Errorf("GraphQL pagination changed checkpoint")
		}
		if !marketDone {
			for index, rowID := range page.MarketRowIDs {
				if rowID <= marketAfter {
					return morphoPositionRefs{}, fmt.Errorf("market rows are not ordered")
				}
				marketAfter = rowID
				result.MarketIDs = append(result.MarketIDs, page.MarketIDs[index])
			}
			marketDone = len(page.MarketRowIDs) < morphoIndexerPageSize
		}
		if !vaultDone {
			for index, rowID := range page.VaultRowIDs {
				if rowID <= vaultAfter {
					return morphoPositionRefs{}, fmt.Errorf("vault rows are not ordered")
				}
				vaultAfter = rowID
				result.Vaults = append(result.Vaults, page.Vaults[index])
			}
			vaultDone = len(page.VaultRowIDs) < morphoIndexerPageSize
		}
		if len(result.MarketIDs) > morphoMaxIndexedMarkets {
			return morphoPositionRefs{}, fmt.Errorf("account has more than %d indexed Morpho markets", morphoMaxIndexedMarkets)
		}
		if len(result.Vaults) > morphoMaxIndexedVaults {
			return morphoPositionRefs{}, fmt.Errorf("account has more than %d indexed Morpho vaults", morphoMaxIndexedVaults)
		}
	}
	if includeFeeMarkets {
		feeAfter := morphoFeeMarketRowPrefix(block.ChainID)
		feeRows := 0
		for {
			page, pageErr := i.graphqlFeeMarketPage(ctx, block.ChainID, feeAfter, queryBlock)
			if pageErr != nil {
				return morphoPositionRefs{}, pageErr
			}
			for _, rowID := range page.RowIDs {
				if rowID <= feeAfter {
					return morphoPositionRefs{}, fmt.Errorf("fee-market rows are not ordered")
				}
				feeAfter = rowID
			}
			feeRows += len(page.RowIDs)
			if feeRows > morphoMaxFeeMarketRows {
				return morphoPositionRefs{}, fmt.Errorf(
					"chain has more than %d indexed Morpho fee markets",
					morphoMaxFeeMarketRows,
				)
			}
			result.MarketIDs = append(result.MarketIDs, page.ActiveMarketIDs...)
			if len(page.RowIDs) < morphoIndexerPageSize {
				break
			}
		}
	}
	return dedupeMorphoRefs(result)
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
	includeCurrentFeeMarkets := feeRecipient != (common.Address{}) && feeRecipient == account
	if !includeCurrentFeeMarkets {
		i.api.awaitPresence(ctx, i.presenceProbe(account), strings.ToLower(account.Hex()))
	}
	indexed, err := i.indexedRefs(
		ctx, block, account, includeCurrentFeeMarkets,
		indexStart{client: client, block: deployment.Window.ActivationBlock},
	)
	if err != nil {
		return morphoPositionRefs{}, fmt.Errorf("Morpho index query: %w", err)
	}
	return indexed, nil
}
