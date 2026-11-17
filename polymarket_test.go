package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
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
	// delay holds each HTTP request, so that requests sent together overlap; widest is the most
	// that were in flight at once.
	delay            time.Duration
	inFlight, widest int
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
	s.mu.Lock()
	s.inFlight++
	s.widest = max(s.widest, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	time.Sleep(s.delay)
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

// A trader who never redeems holds thousands of tokens. Its balances and resolutions take tens of
// calls each, sent as JSON-RPC batches at once rather than one after another, and every result
// lands with its call.
func TestPolymarketReadsAHeavyWalletInConcurrentBatches(t *testing.T) {
	const held = 40 * polymarketBalanceBatch
	stub := &polymarketStubRPC{
		t: t, balances: make(map[string]*big.Int), denominators: make(map[common.Hash]*big.Int),
		numerators: make(map[common.Hash][]*big.Int), methods: make(map[string]int), delay: 20 * time.Millisecond,
	}
	ids := make([]*big.Int, 0, held)
	tokens := make(map[string]polymarketToken, held)
	for index := 0; index < held; index++ {
		// Tokens of one condition each: a balance per token, a resolution per condition.
		condition := common.BigToHash(big.NewInt(int64(index + 1)))
		token := polymarketToken{ID: big.NewInt(int64(1_000_000 + index)), ConditionID: condition, IndexSet: 1}
		ids = append(ids, token.ID)
		tokens[token.ID.String()] = token
		stub.balances[token.ID.String()] = big.NewInt(int64(1_000_000 + index))
		if index%2 == 0 {
			stub.denominators[condition] = big.NewInt(1)
			stub.numerators[condition] = []*big.Int{big.NewInt(int64(index % 4 / 2))}
		}
	}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	client, err := DialRPC(t.Context(), Polygon, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	block := BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}

	holdings, err := polymarketBalances(t.Context(), client, block, common.HexToAddress("0xaa"), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(holdings) != held {
		t.Fatalf("read %d holdings, want %d", len(holdings), held)
	}
	for index, holding := range holdings {
		if holding.id.Cmp(ids[index]) != 0 || holding.amount.Int64() != int64(1_000_000+index) {
			t.Fatalf("holding %d = %s of %s, want its own balance", index, holding.amount, holding.id)
		}
	}
	positions := make([]polymarketPosition, len(holdings))
	for index, holding := range holdings {
		positions[index] = polymarketPosition{holding: holding, token: tokens[holding.id.String()]}
	}
	calls := make([]ContractCall, len(positions))
	for index, position := range positions {
		calls[index] = ContractCall{Contract: polymarketCTF, ABI: polymarketCTFABI, Method: "payoutDenominator", Args: []any{position.token.ConditionID}}
	}
	rows, err := polymarketViews(t.Context(), client, block, calls)
	if err != nil {
		t.Fatal(err)
	}
	for index, row := range rows {
		denominator, err := BigIntAt(row, 0)
		if err != nil || denominator.Int64() != int64(1-index%2) {
			t.Fatalf("condition %d resolution = %v (%v)", index, denominator, err)
		}
	}
	if stub.methods["balanceOfBatch"] != held/polymarketBalanceBatch || stub.methods["aggregate3"] != held/polymarketViewBatch {
		t.Fatalf("contract reads = %v", stub.methods)
	}
	if stub.widest < 2 {
		t.Fatal("the wallet's JSON-RPC batches were sent one after another")
	}
	if stub.widest > polymarketRPCConcurrency {
		t.Fatalf("%d JSON-RPC batches were in flight at once, more than %d", stub.widest, polymarketRPCConcurrency)
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
// deposit wallet at walletBlock, the wallet's position rows and the outcome token rows. It answers
// each query as the index does — rows under the field's id_starts_with, from its id_gte, after its
// id_gt, among its id_in, at most first of them in id order — and records it.
type polymarketIndexServer struct {
	processed   uint64
	walletBlock uint64
	walletTime  uint64
	rows        []map[string]string
	tokens      []map[string]any
	// unfiltered answers every query with every row, as a server ignoring the where clause would.
	unfiltered bool
	// walletBlockFor, when set, answers the newest wallet's block for a query instead of walletBlock.
	walletBlockFor func(query string) uint64

	mu      sync.Mutex
	queries []string
}

var (
	polymarketTestFirst    = regexp.MustCompile(`(positions|outcomeTokens)\(first: (\d+),`)
	polymarketTestPrefix   = regexp.MustCompile(`id_starts_with: "([^"]*)"`)
	polymarketTestAfter    = regexp.MustCompile(`id_gt: "([^"]*)"`)
	polymarketTestFrom     = regexp.MustCompile(`id_gte: "([^"]*)"`)
	polymarketTestIDsIn    = regexp.MustCompile(`id_in: \[([^\]]*)\]`)
	polymarketTestQuotedID = regexp.MustCompile(`"([^"]*)"`)
)

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
	walletBlock := s.walletBlock
	if s.walletBlockFor != nil {
		walletBlock = s.walletBlockFor(body.Query)
	}
	checkpoints := []map[string]string{}
	if walletBlock != 0 {
		checkpoints = append(checkpoints, map[string]string{
			"blockNumber": strconv.FormatUint(walletBlock, 10), "timestampSeconds": strconv.FormatUint(s.walletTime, 10),
		})
	}
	data := map[string]any{"indexerCheckpoints": checkpoints}
	field := polymarketTestFirst.FindStringSubmatch(body.Query)
	if field == nil {
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
		return
	}
	entity := field[1]
	first, _ := strconv.Atoi(field[2])
	var all []map[string]any
	if entity == "positions" {
		for _, row := range s.rows {
			converted := make(map[string]any, len(row))
			for key, value := range row {
				converted[key] = value
			}
			all = append(all, converted)
		}
	} else {
		all = append(all, s.tokens...)
	}
	sort.Slice(all, func(left, right int) bool { return all[left]["id"].(string) < all[right]["id"].(string) })
	prefix, after := "", ""
	if found := polymarketTestPrefix.FindStringSubmatch(body.Query); found != nil {
		prefix = found[1]
	}
	from := ""
	if found := polymarketTestAfter.FindStringSubmatch(body.Query); found != nil {
		after = found[1]
	}
	if found := polymarketTestFrom.FindStringSubmatch(body.Query); found != nil {
		from = found[1]
	}
	var wanted map[string]bool
	if found := polymarketTestIDsIn.FindStringSubmatch(body.Query); found != nil {
		wanted = make(map[string]bool)
		for _, id := range polymarketTestQuotedID.FindAllStringSubmatch(found[1], -1) {
			wanted[id[1]] = true
		}
	}
	rows := make([]map[string]any, 0)
	for _, row := range all {
		id := row["id"].(string)
		if !s.unfiltered && (!strings.HasPrefix(id, prefix) || id <= after || id < from || (wanted != nil && !wanted[id])) {
			continue
		}
		if len(rows) < first {
			rows = append(rows, row)
		}
	}
	data[entity] = rows
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
}

// queriesOf returns the recorded queries that read entity.
func (s *polymarketIndexServer) queriesOf(entity string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, query := range s.queries {
		if strings.Contains(query, entity+"(") {
			out = append(out, query)
		}
	}
	return out
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
			{"id": wallet + "-11", "wallet": wallet, "tokenId": "11", "balance": "2000000", "seedBalance": "0"},
			{"id": wallet + "-22", "wallet": wallet, "tokenId": "22", "balance": "2000000", "seedBalance": "0"},
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
	for _, query := range server.queriesOf("positions") {
		for _, want := range []string{
			"indexerCheckpoints: wallets(first: 1, orderBy: createdBlock, orderDirection: desc, block: { number: 97000000 })",
			"blockNumber: createdBlock timestampSeconds: createdAt",
			`id_starts_with: "` + wallet + `-`,
			`or: [{ balance_gt: "100000" }, { seedBalance_not: "0" }]`,
		} {
			if !strings.Contains(query, want) {
				t.Errorf("page query lacks %q:\n%s", want, query)
			}
		}
		if strings.Contains(query, "indexerCheckpoints(") {
			t.Errorf("page query reads a checkpoint row the processor does not write:\n%s", query)
		}
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
	for _, test := range []struct {
		row        map[string]string
		unfiltered bool
		error      string
	}{
		{row: map[string]string{"id": wallet + "-11", "wallet": wallet, "tokenId": "12", "balance": "2000000", "seedBalance": "0"}, error: "malformed Polymarket position row"},
		{row: map[string]string{"id": wallet + "-11", "wallet": wallet, "tokenId": "11", "balance": "1e6", "seedBalance": "0"}, error: "malformed Polymarket position row"},
		// A token id with a leading zero is no token's: the stream for the digit 0 reads it, and it fails.
		{row: map[string]string{"id": wallet + "-011", "wallet": wallet, "tokenId": "011", "balance": "2000000", "seedBalance": "0"}, error: "malformed Polymarket position row"},
		{row: map[string]string{"id": wallet + "-11", "wallet": "0x00000000000000000000000000000000000000bb", "tokenId": "11", "balance": "2000000", "seedBalance": "0"}, error: "malformed Polymarket position row"},
		// A row answered to the stream of another leading digit is outside the stream's prefix.
		{row: map[string]string{"id": wallet + "-22", "wallet": wallet, "tokenId": "22", "balance": "2000000", "seedBalance": "0"}, unfiltered: true, error: "foreign positions row"},
	} {
		indexer := polymarketTestIndexer(t, &polymarketIndexServer{
			processed: 97_000_000, walletBlock: 97_000_000, walletTime: 1_794_000_000,
			rows: []map[string]string{test.row}, unfiltered: test.unfiltered,
		})
		_, err := indexer.TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
		if err == nil || !strings.Contains(err.Error(), test.error) {
			t.Errorf("row %v: error = %v, want %q", test.row, err, test.error)
		}
	}
}

// polymarketTestRows are position rows of wallet for each token id.
func polymarketTestRows(wallet string, ids []string) []map[string]string {
	rows := make([]map[string]string, len(ids))
	for index, id := range ids {
		rows[index] = map[string]string{"id": wallet + "-" + id, "wallet": wallet, "tokenId": id, "balance": "2000000", "seedBalance": "0"}
	}
	return rows
}

// A trader's rows run to the tens of thousands, and the server answers one request's rows at about
// a thousand a second, so the wallet is read in one stream per leading digit of the token id, each
// in requests of its own and in large pages. Together the streams read every row, in id order.
func TestPolymarketIndexReadsAWalletInAStreamPerLeadingDigit(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	// "3" is a whole id equal to its stream's prefix, which the stream still reads.
	ids := []string{"905", "11", "3", "22"}
	// One digit's rows run past a page.
	for index := 0; index <= polymarketIndexPageSize; index++ {
		ids = append(ids, strconv.Itoa(700_000+index))
	}
	server := &polymarketIndexServer{
		processed: 97_000_000, walletBlock: 96_999_990, walletTime: 1_794_000_000 - 20, rows: polymarketTestRows(wallet, ids),
	}
	read, err := polymarketTestIndexer(t, server).TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(read.TokenIDs) != len(ids) {
		t.Fatalf("read %d token ids, want %d", len(read.TokenIDs), len(ids))
	}
	for index, id := range read.TokenIDs {
		if id.String() != ids[index] {
			t.Fatalf("token id %d = %s, want %s: the rows are not in id order", index, id, ids[index])
		}
	}
	perDigit := make(map[string]int)
	for _, query := range server.queriesOf("positions") {
		lead := polymarketTestPrefix.FindStringSubmatch(query)[1]
		perDigit[strings.TrimPrefix(lead, wallet+"-")]++
		if !strings.Contains(query, fmt.Sprintf("positions(first: %d,", polymarketIndexPageSize)) {
			t.Errorf("page does not ask for %d rows:\n%s", polymarketIndexPageSize, query)
		}
		if strings.Count(query, "positions(") != 1 {
			t.Errorf("a request reads more than one stream:\n%s", query)
		}
	}
	for digit := '0'; digit <= '9'; digit++ {
		want := 1
		if digit == '7' {
			want = 2
		}
		if perDigit[string(digit)] != want {
			t.Errorf("stream %c sent %d pages, want %d (pages = %v)", digit, perDigit[string(digit)], want, perDigit)
		}
	}
}

// A row names a token to read only when its balance, the wallet's before the start and its net
// since, is above the dust floor: market making leaves tens of thousands of positions closed to a
// remainder. The server selects rows with no balance before the start by their net alone.
func TestPolymarketIndexReadsOnlyRowsAboveTheDustFloor(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	row := func(id, balance, seed string) map[string]string {
		return map[string]string{"id": wallet + "-" + id, "wallet": wallet, "tokenId": id, "balance": balance, "seedBalance": seed}
	}
	server := &polymarketIndexServer{
		processed: 97_000_000, walletBlock: 97_000_000, walletTime: 1_794_000_000,
		rows: []map[string]string{
			row("11", "2000000", "0"),        // bought since the start
			row("22", "100000", "0"),         // a remainder of a tenth of a share
			row("33", "-5000000", "5000000"), // held before the start, sold since
			row("44", "-1000000", "3000000"), // held before the start, partly sold
			row("55", "-4900000", "5000000"), // held before the start, sold to a remainder
			row("66", "0", "7000000"),        // held before the start, untouched in net
		},
	}
	read, err := polymarketTestIndexer(t, server).TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(read.TokenIDs))
	for index, id := range read.TokenIDs {
		got[index] = id.String()
	}
	if strings.Join(got, ",") != "11,44,66" {
		t.Fatalf("token ids = %v, want 11, 44 and 66", got)
	}
}

// The bound on a wallet's rows holds across its streams: no stream past it, the wallet still is.
func TestPolymarketIndexBoundsAWalletsRowsAcrossItsStreams(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	perDigit := polymarketMaxIndexedPositions/9 + 1
	ids := make([]string, 0, 9*perDigit)
	for digit := 1; digit <= 9; digit++ {
		for index := 0; index < perDigit; index++ {
			ids = append(ids, strconv.Itoa(digit*10_000_000+index))
		}
	}
	server := &polymarketIndexServer{processed: 97_000_000, walletBlock: 97_000_000, walletTime: 1_794_000_000, rows: polymarketTestRows(wallet, ids)}
	_, err := polymarketTestIndexer(t, server).TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d positions rows", polymarketMaxIndexedPositions)) {
		t.Fatalf("error = %v, want the wallet's row bound", err)
	}
}

// Every page of every stream carries the read's one certificate.
func TestPolymarketIndexHoldsItsStreamsToOneCertificate(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	server := &polymarketIndexServer{
		processed: 97_000_000, walletTime: 1_794_000_000, rows: polymarketTestRows(wallet, []string{"11", "22"}),
		walletBlockFor: func(query string) uint64 {
			if strings.Contains(query, `id_starts_with: "`+wallet+`-2"`) {
				return 96_999_999
			}
			return 97_000_000
		},
	}
	_, err := polymarketTestIndexer(t, server).TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
	if err == nil || !strings.Contains(err.Error(), "GraphQL pagination changed checkpoint") {
		t.Fatalf("error = %v, want the changed checkpoint", err)
	}
}

// A lookup names its ids polymarketLookupBatch to a request, each in one page.
func TestPolymarketOutcomeLookupIsBatched(t *testing.T) {
	account := common.HexToAddress("0x00000000000000000000000000000000000000Aa")
	wallet := strings.ToLower(account.Hex())
	const count = 2*polymarketLookupBatch + 7
	ids := make([]*big.Int, 0, count+1)
	tokens := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		id := big.NewInt(int64(1_000_000 + index))
		ids = append(ids, id)
		tokens = append(tokens, map[string]any{
			"id": id.String(), "conditionId": "0x" + fmt.Sprintf("%064x", index+1), "outcomeIndex": 0, "indexSet": 1,
		})
	}
	// An id named twice is asked for once; one the index does not describe is left out.
	ids = append(ids, ids[0], big.NewInt(9))
	server := &polymarketIndexServer{
		processed: 97_000_000, walletBlock: 97_000_000, walletTime: 1_794_000_000,
		rows: polymarketTestRows(wallet, []string{"11"}), tokens: tokens,
	}
	read, err := polymarketTestIndexer(t, server).TokenIDs(t.Context(), nil, BlockRef{ChainID: Polygon, Number: 97_000_000, Timestamp: 1_794_000_000}, account)
	if err != nil {
		t.Fatal(err)
	}
	described, err := read.Tokens(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(described) != count || described["1000000"].ConditionID != common.HexToHash("0x01") {
		t.Fatalf("described %d tokens, want %d", len(described), count)
	}
	lookups := server.queriesOf("outcomeTokens")
	if len(lookups) != 3 {
		t.Fatalf("sent %d lookups, want 3", len(lookups))
	}
	asked := 0
	for _, query := range lookups {
		named := len(polymarketTestQuotedID.FindAllString(polymarketTestIDsIn.FindStringSubmatch(query)[1], -1))
		if named > polymarketLookupBatch || !strings.Contains(query, fmt.Sprintf("outcomeTokens(first: %d,", named+1)) {
			t.Errorf("a lookup names %d ids:\n%.200s", named, query)
		}
		asked += named
	}
	if asked != count+1 {
		t.Fatalf("lookups named %d ids, want %d", asked, count+1)
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
