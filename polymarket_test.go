package portfolio

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

func polymarketTestBig(t *testing.T, value string) *big.Int {
	t.Helper()
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid integer %q", value)
	}
	return parsed
}

// The collection and position ids below are the contract's own answers on Polygon, for one
// standard market (USDC.e collateral) and one neg-risk market (the wrapped collateral).
func TestPolymarketCollectionIDMatchesTheContract(t *testing.T) {
	for _, test := range []struct {
		name        string
		conditionID string
		outcome     int
		collection  string
		tokenID     string
		negRisk     bool
	}{
		{
			name:        "standard outcome 0",
			conditionID: "0x9b5ff439175bd0bd854e0f7ea3a9c370ca9316d46c016d879b2d702300a84764", outcome: 0,
			collection: "0x1316a14c1c1e87f6aae181aee3357f373c9e6fd4d0b7379aa98cc17301ec67c1",
			tokenID:    "48177841782615845297800795425780987686345090974891807130897417179241596256598",
		},
		{
			name:        "standard outcome 1",
			conditionID: "0x9b5ff439175bd0bd854e0f7ea3a9c370ca9316d46c016d879b2d702300a84764", outcome: 1,
			collection: "0x2e53c10a26d1718c3952e2079f43e0dd7f479f0c3f36c249c04ece4483567106",
			tokenID:    "88292521707896238366488437877864855199540950428102684804712384070924114141331",
		},
		{
			name:        "neg-risk outcome 1",
			conditionID: "0x86e8f678e6cebb9596ffbb5a736b2ff9f36b12905d5b65b1556329c28533c7a1", outcome: 1,
			collection: "0x1b01037fbe7a59e29ddc2365377952b50d70c84a12a2b00c783e7e086523675b",
			tokenID:    "15595081936246780886940656289399636714509677762753928040257148545998496082698",
			negRisk:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conditionID := common.HexToHash(test.conditionID)
			indexSet := int64(1) << test.outcome
			collection := polymarketCollectionID(conditionID, big.NewInt(indexSet))
			if got := common.Hash(collection).Hex(); got != test.collection {
				t.Fatalf("collection = %s, want %s", got, test.collection)
			}
			id := polymarketTestBig(t, test.tokenID)
			negRisk, err := polymarketVerifyToken(id, polymarketToken{
				ID: id, ConditionID: conditionID, OutcomeIndex: test.outcome, IndexSet: indexSet,
			})
			if err != nil {
				t.Fatal(err)
			}
			if negRisk != test.negRisk {
				t.Fatalf("negRisk = %v, want %v", negRisk, test.negRisk)
			}
		})
	}
}

// The index names a token's condition and outcome; the token's id decides whether that is true.
func TestPolymarketVerifyTokenRejectsAMisdescribedToken(t *testing.T) {
	id := polymarketTestBig(t, "48177841782615845297800795425780987686345090974891807130897417179241596256598")
	condition := common.HexToHash("0x9b5ff439175bd0bd854e0f7ea3a9c370ca9316d46c016d879b2d702300a84764")
	for _, test := range []struct {
		name  string
		token polymarketToken
	}{
		{"the other outcome", polymarketToken{ConditionID: condition, OutcomeIndex: 1, IndexSet: 2}},
		{"another condition", polymarketToken{
			ConditionID:  common.HexToHash("0x86e8f678e6cebb9596ffbb5a736b2ff9f36b12905d5b65b1556329c28533c7a1"),
			OutcomeIndex: 0, IndexSet: 1,
		}},
		{"an index set that is not the outcome's", polymarketToken{ConditionID: condition, OutcomeIndex: 0, IndexSet: 3}},
		{"a negative outcome", polymarketToken{ConditionID: condition, OutcomeIndex: -1, IndexSet: 0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := polymarketVerifyToken(id, test.token); err == nil {
				t.Fatal("misdescribed token was accepted")
			}
		})
	}
}

// polymarketStubRPC is the Conditional Tokens contract: balances by token, and each condition's
// payout numerators and denominator.
type polymarketStubRPC struct {
	t            *testing.T
	balances     map[string]*big.Int
	denominators map[common.Hash]*big.Int
	numerators   map[common.Hash][]*big.Int
	mu           sync.Mutex
	methods      map[string]int
}

func (s *polymarketStubRPC) dispatch(to common.Address, data []byte) (string, map[string]any) {
	fail := map[string]any{"code": -32000, "message": "execution reverted"}
	if to == polymarketMulticall {
		return s.aggregate(data)
	}
	if to != polymarketCTF || len(data) < 4 {
		return "", fail
	}
	method, err := polymarketCTFABI.MethodById(data[:4])
	if err != nil {
		return "", fail
	}
	args, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		s.t.Errorf("decode %s: %v", method.Name, err)
		return "", fail
	}
	s.mu.Lock()
	s.methods[method.Name]++
	s.mu.Unlock()
	var out []byte
	switch method.Name {
	case "balanceOfBatch":
		ids := args[1].([]*big.Int)
		balances := make([]*big.Int, len(ids))
		for index, id := range ids {
			balances[index] = new(big.Int)
			if balance, held := s.balances[id.String()]; held {
				balances[index] = balance
			}
		}
		out, _ = method.Outputs.Pack(balances)
	case "payoutDenominator":
		denominator := s.denominators[common.Hash(args[0].([32]byte))]
		if denominator == nil {
			denominator = new(big.Int)
		}
		out, _ = method.Outputs.Pack(denominator)
	case "payoutNumerators":
		numerators := s.numerators[common.Hash(args[0].([32]byte))]
		outcome := args[1].(*big.Int)
		if !outcome.IsInt64() || outcome.Int64() >= int64(len(numerators)) {
			return "", fail
		}
		out, _ = method.Outputs.Pack(numerators[outcome.Int64()])
	default:
		return "", fail
	}
	return "0x" + common.Bytes2Hex(out), nil
}

// aggregate answers Multicall3's aggregate3 by dispatching each call it carries.
func (s *polymarketStubRPC) aggregate(data []byte) (string, map[string]any) {
	fail := map[string]any{"code": -32000, "message": "execution reverted"}
	method := polymarketMulticallABI.Methods["aggregate3"]
	if len(data) < 4 || string(data[:4]) != string(method.ID) {
		return "", fail
	}
	args, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		s.t.Errorf("decode aggregate3: %v", err)
		return "", fail
	}
	calls := *abi.ConvertType(args[0], new([]polymarketMulticallCall)).(*[]polymarketMulticallCall)
	s.mu.Lock()
	s.methods["aggregate3"]++
	s.mu.Unlock()
	results := make([]polymarketMulticallResult, len(calls))
	for index, call := range calls {
		result, failure := s.dispatch(call.Target, call.CallData)
		if failure != nil {
			if !call.AllowFailure {
				return "", fail
			}
			continue
		}
		results[index] = polymarketMulticallResult{Success: true, ReturnData: common.FromHex(result)}
	}
	out, err := method.Outputs.Pack(results)
	if err != nil {
		s.t.Errorf("encode aggregate3: %v", err)
		return "", fail
	}
	return "0x" + common.Bytes2Hex(out), nil
}

func (s *polymarketStubRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		s.t.Errorf("read RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	answer := func(call rpcTestRequest) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		switch call.Method {
		case "eth_chainId":
			response["result"] = "0x" + strconv.FormatUint(uint64(Polygon), 16)
			return response
		case "eth_call":
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
			return response
		}
		var input struct {
			To   common.Address `json:"to"`
			Data string         `json:"data"`
		}
		if err := json.Unmarshal(call.Params[0], &input); err != nil {
			response["error"] = map[string]any{"code": -32602, "message": "bad input"}
			return response
		}
		result, failure := s.dispatch(input.To, common.FromHex(input.Data))
		if failure != nil {
			response["error"] = failure
		} else {
			response["result"] = result
		}
		return response
	}
	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode RPC call: %v", err)
			return
		}
		_ = json.NewEncoder(writer).Encode(answer(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode RPC batch: %v", err)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

// polymarketFixedIndexer answers every read with the same tokens.
type polymarketFixedIndexer struct {
	ids    []*big.Int
	tokens map[string]polymarketToken
}

func (i polymarketFixedIndexer) TokenIDs(context.Context, *RPCClient, BlockRef, common.Address) (polymarketRead, error) {
	return polymarketRead{
		IndexerBlock: 97_000_000,
		TokenIDs:     i.ids,
		Tokens: func(_ context.Context, ids []*big.Int) (map[string]polymarketToken, error) {
			described := make(map[string]polymarketToken)
			for _, id := range ids {
				if token, known := i.tokens[id.String()]; known {
					described[id.String()] = token
				}
			}
			return described, nil
		},
	}, nil
}

// polymarketTestToken is outcome of a USDC.e condition, with the id the contract gives it.
func polymarketTestToken(conditionID common.Hash, outcome int) polymarketToken {
	indexSet := int64(1) << outcome
	return polymarketToken{
		ID:          polymarketPositionID(polymarketUSDCe.Address, polymarketCollectionID(conditionID, big.NewInt(indexSet))),
		ConditionID: conditionID, OutcomeIndex: outcome, IndexSet: indexSet,
	}
}

func TestPolymarketPositionsReadBalancesAndResolutionsAtThePin(t *testing.T) {
	open := common.HexToHash("0x01")
	won := common.HexToHash("0x02")
	split := common.HexToHash("0x03")
	openYes := polymarketTestToken(open, 0)
	openNo := polymarketTestToken(open, 1)
	wonYes := polymarketTestToken(won, 0)
	wonNo := polymarketTestToken(won, 1)
	splitYes := polymarketTestToken(split, 0)
	sold := polymarketTestToken(common.HexToHash("0x04"), 0)
	undescribed := polymarketTestToken(common.HexToHash("0x05"), 1)
	misdescribed := polymarketTestToken(common.HexToHash("0x06"), 0)
	dust := polymarketTestToken(common.HexToHash("0x07"), 0)

	stub := &polymarketStubRPC{
		t: t,
		balances: map[string]*big.Int{
			openYes.ID.String():      big.NewInt(12_500_000),
			openNo.ID.String():       big.NewInt(100_001),
			wonYes.ID.String():       big.NewInt(3_000_000),
			wonNo.ID.String():        big.NewInt(9_000_000),
			splitYes.ID.String():     big.NewInt(2_000_000),
			undescribed.ID.String():  big.NewInt(1_000_000),
			misdescribed.ID.String(): big.NewInt(1_000_000),
			dust.ID.String():         big.NewInt(100_000),
		},
		denominators: map[common.Hash]*big.Int{won: big.NewInt(1), split: big.NewInt(2)},
		numerators: map[common.Hash][]*big.Int{
			won:   {big.NewInt(1), big.NewInt(0)},
			split: {big.NewInt(1), big.NewInt(1)},
		},
		methods: make(map[string]int),
	}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	client, err := DialRPC(t.Context(), Polygon, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	wrong := misdescribed
	wrong.OutcomeIndex, wrong.IndexSet = 1, 2
	indexer := polymarketFixedIndexer{
		ids: []*big.Int{openYes.ID, openNo.ID, wonYes.ID, wonNo.ID, splitYes.ID, sold.ID, undescribed.ID, misdescribed.ID, dust.ID},
		tokens: map[string]polymarketToken{
			openYes.ID.String(): openYes, openNo.ID.String(): openNo, wonYes.ID.String(): wonYes,
			wonNo.ID.String(): wonNo, splitYes.ID.String(): splitYes, sold.ID.String(): sold,
			misdescribed.ID.String(): wrong, dust.ID.String(): dust,
		},
	}
	adapter := newPolymarketAdapterWithIndexer(indexer)
	block := BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}
	groups, err := adapter.Positions(t.Context(), client, block, common.HexToAddress("0xaa"))
	if err == nil {
		t.Fatal("an undescribed and a misdescribed holding were reported without an error")
	}
	for _, want := range []string{"does not describe held token " + undescribed.ID.String(), "is not outcome 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	type reported struct {
		amount string
		ratio  string
	}
	got := make(map[string]reported, len(groups))
	for _, group := range groups {
		if len(group.Components) != 1 {
			t.Fatalf("group %s has %d components", group.ID, len(group.Components))
		}
		component := group.Components[0]
		if component.Token.Address != polymarketCTF || component.Token.TokenID != group.ID ||
			component.Token.Decimals != 6 || component.Kind != "asset" {
			t.Errorf("group %s reports token %+v as %s", group.ID, component.Token, component.Kind)
		}
		if component.Source.Holds == nil || len(component.Source.Holds) != 0 {
			t.Errorf("group %s declares holdings %v, want none", group.ID, component.Source.Holds)
		}
		ratio := ""
		if component.PriceBasis != nil {
			if component.PriceBasis.Token != polymarketUSDCe {
				t.Errorf("group %s is valued through %+v", group.ID, component.PriceBasis.Token)
			}
			ratio = component.PriceBasis.RatioRaw
		}
		got[group.ID] = reported{amount: component.AmountRaw, ratio: ratio}
	}
	want := map[string]reported{
		// An open market's share is its own asset, quoted by its id; a tenth of a share or less is
		// dust and not reported.
		openYes.ID.String(): {amount: "12500000"},
		openNo.ID.String():  {amount: "100001"},
		// A resolved share is worth its payout in USDC.e; the losing side of won pays nothing.
		wonYes.ID.String():   {amount: "3000000", ratio: "1000000000000000000"},
		splitYes.ID.String(): {amount: "2000000", ratio: "500000000000000000"},
	}
	if len(got) != len(want) {
		t.Fatalf("reported %v, want %v", got, want)
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Errorf("token %s = %+v, want %+v", id, got[id], expected)
		}
	}
	// One resolution read per condition held, one payout read per resolved outcome held, each set
	// in one Multicall3 call.
	if stub.methods["payoutDenominator"] != 3 || stub.methods["payoutNumerators"] != 3 || stub.methods["aggregate3"] != 2 {
		t.Errorf("contract reads = %v", stub.methods)
	}
}

func TestPolymarketPositionsBeforeTheIndexStartReadNothing(t *testing.T) {
	adapter := newPolymarketAdapterWithIndexer(polymarketFixedIndexer{})
	groups, err := adapter.Positions(t.Context(), nil, BlockRef{ChainID: Polygon, Number: polymarketIndexStart - 1}, common.HexToAddress("0xaa"))
	if err != nil || len(groups) != 0 {
		t.Fatalf("groups = %v, err = %v", groups, err)
	}
}

// Two outcome tokens share one contract, so a quote keys on the token's id too: each is asked for
// and valued on its own.
func TestOutcomeTokensOfOneContractArePricedApart(t *testing.T) {
	yes := Token{ChainID: Polygon, Address: polymarketCTF, Symbol: "OUTCOME-0", Decimals: 6, TokenID: "11"}
	no := Token{ChainID: Polygon, Address: polymarketCTF, Symbol: "OUTCOME-1", Decimals: 6, TokenID: "22"}
	if PriceKey(yes) == PriceKey(no) || AssetForToken(yes) == AssetForToken(no) {
		t.Fatal("two token ids of one contract share a price key")
	}
	if got, want := PriceKey(yes), "polygon:"+strings.ToLower(polymarketCTF.Hex())+":11"; got != want {
		t.Fatalf("price key = %s, want %s", got, want)
	}
	if got := PriceKey(polymarketUSDCe); strings.Count(got, ":") != 1 {
		t.Fatalf("an ERC-20's price key %s carries a token id", got)
	}
	snapshots := []Snapshot{{
		ChainID: Polygon, Block: BlockRef{ChainID: Polygon, Number: 1},
		Groups: []Group{
			{ID: "11", Components: []Component{NewComponent("asset", yes, big.NewInt(2_000_000), Source{})}},
			{ID: "22", Components: []Component{NewComponent("asset", no, big.NewInt(2_000_000), Source{})}},
		},
	}}
	queries := collectPriceQueries(snapshots)
	if len(queries) != 1 || len(queries[0].Tokens) != 2 {
		t.Fatalf("price queries = %+v, want both tokens in one query", queries)
	}
	provider := fixedPriceProvider{prices: map[AssetID]float64{AssetForToken(yes): 0.25, AssetForToken(no): 0.75}}
	prices, failures := fetchPrices(t.Context(), provider, snapshots)
	if len(failures) != 0 {
		t.Fatalf("pricing failures: %v", failures)
	}
	for _, test := range []struct {
		token Token
		want  float64
	}{{yes, 0.25}, {no, 0.75}} {
		price, priced, err := componentPrice(NewComponent("asset", test.token, big.NewInt(1), Source{}), prices)
		if err != nil || !priced || price != test.want {
			t.Errorf("token %s priced %v (%v, %v), want %v", test.token.TokenID, price, priced, err, test.want)
		}
	}
}

type fixedPriceProvider struct {
	prices map[AssetID]float64
}

func (p fixedPriceProvider) USDPrices(_ context.Context, tokens []Token) PriceResult {
	result := PriceResult{Prices: make(map[AssetID]float64)}
	for _, token := range tokens {
		if price, known := p.prices[AssetForToken(token)]; known {
			result.Prices[AssetForToken(token)] = price
		}
	}
	return result
}

func (p fixedPriceProvider) USDPricesAt(ctx context.Context, tokens []Token, _ time.Time) PriceResult {
	return p.USDPrices(ctx, tokens)
}

// polymarketIndexServer is Polymarket's index for one wallet: a processor at processed, the newest
// deposit wallet at walletBlock, and the wallet's position rows. It records every GraphQL query.
type polymarketIndexServer struct {
	processed   uint64
	walletBlock uint64
	walletTime  uint64
	rows        []map[string]string
	mu          sync.Mutex
	queries     []string
}

func (s *polymarketIndexServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	if request.Method == http.MethodGet {
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("17", []ChainID{Polygon}, s.processed))
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)
	s.mu.Lock()
	s.queries = append(s.queries, body.Query)
	s.mu.Unlock()
	checkpoints := []map[string]string{}
	if s.walletBlock != 0 {
		checkpoints = append(checkpoints, map[string]string{
			"blockNumber": strconv.FormatUint(s.walletBlock, 10), "timestampSeconds": strconv.FormatUint(s.walletTime, 10),
		})
	}
	rows := s.rows
	if rows == nil {
		rows = []map[string]string{}
	}
	// Every fixture fits one page, so a page never follows it.
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
		"indexerCheckpoints": checkpoints, "positions": rows,
	}})
}

func polymarketTestIndexer(t *testing.T, server *polymarketIndexServer) *polymarketIndexer {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	indexer := newPolymarketIndexer(SentioIndexerConfig{})
	indexer.index.api = &sentioAPIClient{apiKey: "test", httpClient: httpServer.Client(), statuses: make(map[string]sentioStatusCache)}
	indexer.index.config = SentioIndexerConfig{
		GraphQLURL: httpServer.URL + "/graphql", StatusURL: httpServer.URL + "/status", ProcessorVersion: "17",
	}
	return indexer
}

// The processor writes no checkpoint row, so the newest deposit wallet certifies each page in its
// place: same request, same block, and the usual lag rules.
func TestPolymarketIndexIsCertifiedByItsNewestWallet(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	const pinTime = uint64(1_794_000_000)
	server := &polymarketIndexServer{
		processed: 97_000_000, walletBlock: 96_999_990, walletTime: pinTime - 20,
		rows: []map[string]string{
			{"id": wallet + "-11", "wallet": wallet, "tokenId": "11"},
			{"id": wallet + "-22", "wallet": wallet, "tokenId": "22"},
		},
	}
	indexer := polymarketTestIndexer(t, server)
	read, err := indexer.TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: pinTime}, account)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.TokenIDs) != 2 || read.TokenIDs[0].String() != "11" || read.TokenIDs[1].String() != "22" {
		t.Fatalf("token ids = %v", read.TokenIDs)
	}
	if read.IndexerBlock != 97_000_000 {
		t.Fatalf("indexer block = %d", read.IndexerBlock)
	}
	query := server.queries[0]
	for _, want := range []string{
		"indexerCheckpoints: wallets(first: 1, orderBy: createdBlock, orderDirection: desc, block: { number: 97000000 })",
		"blockNumber: createdBlock timestampSeconds: createdAt",
		`id_starts_with: "` + wallet + `-"`,
		`or: [{ balance_not: "0" }, { seedBalance_not: "0" }]`,
	} {
		if !strings.Contains(query, want) {
			t.Errorf("page query lacks %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "indexerCheckpoints(") {
		t.Errorf("page query reads a checkpoint row the processor does not write:\n%s", query)
	}

	// A fixed pin a week and more past the newest wallet is stale, as a checkpoint would be.
	stale := polymarketTestIndexer(t, &polymarketIndexServer{
		processed: 97_000_000, walletBlock: 96_000_000, walletTime: pinTime - uint64(indexBackfillMaxLag/time.Second) - 1,
	})
	if _, err := stale.TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: pinTime, Fixed: true}, account); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v, want the staleness error", err)
	}
}

func TestPolymarketIndexRejectsAForeignRow(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	for _, row := range []map[string]string{
		{"id": wallet + "-11", "wallet": wallet, "tokenId": "12"},
		{"id": wallet + "-011", "wallet": wallet, "tokenId": "011"},
		{"id": wallet + "-11", "wallet": "0x00000000000000000000000000000000000000bb", "tokenId": "11"},
	} {
		indexer := polymarketTestIndexer(t, &polymarketIndexServer{
			processed: 97_000_000, walletBlock: 97_000_000, walletTime: 1_794_000_000, rows: []map[string]string{row},
		})
		if _, err := indexer.TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account); err == nil {
			t.Errorf("row %v was accepted", row)
		}
	}
}

func TestIndexCheckpointReadsATimeInSeconds(t *testing.T) {
	checkpoint, err := parseIndexCheckpoint(json.RawMessage(`[{"blockNumber":"7","timestampSeconds":"1794000000"}]`))
	if err != nil || checkpoint != (indexCheckpoint{written: true, block: 7, ms: 1_794_000_000_000}) {
		t.Fatalf("checkpoint = %+v, err = %v", checkpoint, err)
	}
	if _, err := parseIndexCheckpoint(json.RawMessage(`[{"blockNumber":"7","timestampMs":"1","timestampSeconds":"1"}]`)); err == nil {
		t.Fatal("a checkpoint with two timestamps was accepted")
	}
	if _, err := parseIndexCheckpoint(json.RawMessage(`[{"blockNumber":"7","timestampSeconds":"x"}]`)); err == nil {
		t.Fatal("a malformed timestamp was accepted")
	}
}
