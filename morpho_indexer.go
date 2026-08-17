package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	morphoIndexerPageSize   = 500
	morphoMaxIndexedMarkets = 4_096
	morphoMaxFeeMarketRows  = 8_192
	morphoMaxIndexedVaults  = 4_096
	morphoMaxRPCTailBlocks  = 2_048
	morphoMaxTailContracts  = 4_096
	morphoCheckpointMaxLag  = 15 * time.Minute
	morphoBackfillMaxLag    = 7*24*time.Hour + 30*time.Minute
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
	api            *sentioAPIClient
	config         SentioIndexerConfig
	requiredChains []ChainID
}

func newMorphoIndexer(config SentioIndexerConfig) *morphoIndexer {
	return &morphoIndexer{
		api: newSentioAPIClient(), config: config,
		requiredChains: append([]ChainID(nil), SupportedChainIDs...),
	}
}

const morphoWalletQuery = `
query MorphoWalletPositions(
  $chainId: Int!
  $account: String!
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
    where: { chainId: $chainId, account: $account, id_gt: $marketAfter }
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
    where: { chainId: $chainId, account: $account, id_gt: $vaultAfter }
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
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Markets []struct {
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
) (morphoGraphQLPage, error) {
	var payload morphoGraphQLResponse
	err := i.api.doJSON(
		ctx,
		http.MethodPost,
		i.config.GraphQLURL,
		map[string]any{
			"query": morphoWalletQuery,
			"variables": map[string]any{
				"chainId": int(chainID), "account": strings.ToLower(account.Hex()),
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
	if len(payload.Data.Checkpoints) != 1 {
		return morphoGraphQLPage{}, fmt.Errorf("GraphQL returned %d checkpoints", len(payload.Data.Checkpoints))
	}
	checkpointBlock, err := strconv.ParseUint(payload.Data.Checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return morphoGraphQLPage{}, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	checkpointMS, err := strconv.ParseUint(payload.Data.Checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return morphoGraphQLPage{}, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	prefix := morphoRowPrefix(chainID, account)
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

func validateMorphoCheckpoint(block BlockRef, page morphoGraphQLPage) error {
	if page.CheckpointBlock > block.Number {
		return fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", page.CheckpointBlock, block.Number)
	}
	checkpointSeconds := page.CheckpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp > checkpointSeconds {
		lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
		maximumLag := morphoCheckpointMaxLag
		if block.Fixed {
			maximumLag = morphoBackfillMaxLag
		}
		if lag > maximumLag {
			return fmt.Errorf("indexer checkpoint is stale by %s", lag)
		}
	}
	return nil
}

func (i *morphoIndexer) indexedRefs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
	includeFeeMarkets bool,
) (morphoPositionRefs, error) {
	statuses, err := i.api.chainStatuses(ctx, i.config, i.requiredChains, false)
	if err != nil {
		return morphoPositionRefs{}, err
	}
	status := statuses[block.ChainID]
	if status.State != "PROCESSING_LATEST" {
		return morphoPositionRefs{}, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID, status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	if block.Number > status.ProcessedBlock && block.Number-status.ProcessedBlock > morphoMaxRPCTailBlocks {
		statuses, err = i.api.chainStatuses(ctx, i.config, i.requiredChains, true)
		if err != nil {
			return morphoPositionRefs{}, err
		}
		status = statuses[block.ChainID]
	}
	if status.State != "PROCESSING_LATEST" ||
		(block.Number > status.ProcessedBlock && block.Number-status.ProcessedBlock > morphoMaxRPCTailBlocks) {
		return morphoPositionRefs{}, fmt.Errorf(
			"indexer RPC tail exceeds %d blocks for chain %d: processed=%d target=%d",
			morphoMaxRPCTailBlocks, block.ChainID, status.ProcessedBlock, block.Number,
		)
	}
	queryBlock := min(block.Number, status.ProcessedBlock)
	prefix := morphoRowPrefix(block.ChainID, account)
	marketAfter := prefix
	vaultAfter := prefix
	marketDone := false
	vaultDone := false
	result := morphoPositionRefs{IndexerBlock: queryBlock}
	var checkpointBlock uint64
	var checkpointMS uint64
	firstPage := true
	for !marketDone || !vaultDone {
		page, pageErr := i.graphqlPage(
			ctx, block.ChainID, account, marketAfter, vaultAfter, queryBlock,
		)
		if pageErr != nil {
			return morphoPositionRefs{}, pageErr
		}
		if firstPage {
			if err := validateMorphoCheckpoint(block, page); err != nil {
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
	return mergeMorphoRefs(result, nil, nil)
}

func morphoAdaptiveLogs(
	ctx context.Context,
	client *RPCClient,
	fromBlock uint64,
	toBlock uint64,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]rpcLog, error) {
	logs, err := client.Logs(ctx, fromBlock, toBlock, addresses, topics)
	if err == nil {
		return logs, nil
	}
	if fromBlock == toBlock {
		return nil, err
	}
	middle := fromBlock + (toBlock-fromBlock)/2
	left, leftErr := morphoAdaptiveLogs(ctx, client, fromBlock, middle, addresses, topics)
	if leftErr != nil {
		return nil, leftErr
	}
	right, rightErr := morphoAdaptiveLogs(ctx, client, middle+1, toBlock, addresses, topics)
	if rightErr != nil {
		return nil, rightErr
	}
	return append(left, right...), nil
}

var (
	morphoTopic2AccountEvents = []common.Hash{
		crypto.Keccak256Hash([]byte("Withdraw(bytes32,address,address,address,uint256,uint256)")),
		crypto.Keccak256Hash([]byte("Borrow(bytes32,address,address,address,uint256,uint256)")),
		crypto.Keccak256Hash([]byte("WithdrawCollateral(bytes32,address,address,address,uint256)")),
	}
	morphoTopic3AccountEvents = []common.Hash{
		crypto.Keccak256Hash([]byte("Supply(bytes32,address,address,uint256,uint256)")),
		crypto.Keccak256Hash([]byte("Repay(bytes32,address,address,uint256,uint256)")),
		crypto.Keccak256Hash([]byte("SupplyCollateral(bytes32,address,address,uint256)")),
		crypto.Keccak256Hash([]byte("Liquidate(bytes32,address,address,uint256,uint256,uint256,uint256,uint256)")),
	}
	morphoTransferTopic       = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	morphoAccrueInterestTopic = crypto.Keccak256Hash(
		[]byte("AccrueInterest(bytes32,uint256,uint256,uint256)"),
	)
	morphoSetFeeRecipientTopic = crypto.Keccak256Hash([]byte("SetFeeRecipient(address)"))
	morphoSetFeeTopic          = crypto.Keccak256Hash([]byte("SetFee(bytes32,uint256)"))
)

func morphoAccountTopic(account common.Address) common.Hash {
	return common.BytesToHash(account.Bytes())
}

func morphoTailMarketIDs(
	ctx context.Context,
	client *RPCClient,
	fromBlock uint64,
	toBlock uint64,
	account common.Address,
	core common.Address,
	includeCurrentFeeMarkets bool,
) ([]common.Hash, error) {
	if fromBlock > toBlock {
		return nil, nil
	}
	accountTopic := morphoAccountTopic(account)
	filters := [][][]common.Hash{
		{morphoTopic2AccountEvents, nil, {accountTopic}},
		{morphoTopic3AccountEvents, nil, nil, {accountTopic}},
	}
	seen := make(map[common.Hash]struct{})
	for _, topics := range filters {
		logs, err := morphoAdaptiveLogs(ctx, client, fromBlock, toBlock, []common.Address{core}, topics)
		if err != nil {
			return nil, fmt.Errorf("core identifier RPC tail: %w", err)
		}
		for _, log := range logs {
			if len(log.Topics) < 2 {
				return nil, fmt.Errorf("core RPC tail returned a malformed log")
			}
			seen[log.Topics[1]] = struct{}{}
		}
	}
	feeMarkets, err := morphoTailFeeMarketIDs(
		ctx, client, fromBlock, toBlock, account, core, includeCurrentFeeMarkets,
	)
	if err != nil {
		return nil, err
	}
	for _, marketID := range feeMarkets {
		seen[marketID] = struct{}{}
	}
	result := make([]common.Hash, 0, len(seen))
	for marketID := range seen {
		result = append(result, marketID)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Hex() < result[j].Hex() })
	return result, nil
}

func morphoPositiveSetFeeMarketIDsFromLogs(logs []rpcLog) ([]common.Hash, error) {
	seen := make(map[common.Hash]struct{})
	for _, log := range logs {
		if len(log.Topics) != 2 || log.Topics[0] != morphoSetFeeTopic || len(log.Data) != 32 {
			return nil, fmt.Errorf("fee-market RPC tail returned a malformed SetFee log")
		}
		if new(big.Int).SetBytes(log.Data).Sign() > 0 {
			seen[log.Topics[1]] = struct{}{}
		}
	}
	result := make([]common.Hash, 0, len(seen))
	for marketID := range seen {
		result = append(result, marketID)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Hex() < result[right].Hex() })
	return result, nil
}

func morphoFeeMarketIDsFromLogs(
	initialRecipient common.Address,
	account common.Address,
	logs []rpcLog,
) ([]common.Hash, error) {
	sort.Slice(logs, func(left, right int) bool {
		if logs[left].BlockNumber != logs[right].BlockNumber {
			return logs[left].BlockNumber < logs[right].BlockNumber
		}
		return logs[left].LogIndex < logs[right].LogIndex
	})
	recipient := initialRecipient
	seen := make(map[common.Hash]struct{})
	for _, log := range logs {
		if len(log.Topics) == 0 {
			return nil, fmt.Errorf("fee-recipient RPC tail returned a log without topics")
		}
		switch log.Topics[0] {
		case morphoSetFeeRecipientTopic:
			if len(log.Topics) != 2 {
				return nil, fmt.Errorf("fee-recipient RPC tail returned a malformed SetFeeRecipient log")
			}
			recipient = common.BytesToAddress(log.Topics[1].Bytes())
		case morphoAccrueInterestTopic:
			if len(log.Topics) != 2 || len(log.Data) != 96 {
				return nil, fmt.Errorf("fee-recipient RPC tail returned a malformed AccrueInterest log")
			}
			feeShares := new(big.Int).SetBytes(log.Data[64:96])
			if recipient == account && feeShares.Sign() > 0 {
				seen[log.Topics[1]] = struct{}{}
			}
		default:
			return nil, fmt.Errorf("fee-recipient RPC tail returned an unexpected event")
		}
	}
	result := make([]common.Hash, 0, len(seen))
	for marketID := range seen {
		result = append(result, marketID)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Hex() < result[right].Hex() })
	return result, nil
}

func morphoTailFeeMarketIDs(
	ctx context.Context,
	client *RPCClient,
	fromBlock uint64,
	toBlock uint64,
	account common.Address,
	core common.Address,
	includeCurrentFeeMarkets bool,
) ([]common.Hash, error) {
	if fromBlock == 0 || fromBlock > toBlock {
		return nil, nil
	}
	values, err := client.Call(
		ctx,
		BlockRef{ChainID: client.chainID, Number: fromBlock - 1, Fixed: true},
		core,
		morphoCoreABI,
		"feeRecipient",
	)
	if err != nil {
		return nil, fmt.Errorf("fee recipient before RPC tail: %w", err)
	}
	initialRecipient, err := AddressAt(values, 0)
	if err != nil {
		return nil, fmt.Errorf("fee recipient before RPC tail: %w", err)
	}
	setLogs, err := morphoAdaptiveLogs(
		ctx,
		client,
		fromBlock,
		toBlock,
		[]common.Address{core},
		[][]common.Hash{{morphoSetFeeRecipientTopic}},
	)
	if err != nil {
		return nil, fmt.Errorf("fee-recipient changes RPC tail: %w", err)
	}
	relevantRecipient := initialRecipient == account
	for _, log := range setLogs {
		if len(log.Topics) != 2 {
			return nil, fmt.Errorf("fee-recipient RPC tail returned a malformed SetFeeRecipient log")
		}
		if common.BytesToAddress(log.Topics[1].Bytes()) == account {
			relevantRecipient = true
		}
	}
	if !relevantRecipient {
		return nil, nil
	}
	accrueLogs, err := morphoAdaptiveLogs(
		ctx,
		client,
		fromBlock,
		toBlock,
		[]common.Address{core},
		[][]common.Hash{{morphoAccrueInterestTopic}},
	)
	if err != nil {
		return nil, fmt.Errorf("fee accrual identifier RPC tail: %w", err)
	}
	result, err := morphoFeeMarketIDsFromLogs(initialRecipient, account, append(setLogs, accrueLogs...))
	if err != nil {
		return nil, err
	}
	if !includeCurrentFeeMarkets {
		return result, nil
	}
	feeLogs, err := morphoAdaptiveLogs(
		ctx,
		client,
		fromBlock,
		toBlock,
		[]common.Address{core},
		[][]common.Hash{{morphoSetFeeTopic}},
	)
	if err != nil {
		return nil, fmt.Errorf("fee-market changes RPC tail: %w", err)
	}
	positiveFeeMarkets, err := morphoPositiveSetFeeMarketIDsFromLogs(feeLogs)
	if err != nil {
		return nil, err
	}
	seen := make(map[common.Hash]struct{}, len(result)+len(positiveFeeMarkets))
	for _, marketID := range append(result, positiveFeeMarkets...) {
		seen[marketID] = struct{}{}
	}
	result = result[:0]
	for marketID := range seen {
		result = append(result, marketID)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Hex() < result[right].Hex() })
	return result, nil
}

func morphoTailVaultCandidates(
	ctx context.Context,
	client *RPCClient,
	fromBlock uint64,
	toBlock uint64,
	account common.Address,
) ([]common.Address, error) {
	if fromBlock > toBlock {
		return nil, nil
	}
	accountTopic := morphoAccountTopic(account)
	filters := [][][]common.Hash{
		{{morphoTransferTopic}, {accountTopic}},
		{{morphoTransferTopic}, nil, {accountTopic}},
	}
	seen := make(map[common.Address]struct{})
	for _, topics := range filters {
		logs, err := morphoAdaptiveLogs(ctx, client, fromBlock, toBlock, nil, topics)
		if err != nil {
			return nil, fmt.Errorf("vault transfer RPC tail: %w", err)
		}
		for _, log := range logs {
			seen[log.Address] = struct{}{}
			if len(seen) > morphoMaxTailContracts {
				return nil, fmt.Errorf("RPC tail contains more than %d transfer contracts", morphoMaxTailContracts)
			}
		}
	}
	result := make([]common.Address, 0, len(seen))
	for address := range seen {
		result = append(result, address)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Hex()) < strings.ToLower(result[j].Hex())
	})
	return result, nil
}

func morphoClassifyTailVaults(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	candidates []common.Address,
	deployment morphoDeployment,
) ([]morphoVaultRef, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	factories := make([]morphoVaultFactory, 0)
	for _, factory := range append(deployment.VaultV1Factories, deployment.VaultV2Factories...) {
		if factory.Window.ActiveAt(block.Number) {
			factories = append(factories, factory)
		}
	}
	if len(factories) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(candidates)*len(factories))
	for _, candidate := range candidates {
		for _, factory := range factories {
			method := "isMetaMorpho"
			if factory.Version == morphoVaultV2 {
				method = "isVaultV2"
			}
			calls = append(calls, ContractCall{
				Contract: factory.Address, ABI: morphoFactoryABI, Method: method, Args: []any{candidate},
			})
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("vault factory validation: %w", err)
	}
	result := make([]morphoVaultRef, 0)
	for candidateIndex, candidate := range candidates {
		var matched *morphoVaultVersion
		for factoryIndex, factory := range factories {
			registered, decodeErr := BoolAt(rows[candidateIndex*len(factories)+factoryIndex], 0)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if !registered {
				continue
			}
			if matched != nil && *matched != factory.Version {
				return nil, fmt.Errorf("vault %s is registered by both Morpho generations", candidate)
			}
			version := factory.Version
			matched = &version
		}
		if matched != nil {
			result = append(result, morphoVaultRef{Address: candidate, Version: *matched})
		}
	}
	return result, nil
}

func mergeMorphoRefs(
	indexed morphoPositionRefs,
	tailMarkets []common.Hash,
	tailVaults []morphoVaultRef,
) (morphoPositionRefs, error) {
	marketSet := make(map[common.Hash]struct{}, len(indexed.MarketIDs)+len(tailMarkets))
	for _, marketID := range append(indexed.MarketIDs, tailMarkets...) {
		marketSet[marketID] = struct{}{}
	}
	indexed.MarketIDs = indexed.MarketIDs[:0]
	for marketID := range marketSet {
		indexed.MarketIDs = append(indexed.MarketIDs, marketID)
	}
	sort.Slice(indexed.MarketIDs, func(i, j int) bool {
		return indexed.MarketIDs[i].Hex() < indexed.MarketIDs[j].Hex()
	})
	vaultMap := make(map[common.Address]morphoVaultRef, len(indexed.Vaults)+len(tailVaults))
	for _, vault := range append(indexed.Vaults, tailVaults...) {
		if existing, exists := vaultMap[vault.Address]; exists && existing.Version != vault.Version {
			return morphoPositionRefs{}, fmt.Errorf("index and RPC tail disagree on vault %s generation", vault.Address)
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
	indexed, err := func() (morphoPositionRefs, error) {
		sentioQueryMu.Lock()
		defer sentioQueryMu.Unlock()
		return i.indexedRefs(ctx, block, account, includeCurrentFeeMarkets)
	}()
	if err != nil {
		return morphoPositionRefs{}, fmt.Errorf("Morpho index query: %w", err)
	}
	if indexed.IndexerBlock >= block.Number {
		return indexed, nil
	}
	fromBlock := indexed.IndexerBlock + 1
	tailMarkets, err := morphoTailMarketIDs(
		ctx,
		client,
		fromBlock,
		block.Number,
		account,
		deployment.Morpho,
		includeCurrentFeeMarkets,
	)
	if err != nil {
		return morphoPositionRefs{}, err
	}
	candidates, err := morphoTailVaultCandidates(ctx, client, fromBlock, block.Number, account)
	if err != nil {
		return morphoPositionRefs{}, err
	}
	tailVaults, err := morphoClassifyTailVaults(ctx, client, block, candidates, deployment)
	if err != nil {
		return morphoPositionRefs{}, err
	}
	return mergeMorphoRefs(indexed, tailMarkets, tailVaults)
}
