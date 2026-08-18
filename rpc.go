package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	rpcAttempts     = 3
	rpcCallTimeout  = 6 * time.Second
	rpcRetryInitial = 500 * time.Millisecond
)

type rpcBlock struct {
	Number    hexutil.Uint64 `json:"number"`
	Hash      common.Hash    `json:"hash"`
	Timestamp hexutil.Uint64 `json:"timestamp"`
}

type rpcLog struct {
	Address     common.Address `json:"address"`
	Topics      []common.Hash  `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	LogIndex    hexutil.Uint64 `json:"logIndex"`
}

func sortRPCLogs(logs []rpcLog) {
	sort.Slice(logs, func(left, right int) bool {
		if logs[left].BlockNumber != logs[right].BlockNumber {
			return logs[left].BlockNumber < logs[right].BlockNumber
		}
		return logs[left].LogIndex < logs[right].LogIndex
	})
}

type RPCClient struct {
	chainID   ChainID
	client    *rpc.Client
	transport *http.Transport
}

func DialRPC(ctx context.Context, chainID ChainID, endpoint string) (*RPCClient, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		MaxConnsPerHost:     16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 6 * time.Second,
	}
	httpClient := &http.Client{
		Timeout:   rpcCallTimeout,
		Transport: transport,
	}
	client, err := rpc.DialOptions(ctx, endpoint, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("dial chain %d RPC: %w", chainID, err)
	}
	result := &RPCClient{chainID: chainID, client: client, transport: transport}
	var actual hexutil.Big
	if err := result.call(ctx, &actual, "eth_chainId"); err != nil {
		client.Close()
		return nil, fmt.Errorf("read chain id: %w", err)
	}
	if (*big.Int)(&actual).Uint64() != uint64(chainID) {
		client.Close()
		return nil, fmt.Errorf("RPC returned chain id %s, expected %d", (*big.Int)(&actual), chainID)
	}
	return result, nil
}

func (c *RPCClient) Close() {
	c.client.Close()
}

func retryableRPCError(err error) bool {
	if err == nil {
		return false
	}
	var httpError rpc.HTTPError
	if errors.As(err, &httpError) {
		status := httpError.StatusCode
		return status == http.StatusTooManyRequests || status == http.StatusBadGateway ||
			status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
	}
	var rpcError rpc.Error
	if errors.As(err, &rpcError) {
		code := rpcError.ErrorCode()
		if code == -32005 || code == -32009 {
			return true
		}
		if code == -32000 {
			message := strings.ToLower(rpcError.Error())
			return strings.Contains(message, "header not found") ||
				strings.Contains(message, "unknown block") ||
				strings.Contains(message, "block not found") ||
				strings.Contains(message, "beyond the latest block")
		}
		return false
	}
	return true
}

func (c *RPCClient) call(ctx context.Context, result any, method string, args ...any) error {
	var last error
	for attempt := 0; attempt < rpcAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		last = c.client.CallContext(callCtx, result, method, args...)
		cancel()
		if last == nil {
			return nil
		}
		if !retryableRPCError(last) {
			return last
		}
		if attempt+1 < rpcAttempts {
			// A timed-out keep-alive connection can otherwise be selected again
			// by net/http. Retrying on a fresh connection makes each retry useful.
			c.transport.CloseIdleConnections()
			timer := time.NewTimer(rpcRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("RPC failed after %d attempts: %w", rpcAttempts, last)
}

func (c *RPCClient) batchCall(ctx context.Context, batch []rpc.BatchElem) error {
	var last error
	for attempt := 0; attempt < rpcAttempts; attempt++ {
		for index := range batch {
			batch[index].Error = nil
		}
		callCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		last = c.client.BatchCallContext(callCtx, batch)
		cancel()
		if last == nil {
			for index := range batch {
				if batch[index].Error != nil {
					last = fmt.Errorf("batch element %d: %w", index, batch[index].Error)
					break
				}
			}
		}
		if last == nil {
			return nil
		}
		if !retryableRPCError(last) {
			return last
		}
		if attempt+1 < rpcAttempts {
			c.transport.CloseIdleConnections()
			timer := time.NewTimer(rpcRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("RPC batch failed after %d attempts: %w", rpcAttempts, last)
}

func (c *RPCClient) LatestBlock(ctx context.Context) (BlockRef, error) {
	return c.blockByTag(ctx, "latest")
}

func (c *RPCClient) BlockByNumber(ctx context.Context, number uint64) (BlockRef, error) {
	return c.blockByTag(ctx, hexutil.EncodeUint64(number))
}

func (c *RPCClient) blockByTag(ctx context.Context, tag string) (BlockRef, error) {
	var block rpcBlock
	if err := c.call(ctx, &block, "eth_getBlockByNumber", tag, false); err != nil {
		return BlockRef{}, err
	}
	if block.Hash == (common.Hash{}) {
		return BlockRef{}, fmt.Errorf("block %s did not include a hash", tag)
	}
	return BlockRef{
		ChainID:   c.chainID,
		Number:    uint64(block.Number),
		Hash:      block.Hash,
		Timestamp: uint64(block.Timestamp),
	}, nil
}

func (c *RPCClient) Call(
	ctx context.Context,
	block BlockRef,
	contract common.Address,
	contractABI abi.ABI,
	method string,
	args ...any,
) ([]any, error) {
	return c.callContract(ctx, block, common.Address{}, contract, contractABI, method, args...)
}

func (c *RPCClient) Logs(
	ctx context.Context,
	fromBlock uint64,
	toBlock uint64,
	addresses []common.Address,
	topics [][]common.Hash,
) ([]rpcLog, error) {
	if fromBlock > toBlock {
		return nil, fmt.Errorf("invalid log range %d-%d", fromBlock, toBlock)
	}
	filter := map[string]any{
		"fromBlock": hexutil.EncodeUint64(fromBlock),
		"toBlock":   hexutil.EncodeUint64(toBlock),
		"topics":    topics,
	}
	if len(addresses) == 1 {
		filter["address"] = addresses[0]
	} else if len(addresses) > 1 {
		filter["address"] = addresses
	}
	var logs []rpcLog
	if err := c.call(ctx, &logs, "eth_getLogs", filter); err != nil {
		return nil, err
	}
	return logs, nil
}

func (c *RPCClient) CallFrom(
	ctx context.Context,
	block BlockRef,
	from common.Address,
	contract common.Address,
	contractABI abi.ABI,
	method string,
	args ...any,
) ([]any, error) {
	return c.callContract(ctx, block, from, contract, contractABI, method, args...)
}

func (c *RPCClient) callContract(
	ctx context.Context,
	block BlockRef,
	from common.Address,
	contract common.Address,
	contractABI abi.ABI,
	method string,
	args ...any,
) ([]any, error) {
	data, err := contractABI.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", method, err)
	}
	var raw hexutil.Bytes
	call := map[string]any{"to": contract, "data": hexutil.Bytes(data)}
	if from != (common.Address{}) {
		call["from"] = from
	}
	if err := c.call(ctx, &raw, "eth_call", call, hexutil.EncodeUint64(block.Number)); err != nil {
		return nil, fmt.Errorf("%s %s: %w", contract, method, err)
	}
	values, err := contractABI.Unpack(method, raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", contract, method, err)
	}
	return values, nil
}

type ContractCall struct {
	Contract common.Address
	From     common.Address
	ABI      abi.ABI
	Method   string
	Args     []any
}

type ContractCallResult struct {
	Values []any
	Error  error
}

func contractCallObject(call ContractCall, data []byte) map[string]any {
	object := map[string]any{"to": call.Contract, "data": hexutil.Bytes(data)}
	if call.From != (common.Address{}) {
		object["from"] = call.From
	}
	return object
}

func (c *RPCClient) ParallelCalls(
	ctx context.Context,
	block BlockRef,
	calls []ContractCall,
) ([][]any, error) {
	results := make([][]any, len(calls))
	const batchSize = 16
	for start := 0; start < len(calls); start += batchSize {
		end := start + batchSize
		if end > len(calls) {
			end = len(calls)
		}
		raw := make([]hexutil.Bytes, end-start)
		batch := make([]rpc.BatchElem, end-start)
		for offset, call := range calls[start:end] {
			data, err := call.ABI.Pack(call.Method, call.Args...)
			if err != nil {
				return nil, fmt.Errorf("call %d encode %s: %w", start+offset, call.Method, err)
			}
			batch[offset] = rpc.BatchElem{
				Method: "eth_call",
				Args: []any{
					contractCallObject(call, data),
					hexutil.EncodeUint64(block.Number),
				},
				Result: &raw[offset],
			}
		}
		if err := c.batchCall(ctx, batch); err != nil {
			return nil, fmt.Errorf("calls %d-%d: %w", start, end-1, err)
		}
		for offset, call := range calls[start:end] {
			values, err := call.ABI.Unpack(call.Method, raw[offset])
			if err != nil {
				return nil, fmt.Errorf(
					"call %d decode %s %s: %w",
					start+offset,
					call.Contract,
					call.Method,
					err,
				)
			}
			results[start+offset] = values
		}
	}
	return results, nil
}

// ParallelCallsAllowFailure keeps contract-level reverts isolated while retaining transport-level
// retries. Callers must classify every result error; silently treating a failed call as zero would
// turn RPC degradation into a missing portfolio position.
func (c *RPCClient) ParallelCallsAllowFailure(
	ctx context.Context,
	block BlockRef,
	calls []ContractCall,
) ([]ContractCallResult, error) {
	results := make([]ContractCallResult, len(calls))
	const batchSize = 16
	for start := 0; start < len(calls); start += batchSize {
		end := min(start+batchSize, len(calls))
		raw := make([]hexutil.Bytes, end-start)
		batch := make([]rpc.BatchElem, end-start)
		for offset, call := range calls[start:end] {
			data, err := call.ABI.Pack(call.Method, call.Args...)
			if err != nil {
				return nil, fmt.Errorf("call %d encode %s: %w", start+offset, call.Method, err)
			}
			batch[offset] = rpc.BatchElem{
				Method: "eth_call",
				Args: []any{
					contractCallObject(call, data),
					hexutil.EncodeUint64(block.Number),
				},
				Result: &raw[offset],
			}
		}
		if err := c.batchCallTransport(ctx, batch); err != nil {
			return nil, fmt.Errorf("calls %d-%d: %w", start, end-1, err)
		}
		for offset, call := range calls[start:end] {
			index := start + offset
			if batch[offset].Error != nil {
				results[index].Error = fmt.Errorf(
					"%s %s: %w",
					call.Contract,
					call.Method,
					batch[offset].Error,
				)
				continue
			}
			values, err := call.ABI.Unpack(call.Method, raw[offset])
			if err != nil {
				results[index].Error = fmt.Errorf(
					"decode %s %s: %w",
					call.Contract,
					call.Method,
					err,
				)
				continue
			}
			results[index].Values = values
		}
	}
	return results, nil
}

func (c *RPCClient) batchCallTransport(ctx context.Context, batch []rpc.BatchElem) error {
	var last error
	for attempt := 0; attempt < rpcAttempts; attempt++ {
		for index := range batch {
			batch[index].Error = nil
		}
		callCtx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
		last = c.client.BatchCallContext(callCtx, batch)
		cancel()
		if last == nil {
			for index := range batch {
				if retryableRPCError(batch[index].Error) {
					last = fmt.Errorf("batch element %d: %w", index, batch[index].Error)
					break
				}
			}
			if last == nil {
				// Non-retryable element errors (for example, contract reverts) belong to
				// the individual result and must not fail otherwise independent calls.
				return nil
			}
		}
		if !retryableRPCError(last) {
			return last
		}
		if attempt+1 < rpcAttempts {
			c.transport.CloseIdleConnections()
			timer := time.NewTimer(rpcRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("RPC batch failed after %d attempts: %w", rpcAttempts, last)
}
