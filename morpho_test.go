package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

var morphoTestIRMABI = MustABI(`[
  {"type":"function","name":"borrowRateView","stateMutability":"view","inputs":[
    {"name":"marketParams","type":"tuple","components":[{"name":"loanToken","type":"address"},{"name":"collateralToken","type":"address"},{"name":"oracle","type":"address"},{"name":"irm","type":"address"},{"name":"lltv","type":"uint256"}]},
    {"name":"market","type":"tuple","components":[{"name":"totalSupplyAssets","type":"uint128"},{"name":"totalSupplyShares","type":"uint128"},{"name":"totalBorrowAssets","type":"uint128"},{"name":"totalBorrowShares","type":"uint128"},{"name":"lastUpdate","type":"uint128"},{"name":"fee","type":"uint128"}]}
  ],"outputs":[{"type":"uint256"}]}
]`)

type morphoAccrualFixture struct {
	client          *RPCClient
	marketID        common.Hash
	feeRecipient    common.Address
	borrowRateCalls *atomic.Int64
}

func newMorphoAccrualFixture(
	t *testing.T,
	account common.Address,
	storedSupplyShares *big.Int,
) morphoAccrualFixture {
	t.Helper()
	loanToken := common.HexToAddress("0x0000000000000000000000000000000000000011")
	collateralToken := common.HexToAddress("0x0000000000000000000000000000000000000022")
	oracle := common.HexToAddress("0x0000000000000000000000000000000000000033")
	irm := common.HexToAddress("0x0000000000000000000000000000000000000044")
	feeRecipient := common.HexToAddress("0x00000000000000000000000000000000000000f1")
	storedBorrowShares := big.NewInt(500_000_000)
	if account == feeRecipient {
		storedBorrowShares = new(big.Int)
	}
	lltv := big.NewInt(800_000_000_000_000_000)
	marketID, err := morphoMarketID(loanToken, collateralToken, oracle, irm, lltv)
	if err != nil {
		t.Fatal(err)
	}
	pack := func(contractABIName string, method string, values ...any) []byte {
		t.Helper()
		contractABI := morphoCoreABI
		if contractABIName == "irm" {
			contractABI = morphoTestIRMABI
		}
		encoded, packErr := contractABI.Methods[method].Outputs.Pack(values...)
		if packErr != nil {
			t.Fatalf("pack %s: %v", method, packErr)
		}
		return encoded
	}
	results := map[string][]byte{
		string(morphoCoreABI.Methods["feeRecipient"].ID): pack("core", "feeRecipient", feeRecipient),
		string(morphoCoreABI.Methods["position"].ID): pack(
			"core", "position", storedSupplyShares, storedBorrowShares, big.NewInt(0),
		),
		string(morphoCoreABI.Methods["market"].ID): pack(
			"core", "market",
			big.NewInt(2_000_000_000), big.NewInt(1_000_000_000),
			big.NewInt(1_000_000_000), big.NewInt(500_000_000),
			big.NewInt(100), big.NewInt(100_000_000_000_000_000),
		),
		string(morphoCoreABI.Methods["idToMarketParams"].ID): pack(
			"core", "idToMarketParams", loanToken, collateralToken, oracle, irm, lltv,
		),
		string(morphoTestIRMABI.Methods["borrowRateView"].ID): pack(
			"irm", "borrowRateView", big.NewInt(1_000_000_000_000),
		),
	}
	expectedBorrowRateCall, err := morphoIRMABI.Pack(
		"borrowRateView",
		morphoMarketParams{
			LoanToken: loanToken, CollateralToken: collateralToken,
			Oracle: oracle, Irm: irm, Lltv: lltv,
		},
		morphoMarketState{
			TotalSupplyAssets: big.NewInt(2_000_000_000),
			TotalSupplyShares: big.NewInt(1_000_000_000),
			TotalBorrowAssets: big.NewInt(1_000_000_000),
			TotalBorrowShares: big.NewInt(500_000_000),
			LastUpdate:        big.NewInt(100),
			Fee:               big.NewInt(100_000_000_000_000_000),
		},
	)
	if err != nil {
		t.Fatalf("pack expected borrowRateView call: %v", err)
	}
	borrowRateCalls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var raw json.RawMessage
		if decodeErr := json.NewDecoder(request.Body).Decode(&raw); decodeErr != nil {
			t.Errorf("decode RPC request: %v", decodeErr)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if len(raw) > 0 && raw[0] == '{' {
			var call rpcTestRequest
			if decodeErr := json.Unmarshal(raw, &call); decodeErr != nil {
				t.Errorf("decode RPC call: %v", decodeErr)
				return
			}
			if call.Method != "eth_chainId" {
				t.Errorf("unexpected singleton method %q", call.Method)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"jsonrpc": "2.0", "id": call.ID, "result": "0x1",
			})
			return
		}
		var calls []rpcTestRequest
		if decodeErr := json.Unmarshal(raw, &calls); decodeErr != nil {
			t.Errorf("decode RPC batch: %v", decodeErr)
			return
		}
		responses := make([]map[string]any, len(calls))
		for index, call := range calls {
			var params struct {
				Data hexutil.Bytes `json:"data"`
			}
			if len(call.Params) < 1 {
				t.Errorf("RPC call has no params")
				return
			}
			if decodeErr := json.Unmarshal(call.Params[0], &params); decodeErr != nil {
				t.Errorf("decode eth_call params: %v", decodeErr)
				return
			}
			if len(params.Data) < 4 {
				t.Errorf("eth_call data is too short")
				return
			}
			selector := string(params.Data[:4])
			result, exists := results[selector]
			if !exists {
				t.Errorf("unexpected selector 0x%x", params.Data[:4])
				return
			}
			if selector == string(morphoTestIRMABI.Methods["borrowRateView"].ID) {
				if !bytes.Equal(params.Data, expectedBorrowRateCall) {
					t.Errorf("borrowRateView did not receive the stored market state")
				}
				borrowRateCalls.Add(1)
			}
			responses[index] = map[string]any{
				"jsonrpc": "2.0", "id": call.ID, "result": "0x" + common.Bytes2Hex(result),
			}
		}
		_ = json.NewEncoder(writer).Encode(responses)
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return morphoAccrualFixture{
		client: client, marketID: marketID, feeRecipient: feeRecipient, borrowRateCalls: borrowRateCalls,
	}
}

func TestMorphoDeploymentsCoverSupportedChains(t *testing.T) {
	for _, chainID := range deploymentChains(morphoDeployments) {
		deployment, exists := morphoDeployments[chainID]
		if !exists {
			t.Fatalf("Morpho deployment is absent for chain %d", chainID)
		}
		if deployment.Morpho == (common.Address{}) || deployment.Window.ActivationBlock == 0 {
			t.Errorf("chain %d has an incomplete Morpho core deployment", chainID)
		}
		if len(deployment.VaultV1Factories)+len(deployment.VaultV2Factories) == 0 {
			t.Errorf("chain %d has no Morpho vault factory", chainID)
		}
		for _, factory := range append(deployment.VaultV1Factories, deployment.VaultV2Factories...) {
			if factory.Address == (common.Address{}) || factory.Window.ActivationBlock < deployment.Window.ActivationBlock {
				t.Errorf("chain %d has an invalid Morpho factory: %+v", chainID, factory)
			}
		}
	}
}

func TestMorphoCanonicalMarketID(t *testing.T) {
	marketID, err := morphoMarketID(
		common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		common.HexToAddress("0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599"),
		common.HexToAddress("0xDddd770BADd886dF3864029e4B377B5F6a2B6b83"),
		common.HexToAddress("0x870aC11D48B15DB9a138Cf899d20F13F79Ba00BC"),
		big.NewInt(860_000_000_000_000_000),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := common.HexToHash("0x3a85e619751152991742810df6ec69ce473daef99e28a64ab2340d7b7ccfee49")
	if marketID != want {
		t.Fatalf("market ID = %s, want %s", marketID, want)
	}
}

func TestMorphoShareFractionMatchesDeBankBoundary(t *testing.T) {
	shares, ok := new(big.Int).SetString("1660150005780932", 10)
	if !ok {
		t.Fatal("invalid shares fixture")
	}
	totalAssets, ok := new(big.Int).SetString("106321198846013", 10)
	if !ok {
		t.Fatal("invalid total assets fixture")
	}
	totalShares, ok := new(big.Int).SetString("91830935606491379162", 10)
	if !ok {
		t.Fatal("invalid total shares fixture")
	}
	numerator, denominator := morphoShareFraction(
		shares,
		totalAssets,
		totalShares,
	)
	wantNumerator := new(big.Int).Mul(
		shares,
		new(big.Int).Add(totalAssets, big.NewInt(1)),
	)
	wantDenominator := new(big.Int).Add(totalShares, big.NewInt(1_000_000))
	if numerator.Cmp(wantNumerator) != 0 || denominator.Cmp(wantDenominator) != 0 {
		t.Fatalf("fraction = %s/%s, want %s/%s", numerator, denominator, wantNumerator, wantDenominator)
	}
}

func TestMorphoPolygonPinnedDebtAccrualMatchesProtocolMath(t *testing.T) {
	// These values are fixed canonical state from Polygon block 93,225,488. Morpho's
	// market() totals are stored before pending interest, so a position at the pinned
	// timestamp must first use expectedMarketBalances and only then convert borrow shares.
	block := BlockRef{
		ChainID: Polygon, Number: 93_225_488, Timestamp: 1_788_543_265, Fixed: true,
	}
	state := morphoMarketState{
		TotalSupplyAssets: big.NewInt(40_446_419_357),
		TotalSupplyShares: big.NewInt(38_414_787_573_112_953),
		TotalBorrowAssets: big.NewInt(30_143_460_275),
		TotalBorrowShares: big.NewInt(28_431_000_979_763_039),
		LastUpdate:        big.NewInt(1_788_461_172),
		Fee:               new(big.Int),
	}
	if block.ChainID != Polygon || block.Number != 93_225_488 ||
		block.Timestamp != 1_788_543_265 || !block.Fixed {
		t.Fatalf("unexpected pinned block: %+v", block)
	}
	if elapsed := block.Timestamp - state.LastUpdate.Uint64(); elapsed != 82_093 {
		t.Fatalf("elapsed = %d, want 82093", elapsed)
	}

	expected, pendingFeeShares := morphoExpectedMarketBalances(
		state,
		big.NewInt(1_375_896_501),
		block.Timestamp-state.LastUpdate.Uint64(),
	)
	if expected.TotalBorrowAssets.String() != "30146865215" {
		t.Fatalf("expected total borrow assets = %s, want 30146865215", expected.TotalBorrowAssets)
	}
	if expected.TotalBorrowShares.Cmp(state.TotalBorrowShares) != 0 {
		t.Fatalf(
			"expected total borrow shares = %s, want %s",
			expected.TotalBorrowShares,
			state.TotalBorrowShares,
		)
	}
	if pendingFeeShares.Sign() != 0 {
		t.Fatalf("pending fee shares = %s, want 0", pendingFeeShares)
	}

	borrowShares := big.NewInt(1_477_422_818_746_994)
	numerator, denominator := morphoShareFraction(
		borrowShares,
		expected.TotalBorrowAssets,
		expected.TotalBorrowShares,
	)
	if numerator.String() != "44539666583808426123160704" {
		t.Fatalf(
			"expected debt numerator = %s, want 44539666583808426123160704",
			numerator,
		)
	}
	if denominator.String() != "28431000980763039" {
		t.Fatalf("expected debt denominator = %s, want 28431000980763039", denominator)
	}
	if roundedUp := morphoMulDivUp(
		borrowShares,
		new(big.Int).Add(expected.TotalBorrowAssets, big.NewInt(1)),
		new(big.Int).Add(expected.TotalBorrowShares, big.NewInt(1_000_000)),
	); roundedUp.String() != "1566588057" {
		t.Fatalf("expected debt rounded up = %s, want 1566588057", roundedUp)
	}
}

func TestMorphoCoreAccruesMarketToPinnedTimestamp(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	fixture := newMorphoAccrualFixture(t, account, big.NewInt(1_000_000_000))
	positions, err := readMorphoCorePositions(
		context.Background(),
		fixture.client,
		BlockRef{ChainID: Ethereum, Number: 123, Timestamp: 1_100, Fixed: true},
		account,
		morphoDeployment{Morpho: morphoDeployments[Ethereum].Morpho},
		[]common.Hash{fixture.marketID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.borrowRateCalls.Load() != 1 {
		t.Fatalf("borrowRateView calls = %d, want 1", fixture.borrowRateCalls.Load())
	}
	if len(positions) != 1 {
		t.Fatalf("positions = %d, want 1", len(positions))
	}
	if positions[0].TotalBorrowAssets.String() != "1001000500" {
		t.Fatalf("total borrow assets = %s, want 1001000500", positions[0].TotalBorrowAssets)
	}
	if positions[0].TotalSupplyAssets.String() != "2001000500" {
		t.Fatalf("total supply assets = %s, want 2001000500", positions[0].TotalSupplyAssets)
	}
	if positions[0].TotalSupplyShares.String() != "1000050052" {
		t.Fatalf("total supply shares = %s, want 1000050052", positions[0].TotalSupplyShares)
	}
}

func TestMorphoCoreCreditsPendingFeeSharesToRecipient(t *testing.T) {
	feeRecipient := common.HexToAddress("0x00000000000000000000000000000000000000f1")
	fixture := newMorphoAccrualFixture(t, feeRecipient, new(big.Int))
	positions, err := readMorphoCorePositions(
		context.Background(),
		fixture.client,
		BlockRef{ChainID: Ethereum, Number: 123, Timestamp: 1_100, Fixed: true},
		feeRecipient,
		morphoDeployment{Morpho: morphoDeployments[Ethereum].Morpho},
		[]common.Hash{fixture.marketID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 {
		t.Fatalf("fee recipient positions = %d, want 1", len(positions))
	}
	if positions[0].SupplyShares.String() != "50052" {
		t.Fatalf("fee recipient supply shares = %s, want 50052", positions[0].SupplyShares)
	}
}

func TestMergeMorphoRefsRejectsGenerationDrift(t *testing.T) {
	vault := common.HexToAddress("0x1111111111111111111111111111111111111111")
	_, err := mergeMorphoRefs(
		morphoPositionRefs{Vaults: []morphoVaultRef{{Address: vault, Version: morphoVaultV1}}},
		nil,
		[]morphoVaultRef{{Address: vault, Version: morphoVaultV2}},
	)
	if err == nil {
		t.Fatal("expected generation mismatch")
	}
}

func TestMorphoTailFiltersMatchIndexedAccountTopics(t *testing.T) {
	wantTopic2 := map[common.Hash]struct{}{
		crypto.Keccak256Hash([]byte("Withdraw(bytes32,address,address,address,uint256,uint256)")):   {},
		crypto.Keccak256Hash([]byte("Borrow(bytes32,address,address,address,uint256,uint256)")):     {},
		crypto.Keccak256Hash([]byte("WithdrawCollateral(bytes32,address,address,address,uint256)")): {},
	}
	wantTopic3 := map[common.Hash]struct{}{
		crypto.Keccak256Hash([]byte("Supply(bytes32,address,address,uint256,uint256)")):                            {},
		crypto.Keccak256Hash([]byte("Repay(bytes32,address,address,uint256,uint256)")):                             {},
		crypto.Keccak256Hash([]byte("SupplyCollateral(bytes32,address,address,uint256)")):                          {},
		crypto.Keccak256Hash([]byte("Liquidate(bytes32,address,address,uint256,uint256,uint256,uint256,uint256)")): {},
	}
	assertTopics := func(name string, actual []common.Hash, expected map[common.Hash]struct{}) {
		t.Helper()
		if len(actual) != len(expected) {
			t.Fatalf("%s filters = %d, want %d", name, len(actual), len(expected))
		}
		for _, topic := range actual {
			if _, exists := expected[topic]; !exists {
				t.Fatalf("%s contains unexpected topic %s", name, topic)
			}
		}
	}
	assertTopics("topic2", morphoTopic2AccountEvents, wantTopic2)
	assertTopics("topic3", morphoTopic3AccountEvents, wantTopic3)
}

func TestMorphoFeeRecipientTailReplaysEventOrder(t *testing.T) {
	alice := common.HexToAddress("0x1111111111111111111111111111111111111111")
	bob := common.HexToAddress("0x2222222222222222222222222222222222222222")
	marketBeforeChange := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	marketForBob := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	marketWithoutFee := common.HexToHash("0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	marketAfterRestore := common.HexToHash("0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	accrue := func(block uint64, index uint64, market common.Hash, feeShares int64) rpcLog {
		data := make([]byte, 96)
		copy(data[64:], common.LeftPadBytes(big.NewInt(feeShares).Bytes(), 32))
		return rpcLog{
			Topics: []common.Hash{morphoAccrueInterestTopic, market}, Data: hexutil.Bytes(data),
			BlockNumber: hexutil.Uint64(block), LogIndex: hexutil.Uint64(index),
		}
	}
	setRecipient := func(block uint64, index uint64, recipient common.Address) rpcLog {
		return rpcLog{
			Topics:      []common.Hash{morphoSetFeeRecipientTopic, common.BytesToHash(recipient.Bytes())},
			BlockNumber: hexutil.Uint64(block), LogIndex: hexutil.Uint64(index),
		}
	}
	markets, err := morphoFeeMarketIDsFromLogs(alice, alice, []rpcLog{
		accrue(12, 2, marketAfterRestore, 2),
		setRecipient(11, 0, bob),
		accrue(10, 0, marketBeforeChange, 1),
		accrue(11, 1, marketForBob, 1),
		setRecipient(12, 0, alice),
		accrue(12, 1, marketWithoutFee, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []common.Hash{marketBeforeChange, marketAfterRestore}
	if len(markets) != len(want) || markets[0] != want[0] || markets[1] != want[1] {
		t.Fatalf("fee markets = %v, want %v", markets, want)
	}
}

func TestMorphoPositiveSetFeeTailDiscoversMarketBeforeAccrual(t *testing.T) {
	marketWithFee := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	marketWithoutFee := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	setFee := func(block uint64, index uint64, market common.Hash, fee int64) rpcLog {
		return rpcLog{
			Topics:      []common.Hash{morphoSetFeeTopic, market},
			Data:        common.LeftPadBytes(big.NewInt(fee).Bytes(), 32),
			BlockNumber: hexutil.Uint64(block),
			LogIndex:    hexutil.Uint64(index),
		}
	}
	markets, err := morphoPositiveSetFeeMarketIDsFromLogs([]rpcLog{
		setFee(10, 0, marketWithFee, 100_000_000_000_000_000),
		setFee(11, 0, marketWithoutFee, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(markets) != 1 || markets[0] != marketWithFee {
		t.Fatalf("positive fee markets = %v, want [%s]", markets, marketWithFee)
	}
}

func TestMorphoFeeMarketGraphQLKeepsOnlyActiveFees(t *testing.T) {
	active := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	inactive := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
			return
		}
		if body.Variables["block"] != "123" || body.Variables["chainId"] != float64(1) {
			t.Errorf("unexpected GraphQL variables: %#v", body.Variables)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"data": map[string]any{
				"morphoFeeMarkets": []map[string]any{
					{"id": "1:" + strings.ToLower(active.Hex()), "chainId": 1, "marketId": strings.ToLower(active.Hex()), "fee": "100000000000000000"},
					{"id": "1:" + strings.ToLower(inactive.Hex()), "chainId": 1, "marketId": strings.ToLower(inactive.Hex()), "fee": "0"},
				},
			},
		})
	}))
	t.Cleanup(server.Close)
	indexer := &morphoIndexer{
		api: &sentioAPIClient{
			apiKey: "test", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache),
		},
		config: SentioIndexerConfig{GraphQLURL: server.URL},
	}
	page, err := indexer.graphqlFeeMarketPage(
		context.Background(), Ethereum, morphoFeeMarketRowPrefix(Ethereum), 123,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.RowIDs) != 2 {
		t.Fatalf("fee-market rows = %d, want 2", len(page.RowIDs))
	}
	if len(page.ActiveMarketIDs) != 1 || page.ActiveMarketIDs[0] != active {
		t.Fatalf("active fee markets = %v, want [%s]", page.ActiveMarketIDs, active)
	}
}
