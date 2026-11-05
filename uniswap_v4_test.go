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

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestUniswapV4ActivePositionClassification(t *testing.T) {
	nfts := func(count int) []uniswapIndexedNFT {
		result := make([]uniswapIndexedNFT, count)
		for index := range result {
			result[index] = uniswapIndexedNFT{TokenID: big.NewInt(int64(index + 1))}
		}
		return result
	}
	liquidity := func(value int64) ContractCallResult {
		return ContractCallResult{Values: []any{big.NewInt(value)}}
	}
	revert := ContractCallResult{Error: errors.New("execution reverted")}
	transport := ContractCallResult{Error: errors.New("connection refused")}

	t.Run("closed and burned NFTs are dropped", func(t *testing.T) {
		active, err := uniswapV4ActivePositions(
			nfts(4),
			[]ContractCallResult{liquidity(0), liquidity(7), revert, liquidity(9)},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 2 ||
			active[0].NFT.TokenID.Int64() != 2 || active[0].ManagerLiquidity.Int64() != 7 ||
			active[1].NFT.TokenID.Int64() != 4 || active[1].ManagerLiquidity.Int64() != 9 {
			t.Fatalf("active positions = %+v, want tokens 2 and 4 with their liquidity", active)
		}
	})

	t.Run("closed NFTs do not count toward the active cap", func(t *testing.T) {
		maximum := uniswapIndexerDefinitions[uniswapV4].maxIndexedNFTs
		rows := make([]ContractCallResult, maximum)
		for index := range rows {
			rows[index] = liquidity(0)
			if index%(maximum/uniswapMaxActivePositions) == 0 {
				rows[index] = liquidity(1)
			}
		}
		active, err := uniswapV4ActivePositions(nfts(maximum), rows)
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != uniswapMaxActivePositions {
			t.Fatalf("got %d active positions, want %d", len(active), uniswapMaxActivePositions)
		}
	})

	t.Run("more active positions than the cap fail", func(t *testing.T) {
		rows := make([]ContractCallResult, uniswapMaxActivePositions+1)
		for index := range rows {
			rows[index] = liquidity(1)
		}
		_, err := uniswapV4ActivePositions(nfts(len(rows)), rows)
		want := fmt.Sprintf("more than %d economically active Uniswap v4 positions", uniswapMaxActivePositions)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("cap error = %v, want %q", err, want)
		}
	})

	t.Run("transport errors and malformed rows fail the scan", func(t *testing.T) {
		if _, err := uniswapV4ActivePositions(nfts(1), []ContractCallResult{transport}); err == nil {
			t.Fatal("transport-level liquidity error was swallowed")
		}
		if _, err := uniswapV4ActivePositions(nfts(1), []ContractCallResult{{Values: []any{}}}); err == nil {
			t.Fatal("malformed liquidity row was accepted")
		}
		if _, err := uniswapV4ActivePositions(nfts(2), []ContractCallResult{liquidity(1)}); err == nil {
			t.Fatal("result count mismatch was accepted")
		}
	})
}

// uniswapV4TestRPC answers the position manager, the StateView and the pool's two tokens at one
// pinned block. Every NFT has no liquidity unless listed, and is the account's unless listed.
type uniswapV4TestRPC struct {
	mu         sync.Mutex
	block      BlockRef
	deployment uniswapV4Deployment
	account    common.Address
	poolKey    uniswapV4PoolKey
	poolID     common.Hash
	liquidity  map[int64]int64
	burned     map[int64]bool
	owners     map[int64]common.Address
	symbols    map[common.Address]string
	calls      map[string]int
}

func (s *uniswapV4TestRPC) Call(_ context.Context, args map[string]string, block string) (hexutil.Bytes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if block != hexutil.EncodeUint64(s.block.Number) {
		return nil, fmt.Errorf("unpinned block %s", block)
	}
	data, err := hexutil.Decode(args["data"])
	if err != nil || len(data) < 4 {
		return nil, fmt.Errorf("invalid calldata")
	}
	to := common.HexToAddress(args["to"])
	var contract abi.ABI
	switch {
	case to == s.deployment.Manager:
		contract = uniswapV4PositionManagerABI
	case to == s.deployment.StateView:
		contract = uniswapV4StateViewABI
	case s.symbols[to] != "":
		contract = erc20ABI
	default:
		return nil, fmt.Errorf("unexpected contract %s", to)
	}
	method, err := contract.MethodById(data[:4])
	if err != nil {
		return nil, err
	}
	s.calls[method.Name]++
	values, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, err
	}
	tokenID := func(value any) int64 { return value.(*big.Int).Int64() }
	switch method.Name {
	case "getPositionLiquidity":
		id := tokenID(values[0])
		if s.burned[id] {
			return nil, fmt.Errorf("execution reverted")
		}
		return method.Outputs.Pack(big.NewInt(s.liquidity[id]))
	case "ownerOf":
		owner, exists := s.owners[tokenID(values[0])]
		if !exists {
			owner = s.account
		}
		return method.Outputs.Pack(owner)
	case "getPoolAndPositionInfo":
		if s.liquidity[tokenID(values[0])] == 0 {
			return nil, fmt.Errorf("pool of closed NFT %d was read", tokenID(values[0]))
		}
		// tickUpper at bits 32-55, tickLower at bits 8-31, as uniswapDecodePackedInt24 reads them.
		packed := new(big.Int).Lsh(big.NewInt(60), 32)
		packed.Or(packed, new(big.Int).Lsh(big.NewInt(1<<24-60), 8))
		return method.Outputs.Pack(s.poolKey, packed)
	case "getSlot0":
		if common.Hash(values[0].([32]byte)) != s.poolID {
			return nil, fmt.Errorf("unexpected pool ID")
		}
		return method.Outputs.Pack(new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(0), big.NewInt(0), big.NewInt(3_000))
	case "getPositionInfo":
		if values[1].(common.Address) != s.deployment.Manager ||
			values[2].(*big.Int).Int64() != -60 || values[3].(*big.Int).Int64() != 60 {
			return nil, fmt.Errorf("unexpected position key")
		}
		salt := values[4].([32]byte)
		id := new(big.Int).SetBytes(salt[:]).Int64()
		return method.Outputs.Pack(big.NewInt(s.liquidity[id]), big.NewInt(0), big.NewInt(0))
	case "getFeeGrowthInside":
		// One unit of fee growth per unit of liquidity in token0, none in token1.
		return method.Outputs.Pack(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(0))
	case "symbol":
		return method.Outputs.Pack(s.symbols[to])
	case "decimals":
		return method.Outputs.Pack(uint8(18))
	default:
		return nil, fmt.Errorf("unexpected method %s", method.Name)
	}
}

// An account that keeps far more closed V4 NFTs than it has open positions is still valued: the
// index lists every NFT it holds, only those with liquidity are read further, and the groups are
// exactly the open positions it owns.
func TestUniswapV4ValuesAccountHoldingManyClosedNFTs(t *testing.T) {
	const (
		processorVersion = "7"
		indexedNFTs      = 1_349
		firstTokenID     = 100_000
		blockNumber      = 22_000_000
	)
	block := BlockRef{ChainID: Ethereum, Number: blockNumber, Timestamp: 1_800_000_000}
	deployment := uniswapV4Deployments[Ethereum]
	token0 := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	token1 := common.HexToAddress("0x00000000000000000000000000000000000000a2")
	poolKey := uniswapV4PoolKey{
		Currency0: token0, Currency1: token1,
		Fee: big.NewInt(3_000), TickSpacing: big.NewInt(60),
	}
	poolID, err := uniswapV4PoolID(poolKey)
	if err != nil {
		t.Fatal(err)
	}
	state := &uniswapV4TestRPC{
		block: block, deployment: deployment, account: uniswapIndexerTestOwner,
		poolKey: poolKey, poolID: poolID,
		liquidity: map[int64]int64{
			100_003: 1_000_000, 100_700: 2_000_000, 100_900: 5_000_000, 101_200: 3_000_000,
		},
		burned:  map[int64]bool{100_010: true},
		owners:  map[int64]common.Address{100_900: common.HexToAddress("0x2")},
		symbols: map[common.Address]string{token0: "AAA", token1: "BBB"},
		calls:   make(map[string]int),
	}
	rpcServer := rpc.NewServer()
	if err := rpcServer.RegisterName("eth", state); err != nil {
		t.Fatal(err)
	}
	client := &RPCClient{chainID: Ethereum, client: rpc.DialInProc(rpcServer), transport: &http.Transport{}}
	t.Cleanup(func() { client.Close(); rpcServer.Stop() })

	requestCount := 0
	indexServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus(
				processorVersion, deploymentChains(uniswapV4Deployments), blockNumber,
			))
			return
		}
		requestCount++
		served := (requestCount - 1) * indexPageSize
		count := max(0, min(indexPageSize, indexedNFTs-served))
		rows := make([]map[string]any, 0, count)
		for offset := range count {
			rows = append(rows, uniswapIndexerTestRow(
				uniswapV4, Ethereum, uniswapIndexerTestOwner, big.NewInt(int64(firstTokenID+served+offset)),
			))
		}
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestResponse(
			blockNumber, block.Timestamp*1_000, rows,
		))
	}))
	t.Cleanup(indexServer.Close)

	adapter := &UniswapV4Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "uniswap-v4", Name: "Uniswap V4", Chains: deploymentChains(uniswapV4Deployments),
		}},
		indexer: newUniswapIndexerTestClient(indexServer, processorVersion),
	}
	groups, err := adapter.Positions(context.Background(), client, block, uniswapIndexerTestOwner)
	if err != nil {
		t.Fatal(err)
	}

	wantIDs := []string{"nft:100003", "nft:100700", "nft:101200"}
	if len(groups) != len(wantIDs) {
		t.Fatalf("got %d groups, want %d", len(groups), len(wantIDs))
	}
	for index, group := range groups {
		if group.ID != wantIDs[index] {
			t.Fatalf("group %d = %s, want %s", index, group.ID, wantIDs[index])
		}
		roles := make(map[string]string)
		for _, component := range group.Components {
			roles[component.Metadata["role"].(string)+":"+component.Token.Symbol] = component.AmountRaw
		}
		liquidity := group.Metadata["liquidity"].(string)
		if roles["principal:AAA"] == "" || roles["principal:BBB"] == "" ||
			roles["uncollected-fee:AAA"] != liquidity || roles["uncollected-fee:BBB"] != "" {
			t.Fatalf("group %s components = %v, want both principals and a token0 fee of %s", group.ID, roles, liquidity)
		}
	}

	wantCalls := map[string]int{
		"getPositionLiquidity":   indexedNFTs, // every indexed NFT, once
		"ownerOf":                4,           // only those with liquidity
		"getPoolAndPositionInfo": 3,           // only open positions the account owns
		"getSlot0":               3,
		"getPositionInfo":        3,
		"getFeeGrowthInside":     3,
	}
	for method, want := range wantCalls {
		if state.calls[method] != want {
			t.Errorf("%s calls = %d, want %d", method, state.calls[method], want)
		}
	}
	if wantRequests := (indexedNFTs + indexPageSize - 1) / indexPageSize; requestCount != wantRequests {
		t.Errorf("GraphQL requests = %d, want %d", requestCount, wantRequests)
	}
}
