package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

type uniswapEnumerationTestRPC struct {
	mu              sync.Mutex
	block           BlockRef
	manager         common.Address
	account         common.Address
	count           *big.Int
	enumerable      bool
	duplicate       bool
	zeroID          bool
	wrongOwner      bool
	failMethod      string
	malformedMethod string
	methods         map[string]int
}

func (s *uniswapEnumerationTestRPC) Call(_ context.Context, args map[string]string, block string) (hexutil.Bytes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if block != hexutil.EncodeUint64(s.block.Number) || common.HexToAddress(args["to"]) != s.manager {
		return nil, fmt.Errorf("unexpected contract or unpinned block")
	}
	data, err := hexutil.Decode(args["data"])
	if err != nil || len(data) < 4 {
		return nil, fmt.Errorf("invalid calldata")
	}
	method, err := uniswapV3PositionManagerABI.MethodById(data[:4])
	if err != nil {
		return nil, err
	}
	s.methods[method.Name]++
	if method.Name == s.failMethod {
		return nil, fmt.Errorf("execution reverted")
	}
	if method.Name == s.malformedMethod {
		return hexutil.Bytes{1}, nil
	}
	values, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, err
	}
	switch method.Name {
	case "supportsInterface":
		if values[0] != ([4]byte{0x78, 0x0e, 0x9d, 0x63}) {
			return nil, fmt.Errorf("unexpected ERC165 interface")
		}
		return method.Outputs.Pack(s.enumerable)
	case "balanceOf":
		if values[0] != s.account {
			return nil, fmt.Errorf("unexpected balance owner")
		}
		return method.Outputs.Pack(s.count)
	case "tokenOfOwnerByIndex":
		if values[0] != s.account {
			return nil, fmt.Errorf("unexpected enumeration owner")
		}
		index := values[1].(*big.Int)
		if index.Cmp(s.count) >= 0 {
			return nil, fmt.Errorf("enumeration index out of bounds")
		}
		id := new(big.Int).Add(index, big.NewInt(1))
		if s.duplicate {
			id.SetInt64(1)
		}
		if s.zeroID {
			id.SetInt64(0)
		}
		return method.Outputs.Pack(id)
	case "ownerOf":
		owner := s.account
		if s.wrongOwner {
			owner = common.HexToAddress("0x2")
		}
		return method.Outputs.Pack(owner)
	case "positions":
		// A valid, closed NFT still belongs in a complete ownership inventory.
		return method.Outputs.Pack(big.NewInt(0), common.Address{}, common.HexToAddress("0x3"),
			common.HexToAddress("0x4"), big.NewInt(500), big.NewInt(-1), big.NewInt(1),
			big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0), big.NewInt(0))
	default:
		return nil, fmt.Errorf("unexpected method %s", method.Name)
	}
}

func newUniswapEnumerationTestClient(t *testing.T, count int64) (*RPCClient, *uniswapEnumerationTestRPC) {
	t.Helper()
	state := &uniswapEnumerationTestRPC{
		block:   BlockRef{ChainID: Ethereum, Number: 25_000_000, Timestamp: 1_800_000_000, Fixed: true},
		manager: uniswapV3Deployments[Ethereum].Manager, account: uniswapIndexerTestOwner,
		count: big.NewInt(count), enumerable: true, methods: make(map[string]int),
	}
	server := rpc.NewServer()
	if err := server.RegisterName("eth", state); err != nil {
		t.Fatal(err)
	}
	client := &RPCClient{chainID: Ethereum, client: rpc.DialInProc(server), transport: &http.Transport{}}
	t.Cleanup(func() { client.Close(); server.Stop() })
	return client, state
}

func TestUniswapV3EnumerationIsCompleteAndPinned(t *testing.T) {
	for _, count := range []int64{0, 1, 17, 4096} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			client, state := newUniswapEnumerationTestClient(t, count)
			nfts, err := enumerateUniswapV3NFTs(context.Background(), client, state.block, state.account, state.manager)
			if err != nil || len(nfts) != int(count) {
				t.Fatalf("enumerated %d/%d NFTs: %v", len(nfts), count, err)
			}
			for index, nft := range nfts {
				if nft.TokenID.Int64() != int64(index+1) || nft.Manager != state.manager {
					t.Fatalf("NFT %d = %+v", index, nft)
				}
			}
			if state.methods["tokenOfOwnerByIndex"] != int(count) || state.methods["ownerOf"] != int(count) {
				t.Fatalf("inventory not fully read and verified: %v", state.methods)
			}
		})
	}
}

func TestUniswapV3EnumerationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*uniswapEnumerationTestRPC)
		want string
	}{
		{"unsupported", func(s *uniswapEnumerationTestRPC) { s.enumerable = false }, "does not support"},
		{"over cap", func(s *uniswapEnumerationTestRPC) { s.count.SetInt64(4097) }, "maximum is 4096"},
		{"count overflow", func(s *uniswapEnumerationTestRPC) { s.count.Lsh(big.NewInt(1), 200) }, "maximum is 4096"},
		{"duplicate", func(s *uniswapEnumerationTestRPC) { s.duplicate = true }, "duplicate ID"},
		{"zero ID", func(s *uniswapEnumerationTestRPC) { s.zeroID = true }, "invalid ID"},
		{"wrong owner", func(s *uniswapEnumerationTestRPC) { s.wrongOwner = true }, "invalid owner"},
		{"balance error", func(s *uniswapEnumerationTestRPC) { s.failMethod = "balanceOf" }, "execution reverted"},
		{"enumeration error", func(s *uniswapEnumerationTestRPC) { s.failMethod = "tokenOfOwnerByIndex" }, "execution reverted"},
		{"owner error", func(s *uniswapEnumerationTestRPC) { s.failMethod = "ownerOf" }, "execution reverted"},
		{"malformed count", func(s *uniswapEnumerationTestRPC) { s.malformedMethod = "balanceOf" }, "decode"},
		{"malformed ID", func(s *uniswapEnumerationTestRPC) { s.malformedMethod = "tokenOfOwnerByIndex" }, "decode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, state := newUniswapEnumerationTestClient(t, 2)
			test.edit(state)
			nfts, err := enumerateUniswapV3NFTs(context.Background(), client, state.block, state.account, state.manager)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(nfts) != 0 {
				t.Fatalf("inventory=%v err=%v, want no partial inventory and %q", nfts, err, test.want)
			}
		})
	}
}

func TestUniswapV3DiscoveryWithPausedOrHealthyIndexer(t *testing.T) {
	for _, test := range []struct {
		name          string
		healthy       bool
		failPositions bool
		wantError     bool
	}{
		{name: "paused indexer uses pinned inventory"},
		{name: "healthy indexer retains fast path", healthy: true},
		{name: "enumerated position revert is not omitted", failPositions: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, state := newUniswapEnumerationTestClient(t, 2)
			if test.failPositions {
				state.failMethod = "positions"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/status" {
					status := uniswapIndexerTestStatus("12", []ChainID{Ethereum}, state.block.Number)
					if !test.healthy {
						processor := status["processors"].([]map[string]any)[0]
						processor["pause"] = true
						processor["processorStatus"] = map[string]any{"state": "STARTING"}
					}
					_ = json.NewEncoder(w).Encode(status)
					return
				}
				_ = json.NewEncoder(w).Encode(uniswapIndexerTestResponse(state.block.Number, state.block.Timestamp*1000,
					[]map[string]any{uniswapIndexerTestRow(uniswapV3, Ethereum, state.account, big.NewInt(1))}))
			}))
			defer server.Close()
			adapter := &UniswapV3Adapter{indexer: newUniswapIndexerTestClient(server, "12")}
			groups, err := adapter.Positions(context.Background(), client, state.block, state.account)
			if (err != nil) != test.wantError || len(groups) != 0 {
				t.Fatalf("groups=%v err=%v", groups, err)
			}
			if test.healthy && state.methods["tokenOfOwnerByIndex"] != 0 {
				t.Fatal("healthy indexer unexpectedly enumerated NFTs")
			}
			if !test.healthy && state.methods["tokenOfOwnerByIndex"] != 2 {
				t.Fatal("paused indexer did not independently enumerate all NFTs")
			}
		})
	}
}
