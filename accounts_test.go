package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const expectedInstadappListActivationBlock = uint64(9_747_258)

type accountScopeTestRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

type accountScopeTestServer struct {
	t      *testing.T
	header string
	calls  int
}

func (s *accountScopeTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.t.Helper()
	var call accountScopeTestRequest
	if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = "0x1"
	case "eth_call":
		s.calls++
		response["result"] = s.header
	default:
		s.t.Errorf("unexpected RPC method %q", call.Method)
		http.Error(writer, "unexpected RPC method", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		s.t.Errorf("encode RPC response: %v", err)
	}
}

func newAccountScopeTestClient(t *testing.T) (*RPCClient, *accountScopeTestServer) {
	t.Helper()
	header, err := instadappListABI.Methods["userLink"].Outputs.Pack(
		uint64(0),
		uint64(0),
		uint64(0),
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := &accountScopeTestServer{t: t, header: "0x" + common.Bytes2Hex(header)}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, handler
}

func TestResolveAccountScopeHonorsInstadappListDeployment(t *testing.T) {
	owner := common.HexToAddress("0x1234")
	for _, test := range []struct {
		name      string
		block     uint64
		wantCalls int
	}{
		{
			name:      "before activation",
			block:     expectedInstadappListActivationBlock - 1,
			wantCalls: 0,
		},
		{
			name:      "at activation",
			block:     expectedInstadappListActivationBlock,
			wantCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := newAccountScopeTestClient(t)
			accounts, err := resolveAccountScope(
				context.Background(),
				client,
				BlockRef{ChainID: Ethereum, Number: test.block, Fixed: true},
				owner,
			)
			if err != nil {
				t.Fatal(err)
			}
			if server.calls != test.wantCalls {
				t.Fatalf("Instadapp call count = %d, want %d", server.calls, test.wantCalls)
			}
			if len(accounts) != 1 || accounts[0] != (attributedAccount{
				Address: owner, Attribution: "wallet", Source: "direct",
			}) {
				t.Fatalf("accounts = %+v, want only the direct wallet", accounts)
			}
		})
	}
}
