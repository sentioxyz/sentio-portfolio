package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

var uniswapIndexerTestOwner = common.HexToAddress("0xaAaAaAaaAaAaAaaAaAAAAAAAAaaaAaAaAaaAaaAa")

func uniswapIndexerTestRow(
	version uniswapGeneration,
	chainID ChainID,
	owner common.Address,
	tokenID *big.Int,
) map[string]any {
	manager, exists := uniswapExpectedManager(version, chainID)
	if !exists {
		panic("test requested an undeployed Uniswap generation")
	}
	return map[string]any{
		"id":      uniswapPositionRowID(chainID, owner, manager, tokenID),
		"chainId": int(chainID),
		"owner":   strings.ToLower(owner.Hex()),
		"manager": strings.ToLower(manager.Hex()),
		"tokenId": tokenID.String(),
		"balance": "1",
	}
}

func uniswapIndexerTestResponse(
	checkpointBlock uint64,
	checkpointMS uint64,
	rows []map[string]any,
) map[string]any {
	return map[string]any{
		"data": map[string]any{
			"indexerCheckpoints": []map[string]any{{
				"blockNumber": strconv.FormatUint(checkpointBlock, 10),
				"timestampMs": strconv.FormatUint(checkpointMS, 10),
			}},
			"positions": rows,
		},
	}
}

func uniswapIndexerTestStatus(version string, chains []ChainID, block uint64) map[string]any {
	states := make([]map[string]any, 0, len(chains))
	for _, chainID := range chains {
		states = append(states, map[string]any{
			"chainId":                    strconv.FormatUint(uint64(chainID), 10),
			"processedBlockNumber":       strconv.FormatUint(block, 10),
			"estimatedLatestBlockNumber": strconv.FormatUint(block, 10),
			"status": map[string]any{
				"state":       "PROCESSING_LATEST",
				"errorRecord": map[string]any{"message": ""},
			},
		})
	}
	return map[string]any{
		"processors": []map[string]any{{
			"version":         version,
			"versionState":    "PENDING",
			"processorStatus": map[string]any{"state": "PROCESSING"},
			"states":          states,
		}},
	}
}

func newUniswapIndexerTestClient(server *httptest.Server, version string) *uniswapIndexer {
	config := SentioIndexerConfig{
		GraphQLURL:       server.URL + "/graphql",
		StatusURL:        server.URL + "/status",
		ProcessorVersion: version,
	}
	indexer := newUniswapIndexer(config, config)
	api := &sentioAPIClient{
		apiKey:     "test",
		httpClient: server.Client(),
		statuses:   make(map[string]sentioStatusCache),
	}
	for _, index := range indexer.indexes {
		index.api = api
	}
	return indexer
}

// A Uniswap read asks for the live NFTs, those with a balance, at the block every other index read
// asks at: the processor's last block when it trails the pin.
func TestUniswapIndexerReadsLivePositionsAtTheQueryBlock(t *testing.T) {
	prefix := fmt.Sprintf("%d:%s:", Ethereum, strings.ToLower(uniswapIndexerTestOwner.Hex()))
	row := uniswapIndexerTestRow(uniswapV3, Ethereum, uniswapIndexerTestOwner, big.NewInt(42))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("1", deploymentChains(uniswapV3Deployments), 120))
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
			return
		}
		if strings.Count(body.Query, "block: { number: 120 }") != 2 {
			t.Errorf("query does not pin both checkpoint and positions at the query block: %s", body.Query)
		}
		where := `balance_not: 0, id_starts_with: "` + prefix + `"`
		if !strings.Contains(body.Query, where) {
			t.Errorf("query does not select %s: %s", where, body.Query)
		}
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestResponse(120, 999_000, []map[string]any{row}))
	}))
	t.Cleanup(server.Close)

	indexer := newUniswapIndexerTestClient(server, "1")
	indexed, err := indexer.indexedNFTs(
		context.Background(), uniswapV3, BlockRef{ChainID: Ethereum, Number: 123, Timestamp: 1_000}, uniswapIndexerTestOwner,
	)
	if err != nil {
		t.Fatal(err)
	}
	if indexed.IndexerBlock != 120 || len(indexed.NFTs) != 1 || indexed.NFTs[0].TokenID.Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("indexed = %+v, want token 42 read at block 120", indexed)
	}
}

func TestUniswapIndexerRejectsMalformedOrNonUnitRows(t *testing.T) {
	manager, _ := uniswapExpectedManager(uniswapV3, Ethereum)
	canonicalManager := strings.ToLower(manager.Hex())
	canonicalOwner := strings.ToLower(uniswapIndexerTestOwner.Hex())
	uint256Overflow := new(big.Int).Lsh(big.NewInt(1), 256).String()

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong chain", mutate: func(row map[string]any) { row["chainId"] = 10 }},
		{name: "wrong owner", mutate: func(row map[string]any) { row["owner"] = strings.Repeat("0", 42) }},
		{name: "noncanonical owner", mutate: func(row map[string]any) { row["owner"] = strings.ToUpper(canonicalOwner) }},
		{name: "invalid manager", mutate: func(row map[string]any) { row["manager"] = "not-an-address" }},
		{name: "noncanonical manager", mutate: func(row map[string]any) { row["manager"] = strings.ToUpper(canonicalManager) }},
		{name: "foreign manager", mutate: func(row map[string]any) {
			foreign := common.HexToAddress("0x1111111111111111111111111111111111111111")
			row["manager"] = strings.ToLower(foreign.Hex())
			row["id"] = uniswapPositionRowID(Ethereum, uniswapIndexerTestOwner, foreign, big.NewInt(42))
		}},
		{name: "negative token ID", mutate: func(row map[string]any) { row["tokenId"] = "-1" }},
		{name: "nondecimal token ID", mutate: func(row map[string]any) { row["tokenId"] = "0x2a" }},
		{name: "noncanonical token ID", mutate: func(row map[string]any) { row["tokenId"] = "042" }},
		{name: "token ID exceeds uint256", mutate: func(row map[string]any) {
			row["tokenId"] = uint256Overflow
			row["id"] = fmt.Sprintf("%d:%s:%s:%s", Ethereum, canonicalOwner, canonicalManager, uint256Overflow)
		}},
		{name: "ID owner mismatch", mutate: func(row map[string]any) {
			row["id"] = fmt.Sprintf("%d:%s:%s:42", Ethereum, strings.Repeat("1", 42), canonicalManager)
		}},
		{name: "ID manager mismatch", mutate: func(row map[string]any) {
			row["id"] = fmt.Sprintf("%d:%s:%s:42", Ethereum, canonicalOwner, strings.Repeat("1", 42))
		}},
		{name: "ID token mismatch", mutate: func(row map[string]any) {
			row["id"] = fmt.Sprintf("%d:%s:%s:43", Ethereum, canonicalOwner, canonicalManager)
		}},
		{name: "zero balance", mutate: func(row map[string]any) { row["balance"] = "0" }},
		{name: "balance above one", mutate: func(row map[string]any) { row["balance"] = "2" }},
		{name: "negative balance", mutate: func(row map[string]any) { row["balance"] = "-1" }},
		{name: "noncanonical balance", mutate: func(row map[string]any) { row["balance"] = "01" }},
		{name: "noninteger balance", mutate: func(row map[string]any) { row["balance"] = "1.0" }},
	}

	stream := uniswapPositionStream(uniswapIndexerDefinitions[uniswapV3], uniswapIndexerTestOwner)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			row := uniswapIndexerTestRow(uniswapV3, Ethereum, uniswapIndexerTestOwner, big.NewInt(42))
			test.mutate(row)
			encoded, _ := json.Marshal(row)
			var decoded uniswapPositionRow
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Decode(Ethereum, row["id"].(string), decoded); err == nil {
				t.Fatal("malformed GraphQL row was accepted")
			}
		})
	}
}

// A generation reads no more NFTs than it may hold: past its cap the read fails rather than reading
// on, having asked for one row past the cap.
func TestUniswapIndexerEnforcesGenerationCap(t *testing.T) {
	const version = "18"
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus(
				version,
				deploymentChains(uniswapV4Deployments),
				100,
			))
			return
		}
		requestCount++
		start, count := int64(100_000), int64(500)
		if requestCount == 2 {
			start, count = 100_500, 13
		} else if requestCount != 1 {
			t.Errorf("unexpected GraphQL request %d", requestCount)
		}
		rows := make([]map[string]any, 0, count)
		for offset := int64(0); offset < count; offset++ {
			rows = append(rows, uniswapIndexerTestRow(
				uniswapV4,
				Ethereum,
				uniswapIndexerTestOwner,
				big.NewInt(start+offset),
			))
		}
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestResponse(100, 1_000_000_000, rows))
	}))
	t.Cleanup(server.Close)

	indexer := newUniswapIndexerTestClient(server, version)
	_, err := indexer.indexedNFTs(
		context.Background(),
		uniswapV4,
		BlockRef{ChainID: Ethereum, Number: 100, Timestamp: 1_000_000},
		uniswapIndexerTestOwner,
	)
	if err == nil || !strings.Contains(err.Error(), "more than 512 positions rows") {
		t.Fatalf("cap error = %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("GraphQL requests = %d, want 2", requestCount)
	}
}

func mustUniswapTestManager(
	t *testing.T,
	version uniswapGeneration,
	chainID ChainID,
) common.Address {
	t.Helper()
	manager, exists := uniswapExpectedManager(version, chainID)
	if !exists {
		t.Fatalf("Uniswap %s is not deployed on chain %d", version, chainID)
	}
	return manager
}
