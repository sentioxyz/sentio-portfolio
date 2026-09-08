package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

// SuiJSONRPCClient reads a Sui network through the JSON-RPC surface a fullnode, or a proxy in
// front of one, serves. It is the transport a deployment that runs its own Sui fullnodes already
// has, so it needs no new infrastructure; what it cannot do is read the past. suix_getAllBalances
// answers only for the head, so a read is bracketed by two head observations and reported as a
// window rather than a point: SuiHoldings.Exact is false, Checkpoint is the head observed after
// the read and HeadBeforeRead the one observed before it. A pinned read is refused outright.
//
// Requests go through go-ethereum's JSON-RPC client, as the EVM RPCClient's do, so batching and
// HTTP handling are shared code; a server that rejects batches is detected once and served with
// single calls from then on.
type SuiJSONRPCClient struct {
	chainID   ChainID
	client    *rpc.Client
	transport *http.Transport

	mu               sync.Mutex
	batchUnsupported bool
}

var _ SuiReader = (*SuiJSONRPCClient)(nil)

const (
	suiJSONRPCAttempts     = 3
	suiJSONRPCCallTimeout  = 20 * time.Second
	suiJSONRPCRetryInitial = 300 * time.Millisecond
	// suiJSONRPCMetadataBatchSize is how many suix_getCoinMetadata calls one batch carries.
	suiJSONRPCMetadataBatchSize = 25
)

// DialSuiJSONRPC connects to a Sui JSON-RPC endpoint and verifies it serves the network named by
// chainIdentifier. The endpoint is never quoted in errors.
func DialSuiJSONRPC(
	ctx context.Context,
	chainID ChainID,
	endpoint string,
	chainIdentifier string,
) (*SuiJSONRPCClient, error) {
	if endpoint == "" {
		return nil, errors.New("Sui JSON-RPC endpoint is not configured")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		MaxConnsPerHost:     8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 6 * time.Second,
	}
	httpClient := &http.Client{Timeout: suiJSONRPCCallTimeout, Transport: transport}
	client, err := rpc.DialOptions(ctx, endpoint, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("dial Sui JSON-RPC: %w", redactEndpoints(err))
	}
	result := &SuiJSONRPCClient{chainID: chainID, client: client, transport: transport}
	var actual string
	if err := result.call(ctx, &actual, "sui_getChainIdentifier"); err != nil {
		client.Close()
		return nil, fmt.Errorf("read Sui chain identifier: %w", err)
	}
	if err := checkSuiChainIdentifier(actual, chainIdentifier); err != nil {
		client.Close()
		return nil, err
	}
	return result, nil
}

func (c *SuiJSONRPCClient) Close() {
	c.client.Close()
}

// retryableSuiJSONRPCError retries transport failures and gateway statuses. A JSON-RPC error
// object is the server's verdict on the request and is final: Sui error codes do not reuse the
// EVM pools' rate-limit codes, so the EVM classifier's -32005 rule would wrongly retry a server
// saying it does not support batches.
func retryableSuiJSONRPCError(err error) bool {
	if err == nil {
		return false
	}
	var httpError rpc.HTTPError
	if errors.As(err, &httpError) {
		return retryableSuiStatus(httpError.StatusCode)
	}
	var rpcError rpc.Error
	if errors.As(err, &rpcError) {
		return false
	}
	// A response the client cannot decode is the server's shape, not a transient fault; it is what
	// a server that does not serve batches answers a batch with.
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return false
	}
	return true
}

func (c *SuiJSONRPCClient) call(ctx context.Context, result any, method string, args ...any) error {
	var last error
	for attempt := 0; attempt < suiJSONRPCAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, suiJSONRPCCallTimeout)
		startedAt := time.Now()
		last = describeSuiTransportError(c.client.CallContext(callCtx, result, method, args...))
		cancel()
		observeSuiRequest(ctx, c.chainID, method, 0, attempt, startedAt, last)
		if last == nil {
			return nil
		}
		if !retryableSuiJSONRPCError(last) {
			return redactEndpoints(last)
		}
		if attempt+1 < suiJSONRPCAttempts {
			c.transport.CloseIdleConnections()
			if err := waitSuiRetry(ctx, suiJSONRPCRetryInitial<<attempt); err != nil {
				return err
			}
		}
	}
	return redactEndpoints(fmt.Errorf("Sui JSON-RPC failed after %d attempts: %w", suiJSONRPCAttempts, last))
}

// batchCall runs a batch, or the same elements as single calls once the server has shown it
// rejects batches. Element errors stay on their elements; only the round trip is retried.
func (c *SuiJSONRPCClient) batchCall(ctx context.Context, batch []rpc.BatchElem) error {
	c.mu.Lock()
	single := c.batchUnsupported
	c.mu.Unlock()
	if single || len(batch) == 1 {
		return c.singleCalls(ctx, batch)
	}
	var last error
	for attempt := 0; attempt < suiJSONRPCAttempts; attempt++ {
		for index := range batch {
			batch[index].Error = nil
		}
		callCtx, cancel := context.WithTimeout(ctx, suiJSONRPCCallTimeout)
		startedAt := time.Now()
		last = describeSuiTransportError(c.client.BatchCallContext(callCtx, batch))
		cancel()
		observeSuiRequest(ctx, c.chainID, batchMethod(batch), len(batch), attempt, startedAt, last)
		if last == nil {
			return nil
		}
		if !retryableSuiJSONRPCError(last) {
			break
		}
		if attempt+1 < suiJSONRPCAttempts {
			c.transport.CloseIdleConnections()
			if err := waitSuiRetry(ctx, suiJSONRPCRetryInitial<<attempt); err != nil {
				return err
			}
		}
	}
	// A server that does not serve batches answers one with a single error object, which the
	// client cannot decode as a batch response. Rather than parse that message, run the elements
	// singly: if they succeed the batch was the problem, and later batches skip straight to
	// single calls; if they fail the real errors surface on the elements.
	if err := c.singleCalls(ctx, batch); err != nil {
		return redactEndpoints(fmt.Errorf("Sui JSON-RPC batch: %w", errors.Join(last, err)))
	}
	c.mu.Lock()
	c.batchUnsupported = true
	c.mu.Unlock()
	return nil
}

func (c *SuiJSONRPCClient) singleCalls(ctx context.Context, batch []rpc.BatchElem) error {
	for index := range batch {
		batch[index].Error = c.call(ctx, batch[index].Result, batch[index].Method, batch[index].Args...)
		var rpcError rpc.Error
		if batch[index].Error != nil && !errors.As(batch[index].Error, &rpcError) {
			// A transport failure fails the round trip; an element-level verdict stays on it.
			return batch[index].Error
		}
	}
	return nil
}

type suiJSONRPCCheckpoint struct {
	SequenceNumber string `json:"sequenceNumber"`
	Digest         string `json:"digest"`
	TimestampMs    string `json:"timestampMs"`
}

func (p suiJSONRPCCheckpoint) checkpoint() (SuiCheckpoint, error) {
	sequence, err := strconv.ParseUint(p.SequenceNumber, 10, 64)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint sequence %q: %w", p.SequenceNumber, err)
	}
	millis, err := strconv.ParseInt(p.TimestampMs, 10, 64)
	if err != nil || millis <= 0 {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d timestamp %q is invalid", sequence, p.TimestampMs)
	}
	return newSuiCheckpoint(sequence, p.Digest, time.UnixMilli(millis))
}

// latestSequence is the head the endpoint advertises. It is one call, which is what bracketing a
// head read needs; LatestCheckpoint adds the digest and timestamp with a second.
func (c *SuiJSONRPCClient) latestSequence(ctx context.Context) (uint64, error) {
	var raw string
	if err := c.call(ctx, &raw, "sui_getLatestCheckpointSequenceNumber"); err != nil {
		return 0, err
	}
	sequence, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("latest checkpoint sequence %q: %w", raw, err)
	}
	return sequence, nil
}

func (c *SuiJSONRPCClient) LatestCheckpoint(ctx context.Context) (SuiCheckpoint, error) {
	sequence, err := c.latestSequence(ctx)
	if err != nil {
		return SuiCheckpoint{}, err
	}
	return c.CheckpointBySequence(ctx, sequence)
}

// CheckpointBySequence resolves a checkpoint. Fullnodes answer an unknown sequence with a
// JSON-RPC error rather than null; either is errSuiCheckpointUnavailable.
func (c *SuiJSONRPCClient) CheckpointBySequence(ctx context.Context, sequence uint64) (SuiCheckpoint, error) {
	var payload *suiJSONRPCCheckpoint
	err := c.call(ctx, &payload, "sui_getCheckpoint", strconv.FormatUint(sequence, 10))
	if err != nil {
		var rpcError rpc.Error
		if errors.As(err, &rpcError) {
			return SuiCheckpoint{}, fmt.Errorf("checkpoint %d: %w: %v", sequence, errSuiCheckpointUnavailable, err)
		}
		return SuiCheckpoint{}, err
	}
	if payload == nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d: %w", sequence, errSuiCheckpointUnavailable)
	}
	checkpoint, err := payload.checkpoint()
	if err != nil {
		return SuiCheckpoint{}, err
	}
	if checkpoint.Sequence != sequence {
		return SuiCheckpoint{}, fmt.Errorf(
			"Sui JSON-RPC returned checkpoint %d for sequence %d", checkpoint.Sequence, sequence,
		)
	}
	return checkpoint, nil
}

type suiJSONRPCBalance struct {
	CoinType     string `json:"coinType"`
	TotalBalance string `json:"totalBalance"`
}

// Holdings reads owner's balances from the head. pin must be nil: this transport cannot read at a
// fixed checkpoint and will not pretend to by reading the head instead. The head is observed
// before and after the read so the caller knows the window the balances belong to.
func (c *SuiJSONRPCClient) Holdings(
	ctx context.Context,
	owner SuiAddress,
	pin *SuiCheckpoint,
) (SuiHoldings, error) {
	if pin != nil {
		return SuiHoldings{}, fmt.Errorf("checkpoint %d: %w", pin.Sequence, errSuiPinnedReadUnsupported)
	}
	before, err := c.latestSequence(ctx)
	if err != nil {
		return SuiHoldings{}, err
	}
	var payload []suiJSONRPCBalance
	if err := c.call(ctx, &payload, "suix_getAllBalances", owner.Hex()); err != nil {
		return SuiHoldings{}, err
	}
	rows := make([]suiBalanceRow, 0, len(payload))
	for _, balance := range payload {
		rows = append(rows, suiBalanceRow{coinType: balance.CoinType, amount: balance.TotalBalance})
	}
	balances, err := suiBalancesFrom(rows)
	if err != nil {
		return SuiHoldings{}, err
	}
	after, err := c.LatestCheckpoint(ctx)
	if err != nil {
		return SuiHoldings{}, err
	}
	if after.Sequence < before {
		// A pool routing the two head reads to different backends can answer the second from a
		// node that is behind. The balances still came from some head in between, so keep the
		// window ordered rather than report one that ends before it starts.
		before = after.Sequence
	}
	return SuiHoldings{
		Balances:       balances,
		Checkpoint:     after,
		Exact:          false,
		HeadBeforeRead: before,
	}, nil
}

type suiJSONRPCCoinMetadata struct {
	Decimals *int    `json:"decimals"`
	Symbol   *string `json:"symbol"`
	Name     string  `json:"name"`
}

// CoinMetadata reads the on-chain metadata of the given coin types, several per batch. Coin types
// must already be normalized.
func (c *SuiJSONRPCClient) CoinMetadata(
	ctx context.Context,
	coinTypes []string,
) (map[string]SuiCoinMetadata, map[string]error, error) {
	metadata := make(map[string]SuiCoinMetadata, len(coinTypes))
	unusable := make(map[string]error)
	for start := 0; start < len(coinTypes); start += suiJSONRPCMetadataBatchSize {
		end := min(start+suiJSONRPCMetadataBatchSize, len(coinTypes))
		batch := make([]rpc.BatchElem, end-start)
		results := make([]*suiJSONRPCCoinMetadata, end-start)
		for offset, coinType := range coinTypes[start:end] {
			batch[offset] = rpc.BatchElem{
				Method: "suix_getCoinMetadata",
				Args:   []any{coinType},
				Result: &results[offset],
			}
		}
		if err := c.batchCall(ctx, batch); err != nil {
			return nil, nil, fmt.Errorf("coin metadata %d-%d: %w", start, end-1, err)
		}
		for offset, coinType := range coinTypes[start:end] {
			if batch[offset].Error != nil {
				unusable[coinType] = fmt.Errorf("coin metadata: %w", redactEndpoints(batch[offset].Error))
				continue
			}
			entry := results[offset]
			if entry == nil {
				unusable[coinType] = errors.New("coin has no on-chain metadata")
				continue
			}
			validated, err := suiCoinMetadataFrom(coinType, entry.Decimals, entry.Symbol, entry.Name)
			if err != nil {
				unusable[coinType] = err
				continue
			}
			metadata[coinType] = validated
		}
	}
	return metadata, unusable, nil
}
