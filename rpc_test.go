package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type rpcTestRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      json.RawMessage   `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type rpcTestServer struct {
	t             *testing.T
	mu            sync.Mutex
	batchAttempts int
	retryable     bool
	headLag       bool
	result        string
}

func (s *rpcTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode RPC call: %v", err)
			return
		}
		if call.Method != "eth_chainId" {
			s.t.Errorf("unexpected singleton method %q", call.Method)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": call.ID, "result": "0x1",
		})
		return
	}

	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode RPC batch: %v", err)
		return
	}
	s.mu.Lock()
	s.batchAttempts++
	attempt := s.batchAttempts
	s.mu.Unlock()
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		if index == 0 && (attempt == 1 || !s.retryable) {
			code := 3
			message := "execution reverted"
			if s.headLag {
				code = -32000
				message = "header not found"
			} else if s.retryable {
				code = -32005
				message = "request limit exceeded"
			}
			response["error"] = map[string]any{"code": code, "message": message}
		} else {
			response["result"] = s.result
		}
		responses[index] = response
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func newRPCTestClient(t *testing.T, retryable bool) (*RPCClient, *rpcTestServer) {
	t.Helper()
	contractABI := MustABI(`[
	  {"type":"function","name":"value","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
	]`)
	encoded, err := contractABI.Methods["value"].Outputs.Pack(big.NewInt(7))
	if err != nil {
		t.Fatal(err)
	}
	handler := &rpcTestServer{t: t, retryable: retryable, result: "0x" + common.Bytes2Hex(encoded)}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, handler
}

func rpcTestCalls() []ContractCall {
	contractABI := MustABI(`[
	  {"type":"function","name":"value","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
	]`)
	return []ContractCall{
		{Contract: common.HexToAddress("0x1"), ABI: contractABI, Method: "value"},
		{Contract: common.HexToAddress("0x2"), ABI: contractABI, Method: "value"},
	}
}

func TestParallelCallsAllowFailureRetriesRetryableElementError(t *testing.T) {
	client, handler := newRPCTestClient(t, true)
	results, err := client.ParallelCallsAllowFailure(
		context.Background(),
		BlockRef{ChainID: Ethereum, Number: 1, Fixed: true},
		rpcTestCalls(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.batchAttempts != 2 {
		t.Fatalf("batch attempts = %d, want 2", handler.batchAttempts)
	}
	for index, result := range results {
		if result.Error != nil {
			t.Fatalf("result %d error after retry: %v", index, result.Error)
		}
		value, valueErr := BigIntAt(result.Values, 0)
		if valueErr != nil || value.Cmp(big.NewInt(7)) != 0 {
			t.Fatalf("result %d = %v, %v; want 7", index, value, valueErr)
		}
	}
}

func TestParallelCallsAllowFailureRetriesHeaderPropagationLag(t *testing.T) {
	client, handler := newRPCTestClient(t, true)
	handler.headLag = true
	results, err := client.ParallelCallsAllowFailure(
		context.Background(),
		BlockRef{ChainID: Ethereum, Number: 1},
		rpcTestCalls(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.batchAttempts != 2 {
		t.Fatalf("batch attempts = %d, want 2", handler.batchAttempts)
	}
	for index, result := range results {
		if result.Error != nil {
			t.Fatalf("result %d error after retry: %v", index, result.Error)
		}
	}
}

func TestParallelCallsAllowFailurePreservesContractRevert(t *testing.T) {
	client, handler := newRPCTestClient(t, false)
	results, err := client.ParallelCallsAllowFailure(
		context.Background(),
		BlockRef{ChainID: Ethereum, Number: 1, Fixed: true},
		rpcTestCalls(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if handler.batchAttempts != 1 {
		t.Fatalf("batch attempts = %d, want 1", handler.batchAttempts)
	}
	if results[0].Error == nil {
		t.Fatal("contract revert was not preserved on the first result")
	}
	if results[1].Error != nil {
		t.Fatalf("successful sibling failed: %v", results[1].Error)
	}
}

func TestParallelCallsAllowFailureClassifiesEmptyResults(t *testing.T) {
	for _, test := range []struct {
		name      string
		raw       string
		noOutputs bool
		wantEmpty bool
		wantError bool
	}{
		{name: "empty", raw: "0x", wantEmpty: true, wantError: true},
		{name: "malformed nonempty", raw: "0x01", wantError: true},
		{name: "valid zero", raw: "0x" + strings.Repeat("0", 64)},
		{name: "no ABI outputs", raw: "0x", noOutputs: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, handler := newRPCTestClient(t, false)
			handler.result = test.raw
			calls := rpcTestCalls()
			if test.noOutputs {
				calls[1].ABI = MustABI(`[{"type":"function","name":"value","stateMutability":"view","inputs":[],"outputs":[]}]`)
			}
			rows, err := client.ParallelCallsAllowFailure(context.Background(), BlockRef{ChainID: Ethereum, Number: 1}, calls)
			if err != nil {
				t.Fatal(err)
			}
			// The first element is always a revert, even though its raw result is empty.
			if rows[0].Error == nil || errors.Is(rows[0].Error, errEmptyContractResult) {
				t.Fatalf("revert misclassified: %v", rows[0].Error)
			}
			if (rows[1].Error != nil) != test.wantError || errors.Is(rows[1].Error, errEmptyContractResult) != test.wantEmpty {
				t.Fatalf("result error = %v, wantError = %t, wantEmpty = %t", rows[1].Error, test.wantError, test.wantEmpty)
			}
		})
	}
}

func TestLogsPinsRangeAndTopics(t *testing.T) {
	contract := common.HexToAddress("0x1111111111111111111111111111111111111111")
	topic := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	account := common.HexToHash("0x0000000000000000000000002222222222222222222222222222222222222222")
	filterChannel := make(chan json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call rpcTestRequest
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Errorf("decode RPC request: %v", err)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if call.Method == "eth_chainId" {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"jsonrpc": "2.0", "id": call.ID, "result": "0x1",
			})
			return
		}
		if call.Method != "eth_getLogs" || len(call.Params) != 1 {
			t.Errorf("unexpected RPC call %s with %d params", call.Method, len(call.Params))
			return
		}
		filterChannel <- call.Params[0]
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0", "id": call.ID, "result": []map[string]any{{
				"address": contract, "topics": []common.Hash{topic, account},
			}},
		})
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	logs, err := client.Logs(
		context.Background(), 10, 20, []common.Address{contract},
		[][]common.Hash{{topic}, nil, {account}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Address != contract || len(logs[0].Topics) != 2 || logs[0].Topics[1] != account {
		t.Fatalf("logs = %+v", logs)
	}
	var filter struct {
		FromBlock string          `json:"fromBlock"`
		ToBlock   string          `json:"toBlock"`
		Address   common.Address  `json:"address"`
		Topics    [][]common.Hash `json:"topics"`
	}
	if err := json.Unmarshal(<-filterChannel, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.FromBlock != "0xa" || filter.ToBlock != "0x14" || filter.Address != contract ||
		len(filter.Topics) != 3 || len(filter.Topics[0]) != 1 || filter.Topics[0][0] != topic ||
		filter.Topics[1] != nil || len(filter.Topics[2]) != 1 || filter.Topics[2][0] != account {
		t.Fatalf("filter = %+v", filter)
	}
}

// AGENTS.md requires errors to redact endpoint URLs. The endpoints this service dials carry a
// credential in the path, and go-ethereum quotes the URL in transport errors, so an unredacted
// error is enough to leak one into a log line or a test failure.
func TestRPCErrorsRedactTheEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL + "/tOkEnThatMustNotLeak"
	server.Close()

	_, err := DialRPC(context.Background(), Ethereum, endpoint)
	if err == nil {
		t.Fatal("expected dialing a closed endpoint to fail")
	}
	message := err.Error()
	if strings.Contains(message, endpoint) || strings.Contains(message, "tOkEnThatMustNotLeak") {
		t.Fatalf("RPC error leaked the endpoint: %s", message)
	}
	if !strings.Contains(message, "[redacted URL]") {
		t.Fatalf("RPC error was not redacted: %s", message)
	}
}

func TestRedactEndpointsKeepsTheCauseInspectable(t *testing.T) {
	cause := errors.New(`Post "https://mainnet.example/secret": connection refused`)
	wrapped := fmt.Errorf("read chain id: %w", cause)
	redacted := redactEndpoints(wrapped)
	if strings.Contains(redacted.Error(), "secret") {
		t.Fatalf("redaction left the endpoint in place: %s", redacted.Error())
	}
	if !errors.Is(redacted, cause) {
		t.Fatal("redaction broke the error chain")
	}
}

// An error with no URL in it is returned unchanged, so redaction cannot disturb the callers that
// match on RPC error text (head lag, unknown block, and the rest).
func TestRedactEndpointsLeavesCleanErrorsAlone(t *testing.T) {
	cause := errors.New("block 21 is beyond the latest block 20")
	if redactEndpoints(cause) != cause {
		t.Fatal("an error without an endpoint was rewritten")
	}
}
