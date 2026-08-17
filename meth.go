package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	methActivationBlock     = 18_290_599
	methIndexerPageSize     = 500
	methMaxIndexedRequests  = 4_096
	methMaxRPCTailBlocks    = 2_048
	methCheckpointMaxLag    = 7*24*time.Hour + 30*time.Minute
	methIndexerRetryInitial = 300 * time.Millisecond
)

var (
	methTokenAddress   = common.HexToAddress("0xd5F7838F5C461fefF7FE49ea5ebaF7728bB0ADfa")
	methStakingAddress = common.HexToAddress("0xe3cBd06D7dadB3F4e6557bAb7EdD924CD1489E8f")
	methManagerAddress = common.HexToAddress("0x38fDF7b489316e03eD8754ad339cb5c4483FDcf9")
	methETHToken       = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	methRequestCreatedTopic = crypto.Keccak256Hash(
		[]byte("UnstakeRequestCreated(uint256,address,uint256,uint256,uint256,uint256)"),
	)
)

var methABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakingContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"unstakeRequestsManagerContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var methStakingABI = MustABI(`[
  {"type":"function","name":"mETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"unstakeRequestsManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"mETHToETH","stateMutability":"view","inputs":[{"name":"mETHAmount","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var methManagerABI = MustABI(`[
  {"type":"function","name":"mETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"stakingContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {
    "type":"function",
    "name":"requestByID",
    "stateMutability":"view",
    "inputs":[{"name":"requestID","type":"uint256"}],
    "outputs":[
      {"name":"blockNumber","type":"uint64"},
      {"name":"requester","type":"address"},
      {"name":"id","type":"uint128"},
      {"name":"mETHLocked","type":"uint128"},
      {"name":"ethRequested","type":"uint128"},
      {"name":"cumulativeETHRequested","type":"uint128"}
    ]
  }
]`)

type methIndexerStatus struct {
	Processors []struct {
		Version      any    `json:"version"`
		VersionState string `json:"versionState"`
		Status       struct {
			State string `json:"state"`
		} `json:"processorStatus"`
		States []struct {
			ChainID             string `json:"chainId"`
			ProcessedBlock      string `json:"processedBlockNumber"`
			EstimatedLatest     string `json:"estimatedLatestBlockNumber"`
			ProcessingStateData struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"states"`
	} `json:"processors"`
}

type methGraphQLResponse struct {
	Data struct {
		Checkpoints []struct {
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
		} `json:"indexerCheckpoints"`
		Requests []struct {
			ID        string `json:"id"`
			Owner     string `json:"owner"`
			RequestID string `json:"requestId"`
		} `json:"withdrawalRequests"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type methRPCLog struct {
	Topics  []common.Hash `json:"topics"`
	Removed bool          `json:"removed"`
}

type MethAdapter struct {
	adapterBase
	indexer    SentioIndexerConfig
	apiKey     string
	httpClient *http.Client
	statusMu   sync.Mutex
	statusAt   time.Time
	processed  uint64
}

func newMethAdapter(indexer SentioIndexerConfig) Adapter {
	apiKey := os.Getenv("PORTFOLIO_SENTIO_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("NEXT_PUBLIC_SENTIO_API_KEY")
	}
	return &MethAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "meth-protocol", Name: "Mantle mETH", Chains: []ChainID{Ethereum},
		}},
		indexer:    indexer,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 25 * time.Second},
	}
}

func (a *MethAdapter) doJSON(
	ctx context.Context,
	method string,
	endpoint string,
	body any,
	result any,
) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		var reader io.Reader
		if body != nil {
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			reader = bytes.NewReader(payload)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		request.Header.Set("accept-encoding", "identity")
		if body != nil {
			request.Header.Set("content-type", "application/json")
		}
		if a.apiKey != "" {
			request.Header.Set("api-key", a.apiKey)
		}
		response, err := a.httpClient.Do(request)
		if err == nil {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			response.Body.Close()
			if readErr != nil {
				err = readErr
			} else if response.StatusCode != http.StatusOK {
				err = fmt.Errorf(
					"HTTP %d: %s",
					response.StatusCode,
					strings.TrimSpace(string(payload[:min(len(payload), 300)])),
				)
				if response.StatusCode != http.StatusTooManyRequests &&
					response.StatusCode < http.StatusInternalServerError {
					return err
				}
			} else if decodeErr := json.Unmarshal(payload, result); decodeErr != nil {
				err = decodeErr
			} else {
				return nil
			}
		}
		last = err
		if attempt < 2 {
			timer := time.NewTimer(methIndexerRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("request failed after 3 attempts: %w", last)
}

func (a *MethAdapter) processedBlock(ctx context.Context) (uint64, error) {
	if err := a.indexer.validate(); err != nil {
		return 0, err
	}
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	if time.Since(a.statusAt) < 30*time.Second && a.processed > 0 {
		return a.processed, nil
	}
	var payload methIndexerStatus
	if err := a.doJSON(ctx, http.MethodGet, a.indexer.StatusURL, nil, &payload); err != nil {
		return 0, fmt.Errorf("processor status: %w", err)
	}
	var matches []uint64
	for _, processor := range payload.Processors {
		if fmt.Sprint(processor.Version) != a.indexer.ProcessorVersion ||
			processor.VersionState != "ACTIVE" ||
			processor.Status.State != "PROCESSING" {
			continue
		}
		for _, state := range processor.States {
			if state.ChainID != "1" || state.ProcessingStateData.State != "PROCESSING_LATEST" {
				continue
			}
			block, err := strconv.ParseUint(state.ProcessedBlock, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("processor returned invalid block %q", state.ProcessedBlock)
			}
			matches = append(matches, block)
		}
	}
	if len(matches) != 1 {
		return 0, fmt.Errorf("processor status returned %d active Ethereum states", len(matches))
	}
	a.processed = matches[0]
	a.statusAt = time.Now()
	return a.processed, nil
}

const methWalletQuery = `
query MethWithdrawalRequests(
  $prefix: String!
  $after: ID!
  $checkpoint: ID!
  $first: Int!
  $block: BigInt!
) {
  indexerCheckpoints(first: 2, block: { number: $block }, where: { id: $checkpoint }) {
    blockNumber
    timestampMs
  }
  withdrawalRequests(
    first: $first
    orderBy: id
    orderDirection: asc
    block: { number: $block }
    where: { id_starts_with: $prefix, id_gt: $after }
  ) {
    id
    owner
    requestId
  }
}`

func (a *MethAdapter) graphqlPage(
	ctx context.Context,
	prefix string,
	after string,
	first int,
	block uint64,
) (methGraphQLResponse, error) {
	if err := a.indexer.validate(); err != nil {
		return methGraphQLResponse{}, err
	}
	var payload methGraphQLResponse
	err := a.doJSON(
		ctx,
		http.MethodPost,
		a.indexer.GraphQLURL,
		map[string]any{
			"query": methWalletQuery,
			"variables": map[string]any{
				"prefix":     prefix,
				"after":      after,
				"checkpoint": "1",
				"first":      first,
				"block":      strconv.FormatUint(block, 10),
			},
		},
		&payload,
	)
	if err != nil {
		return methGraphQLResponse{}, err
	}
	if len(payload.Errors) > 0 {
		messages := make([]string, 0, len(payload.Errors))
		for _, gqlErr := range payload.Errors {
			messages = append(messages, gqlErr.Message)
		}
		return methGraphQLResponse{}, fmt.Errorf("GraphQL: %s", strings.Join(messages, "; "))
	}
	return payload, nil
}

func validateMethCheckpoint(block BlockRef, page methGraphQLResponse) error {
	if len(page.Data.Checkpoints) != 1 {
		return fmt.Errorf("GraphQL returned %d checkpoints", len(page.Data.Checkpoints))
	}
	checkpoint := page.Data.Checkpoints[0]
	checkpointBlock, err := strconv.ParseUint(checkpoint.BlockNumber, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid checkpoint block %q", checkpoint.BlockNumber)
	}
	checkpointMS, err := strconv.ParseUint(checkpoint.TimestampMS, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid checkpoint timestamp %q", checkpoint.TimestampMS)
	}
	if checkpointBlock > block.Number {
		return fmt.Errorf("checkpoint %d is ahead of target %d", checkpointBlock, block.Number)
	}
	checkpointSeconds := checkpointMS / 1_000
	if checkpointSeconds > block.Timestamp {
		return fmt.Errorf("checkpoint timestamp is ahead of target")
	}
	lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
	if lag > methCheckpointMaxLag {
		return fmt.Errorf("indexer checkpoint is stale by %s", lag)
	}
	return nil
}

func (a *MethAdapter) indexedRequestIDs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
	queryBlock uint64,
) ([]*big.Int, error) {
	prefix := fmt.Sprintf("1:%s:", strings.ToLower(account.Hex()))
	after := prefix
	result := make([]*big.Int, 0)
	var checkpointBlock string
	var checkpointTimestamp string
	for len(result) <= methMaxIndexedRequests {
		first := min(methIndexerPageSize, methMaxIndexedRequests+1-len(result))
		page, err := a.graphqlPage(ctx, prefix, after, first, queryBlock)
		if err != nil {
			return nil, err
		}
		if err := validateMethCheckpoint(block, page); err != nil {
			return nil, err
		}
		checkpoint := page.Data.Checkpoints[0]
		if checkpointBlock == "" {
			checkpointBlock = checkpoint.BlockNumber
			checkpointTimestamp = checkpoint.TimestampMS
		} else if checkpointBlock != checkpoint.BlockNumber ||
			checkpointTimestamp != checkpoint.TimestampMS {
			return nil, errors.New("GraphQL pagination changed checkpoint")
		}
		for _, row := range page.Data.Requests {
			if !strings.HasPrefix(row.ID, prefix) || row.ID <= after ||
				!strings.EqualFold(row.Owner, account.Hex()) {
				return nil, fmt.Errorf("GraphQL returned invalid owner row %q", row.ID)
			}
			requestID, parseErr := new(big.Int).SetString(row.RequestID, 10)
			if !parseErr || requestID.Sign() < 0 {
				return nil, fmt.Errorf("GraphQL returned invalid request ID %q", row.RequestID)
			}
			after = row.ID
			result = append(result, requestID)
		}
		if len(page.Data.Requests) < first {
			break
		}
	}
	if len(result) > methMaxIndexedRequests {
		return nil, fmt.Errorf("account has more than %d indexed requests", methMaxIndexedRequests)
	}
	return result, nil
}

func methTailRequestIDs(
	ctx context.Context,
	client *RPCClient,
	account common.Address,
	fromBlock uint64,
	toBlock uint64,
) ([]*big.Int, error) {
	if fromBlock > toBlock {
		return nil, nil
	}
	accountTopic := common.BytesToHash(common.LeftPadBytes(account.Bytes(), 32))
	var logs []methRPCLog
	filter := map[string]any{
		"address":   methManagerAddress,
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
		"topics":    []any{methRequestCreatedTopic, nil, accountTopic},
	}
	if err := client.call(ctx, &logs, "eth_getLogs", filter); err != nil {
		return nil, err
	}
	result := make([]*big.Int, 0, len(logs))
	for _, log := range logs {
		if log.Removed {
			continue
		}
		if len(log.Topics) < 3 || log.Topics[0] != methRequestCreatedTopic ||
			log.Topics[2] != accountTopic {
			return nil, errors.New("RPC tail returned malformed withdrawal log")
		}
		result = append(result, log.Topics[1].Big())
	}
	return result, nil
}

func (a *MethAdapter) requestIDs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]*big.Int, error) {
	sentioQueryMu.Lock()
	defer sentioQueryMu.Unlock()
	processed, err := a.processedBlock(ctx)
	if err != nil {
		return nil, err
	}
	queryBlock := block.Number
	if processed < queryBlock {
		queryBlock = processed
	}
	indexed, err := a.indexedRequestIDs(ctx, block, account, queryBlock)
	if err != nil {
		return nil, err
	}
	if block.Number > processed && block.Number-processed > methMaxRPCTailBlocks {
		return nil, fmt.Errorf(
			"indexer tail is %d blocks, maximum %d",
			block.Number-processed,
			methMaxRPCTailBlocks,
		)
	}
	tail, err := methTailRequestIDs(ctx, client, account, processed+1, block.Number)
	if err != nil {
		return nil, fmt.Errorf("RPC tail: %w", err)
	}
	unique := make(map[string]*big.Int, len(indexed)+len(tail))
	for _, requestID := range append(indexed, tail...) {
		unique[requestID.String()] = requestID
	}
	if len(unique) > methMaxIndexedRequests {
		return nil, fmt.Errorf("account has more than %d requests", methMaxIndexedRequests)
	}
	result := make([]*big.Int, 0, len(unique))
	for _, requestID := range unique {
		result = append(result, requestID)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Cmp(result[right]) < 0
	})
	return result, nil
}

func assertMethWiring(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) error {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: methTokenAddress, ABI: methABI, Method: "stakingContract"},
		{Contract: methTokenAddress, ABI: methABI, Method: "unstakeRequestsManagerContract"},
		{Contract: methStakingAddress, ABI: methStakingABI, Method: "mETH"},
		{Contract: methStakingAddress, ABI: methStakingABI, Method: "unstakeRequestsManager"},
		{Contract: methManagerAddress, ABI: methManagerABI, Method: "mETH"},
		{Contract: methManagerAddress, ABI: methManagerABI, Method: "stakingContract"},
	})
	if err != nil {
		return err
	}
	expected := []common.Address{
		methStakingAddress,
		methManagerAddress,
		methTokenAddress,
		methManagerAddress,
		methTokenAddress,
		methStakingAddress,
	}
	for index := range expected {
		actual, decodeErr := AddressAt(rows[index], 0)
		if decodeErr != nil {
			return decodeErr
		}
		if actual != expected[index] {
			return fmt.Errorf("wiring call %d returned %s, expected %s", index, actual, expected[index])
		}
	}
	return nil
}

func readMethWithdrawals(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	requestIDs []*big.Int,
) (*Group, error) {
	if len(requestIDs) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(requestIDs))
	for index, requestID := range requestIDs {
		calls[index] = ContractCall{
			Contract: methManagerAddress,
			ABI:      methManagerABI,
			Method:   "requestByID",
			Args:     []any{requestID},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	amount := new(big.Int)
	active := make([]string, 0)
	for index, row := range rows {
		requester, decodeErr := AddressAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s owner: %w", requestIDs[index], decodeErr)
		}
		if requester == (common.Address{}) {
			continue
		}
		storedID, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s ID: %w", requestIDs[index], decodeErr)
		}
		if requester != account || storedID.Cmp(requestIDs[index]) != 0 {
			return nil, fmt.Errorf("request %s identity changed", requestIDs[index])
		}
		ethRequested, decodeErr := BigIntAt(row, 4)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s amount: %w", requestIDs[index], decodeErr)
		}
		amount.Add(amount, ethRequested)
		active = append(active, requestIDs[index].String())
	}
	if amount.Sign() == 0 {
		return nil, nil
	}
	component := NewComponent(
		"asset",
		methETHToken,
		amount,
		Source{
			Contract: methManagerAddress,
			Method:   "indexed owner request IDs + requestByID.ethRequested",
		},
	)
	return &Group{
		ID:         "meth-withdrawal",
		MarketID:   "meth-withdrawal",
		Label:      "Deposit · mETH withdrawal",
		Components: []Component{component},
		Metadata:   map[string]any{"requestIds": strings.Join(active, ",")},
	}, nil
}

func (a *MethAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum || block.Number < methActivationBlock {
		return nil, nil
	}
	if err := assertMethWiring(ctx, client, block); err != nil {
		return nil, fmt.Errorf("deployment wiring: %w", err)
	}
	balanceRows, err := client.ParallelCalls(ctx, block, []ContractCall{{
		Contract: methTokenAddress,
		ABI:      methABI,
		Method:   "balanceOf",
		Args:     []any{account},
	}})
	if err != nil {
		return nil, fmt.Errorf("mETH balance: %w", err)
	}
	shares, err := BigIntAt(balanceRows[0], 0)
	if err != nil {
		return nil, err
	}
	requestIDs, err := a.requestIDs(ctx, client, block, account)
	if err != nil {
		return nil, fmt.Errorf("withdrawal index: %w", err)
	}
	groups := make([]Group, 0, 2)
	if shares.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx,
			block,
			methStakingAddress,
			methStakingABI,
			"mETHToETH",
			shares,
		)
		if convertErr != nil {
			return nil, fmt.Errorf("convert mETH: %w", convertErr)
		}
		amount, decodeErr := BigIntAt(converted, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		component := NewComponent(
			"asset",
			methETHToken,
			amount,
			Source{Contract: methStakingAddress, Method: "mETHToETH(mETH.balanceOf)"},
		)
		component.Metadata = map[string]any{"shares": shares.String()}
		groups = append(groups, Group{
			ID:         "meth",
			MarketID:   "meth",
			Label:      "Staked · mETH",
			Components: []Component{component},
		})
	}
	withdrawal, err := readMethWithdrawals(ctx, client, block, account, requestIDs)
	if err != nil {
		return nil, fmt.Errorf("withdrawals: %w", err)
	}
	if withdrawal != nil {
		groups = append(groups, *withdrawal)
	}
	return groups, nil
}
