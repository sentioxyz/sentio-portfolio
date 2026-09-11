package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func naviInt(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return n
}
func naviAddress(s string) string {
	a, err := ParseSuiAddress(s)
	if err != nil {
		panic(err)
	}
	return a.Hex()
}

func TestNaviInterestAndTokenPrecision(t *testing.T) {
	// A 10% supply rate accrues linearly; borrow compounds using Move's
	// three-term approximation, including half-up intermediate products.
	index, err := naviIndexAt(naviRay, naviInt("100000000000000000000000000"), 1000, 31_536_001_000, false)
	if err != nil || index.String() != "1100000000000000000000000000" {
		t.Fatalf("supply index %v %v", index, err)
	}
	for _, test := range []struct {
		decimals uint8
		want     string
	}{{6, "1100000"}, {8, "110000000"}, {9, "1100000000"}, {18, "1100000000000000000"}} {
		got := naviTokenAmount(big.NewInt(1_000_000_000), index, test.decimals)
		if got.String() != test.want {
			t.Fatalf("decimals %d: %s", test.decimals, got)
		}
	}
	// 100% per second (an artificial exact arithmetic case) at t=3 gives
	// 1 + 3 + 3 + 1 = 8, exercising both higher-order borrow terms.
	rate := new(big.Int).Mul(naviRay, big.NewInt(31_536_000))
	got, err := naviIndexAt(naviRay, rate, 1000, 4000, true)
	if err != nil || got.Cmp(new(big.Int).Mul(naviRay, big.NewInt(8))) != 0 {
		t.Fatalf("borrow %v %v", got, err)
	}
	if _, err = naviIndexAt(naviRay, rate, 2000, 1999, true); err == nil {
		t.Fatal("future reserve state accepted")
	}
	if got := naviRayMul(big.NewInt(1), naviHalfRay); got.Int64() != 1 {
		t.Fatal("ray rounding is not half up")
	}
}

func TestVoloRedemptionKeepsEveryMoveRoundingStep(t *testing.T) {
	// floor(2/3 * 1e9) then floor(3 * ratio / 1e9) is 1 USD9,
	// whereas fusing the fraction incorrectly returns 2.
	got := voloBaseAmount(big.NewInt(3), big.NewInt(3), big.NewInt(2), big.NewInt(1_000_000_000_000_000_000), 9)
	if got.Int64() != 1 {
		t.Fatalf("rounded claim %v", got)
	}
	// 1,500 USD9 with six-decimal USDC -> 1,500,000,000 base units.
	got = voloBaseAmount(big.NewInt(500_000_000), big.NewInt(1_000_000_000), big.NewInt(3_000_000_000_000), big.NewInt(1_000_000_000_000_000_000), 6)
	if got.String() != "1500000000" {
		t.Fatalf("USDC claim %v", got)
	}
}

type naviReaderFixture struct {
	pin      SuiCheckpoint
	metadata map[string]SuiCoinMetadata
}

func (f naviReaderFixture) LatestCheckpoint(context.Context) (SuiCheckpoint, error) {
	return f.pin, nil
}
func (f naviReaderFixture) CheckpointBySequence(_ context.Context, n uint64) (SuiCheckpoint, error) {
	p := f.pin
	p.Sequence = n
	return p, nil
}
func (f naviReaderFixture) Holdings(context.Context, SuiAddress, *SuiCheckpoint) (SuiHoldings, error) {
	panic("protocol history must not read wallet head")
}
func (f naviReaderFixture) CoinMetadata(_ context.Context, types []string) (map[string]SuiCoinMetadata, map[string]error, error) {
	out := map[string]SuiCoinMetadata{}
	for _, coin := range types {
		if m, ok := f.metadata[coin]; ok {
			out[coin] = m
		}
	}
	return out, nil, nil
}
func (f naviReaderFixture) Close() {}

func historyObjectFixture(id, kind, owner, parent, key, objectType string, content map[string]any) suiHistoryObject {
	raw, _ := json.Marshal(content)
	return suiHistoryObject{ID: naviAddress(id), Kind: kind, Owner: owner, Parent: parent, Key: key, ObjectType: objectType, Content: string(raw), Version: "1", Checkpoint: "90"}
}

func naviFixtureServer(t *testing.T, objects []suiHistoryObject, values []suiHistoryValue, pin SuiCheckpoint) *SuiProtocolReader {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID < objects[j].ID })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		data := map[string]any{}
		if strings.Contains(request.Query, "indexerCheckpoints") {
			data["indexerCheckpoints"] = []map[string]any{{"id": "sui_mainnet", "blockNumber": strconv.FormatUint(pin.Sequence, 10), "timestampMs": strconv.FormatInt(pin.Timestamp.UnixMilli(), 10), "digest": suiTestDigest}}
		} else {
			if request.Variables["block"] != strconv.FormatUint(pin.Sequence, 10) {
				t.Errorf("unpinned query: %v", request.Variables)
			}
			if strings.Contains(request.Query, "suiHistoryObjects") {
				rows := []suiHistoryObject{}
				for _, row := range objects {
					if (strings.Contains(request.Query, `kind: "`+row.Kind+`"`) || (strings.Contains(request.Query, "kind_in:") && strings.Contains(request.Query, strconv.Quote(row.Kind)))) || (strings.Contains(request.Query, `owner: "`+row.Owner+`"`) && row.Owner != "") {
						rows = append(rows, row)
					}
				}
				data["suiHistoryObjects"] = rows
			} else {
				rows := []suiHistoryValue{}
				for _, row := range values {
					if strings.Contains(request.Query, strconv.Quote(row.Kind)) {
						rows = append(rows, row)
					}
				}
				sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
				data["suiHistoryValues"] = rows
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	config := SentioIndexerConfig{GraphQLURL: server.URL, StatusURL: server.URL, ProcessorVersion: "1"}
	return NewSuiProtocolReader(EngineConfig{SentioIndexers: map[string]SentioIndexerConfig{"navi": config, "volo-vaults": config}})
}

func TestNaviHistoricalLendingAndTransferredMultiplyCap(t *testing.T) {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	owner, _ := ParseSuiAddress("0x11")
	cap := naviAddress("0x22")
	parent := naviAddress("0x33")
	objects := []suiHistoryObject{
		historyObjectFixture("0x22", "account", owner.Hex(), "", "", "", map[string]any{"owner": cap}),
		historyObjectFixture("0x44", "principal", cap, parent, "4:0:borrow", "", map[string]any{"name": cap, "value": "2000000000"}),
		historyObjectFixture("0x55", "reserve", "", "", "4:0", "", map[string]any{"value": map[string]any{"id": 0, "coin_type": "2::sui::SUI", "current_borrow_index": naviRay.String(), "current_borrow_rate": "0", "last_update_timestamp": "1000000", "borrow_balance": map[string]any{"user_state": map[string]any{"id": parent}}}}),
	}
	rows := []suiHistoryValue{{ID: "emode:4:" + cap, Kind: "emode", Account: cap, Market: "4", Content: `{"entered":true}`, Checkpoint: "90"}}
	source := naviFixtureServer(t, objects, rows, pin)
	reader := naviReaderFixture{pin: pin, metadata: map[string]SuiCoinMetadata{suiLongType: {CoinType: suiLongType, Symbol: "SUI", Decimals: 9}}}
	result, err := source.Read(context.Background(), "navi", owner, pin, reader)
	if err != nil || len(result.Errors) != 0 || len(result.Groups) != 1 {
		t.Fatalf("result %+v, %v", result, err)
	}
	group := result.Groups[0]
	if group.Label != "Multiply" || group.Components[0].Kind != "debt" || group.Components[0].AmountRaw != "2000000000" {
		t.Fatalf("wrong multiply %+v", group)
	}
	// Removing historical cap ownership must prevent attributing its debt, even
	// though its principal table still exists unchanged.
	objects[0].Owner = naviAddress("0x99")
	result, err = source.Read(context.Background(), "navi", owner, pin, reader)
	// The fake query intentionally returns cap principals for any principal
	// request: an owner mismatch must fail closed, never leak the other account.
	if err == nil && len(result.Errors) == 0 && len(result.Groups) > 0 {
		t.Fatal("transferred cap remained attributed")
	}
}

func TestNaviHistoryRejectsUncoveredAndFutureRows(t *testing.T) {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	source := naviFixtureServer(t, nil, nil, pin)
	owner, _ := ParseSuiAddress("0x1")
	requested := pin
	requested.Sequence++
	if _, err := source.Read(context.Background(), "navi", owner, requested, naviReaderFixture{}); err == nil {
		t.Fatal("uncovered checkpoint accepted")
	}
	if err := historyRowAt("101", 100); err == nil {
		t.Fatal("future state accepted")
	}
	if err := historyRowAt("garbage", 100); err == nil {
		t.Fatal("malformed state accepted")
	}
}

func TestVoloPendingDepositAndWithdrawalDoNotDoubleCount(t *testing.T) {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	owner, _ := ParseSuiAddress("0x11")
	receipt := naviAddress("0x22")
	vault := naviAddress("0x33")
	parent := naviAddress("0x44")
	objects := []suiHistoryObject{
		historyObjectFixture(receipt, "receipt", owner.Hex(), "", vault, "", map[string]any{"vault_id": vault}),
		historyObjectFixture(vault, "vault", "", "", "", "0x1::vault::Vault<0x2::sui::SUI>", map[string]any{"total_shares": "1000000000", "asset_types": []string{"2::sui::SUI"}, "receipts": map[string]any{"id": parent}}),
		historyObjectFixture("0x55", "receiptState", "", parent, receipt, "", map[string]any{"name": receipt, "value": map[string]any{"shares": "500000000", "pending_withdraw_shares": "250000000", "pending_deposit_balance": "200000000", "claimable_principal": "300000000"}}),
		historyObjectFixture("0x66", "oracle", "", "", "", "", map[string]any{"aggregators": map[string]any{"id": naviAddress("0x77")}}),
		historyObjectFixture("0x88", "oraclePrice", "", naviAddress("0x77"), "2::sui::SUI", "", map[string]any{"name": "2::sui::SUI", "value": map[string]any{"price": "1000000000000000000", "decimals": "9", "last_updated": "1000000"}}),
	}
	values := []suiHistoryValue{
		{ID: "assetValue:" + vault + ":2::sui::SUI", Kind: "assetValue", Account: vault, Content: `{"asset":"2::sui::SUI","amount":"2000000000","timestamp":"1000000"}`, Checkpoint: "90"},
		{ID: "oraclePrice:2::sui::SUI", Kind: "oraclePrice", Account: "2::sui::SUI", Content: `{"asset":"2::sui::SUI","amount":"1000000000000000000","timestamp":"1000000"}`, Checkpoint: "90"},
	}
	source := naviFixtureServer(t, objects, values, pin)
	reader := naviReaderFixture{pin: pin, metadata: map[string]SuiCoinMetadata{suiLongType: {CoinType: suiLongType, Symbol: "SUI", Decimals: 9}}}
	result, err := source.Read(context.Background(), "volo-vaults", owner, pin, reader)
	if err != nil || len(result.Errors) != 0 || len(result.Groups) != 1 {
		t.Fatalf("result %+v %v", result, err)
	}
	sum := new(big.Int)
	for _, c := range result.Groups[0].Components {
		sum.Add(sum, naviInt(c.AmountRaw))
	}
	if sum.String() != "1500000000" {
		t.Fatalf("pending withdrawal counted twice: %s", sum)
	}
}

func TestNaviSharedIndexerAdmission(t *testing.T) {
	engine := NewEngineWithConfig(nil, nil, EngineConfig{IndexerConcurrency: 2})
	reader := engine.SuiProtocolReader(EngineConfig{})
	if reader.lane != engine.indexerLane {
		t.Fatal("Sui created a separate indexer lane")
	}
}

func TestNaviVaultStoredNAVAndReceiptTransfer(t *testing.T) {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	owner, _ := ParseSuiAddress("0x11")
	receipt, vault, parent := naviAddress("0x22"), naviAddress("0x33"), naviAddress("0x44")
	objects := []suiHistoryObject{
		historyObjectFixture(receipt, "receipt", owner.Hex(), "", vault, "", map[string]any{"vault_id": vault}),
		historyObjectFixture(vault, "vault", "", "", "", "0x1::navi_vault::Vault<0x2::sui::SUI>", map[string]any{"total_shares": "3", "total_assets": "1000000001", "user_states": map[string]any{"id": parent}}),
		historyObjectFixture("0x55", "receiptState", "", parent, receipt, "", map[string]any{"name": receipt, "value": map[string]any{"shares": "2"}}),
	}
	source := naviFixtureServer(t, objects, nil, pin)
	chain := naviReaderFixture{pin: pin, metadata: map[string]SuiCoinMetadata{suiLongType: {CoinType: suiLongType, Symbol: "SUI", Decimals: 9}}}
	result, err := source.Read(context.Background(), "navi", owner, pin, chain)
	if err != nil || len(result.Errors) > 0 || len(result.Groups) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if result.Groups[0].Components[0].AmountRaw != "666666667" {
		t.Fatalf("NAV rounding: %+v", result.Groups[0])
	}
	objects[0].Owner = naviAddress("0x99")
	result, err = source.Read(context.Background(), "navi", owner, pin, chain)
	if err != nil || len(result.Errors) > 0 || len(result.Groups) > 0 {
		t.Fatalf("receipt stayed with prior owner: %+v %v", result, err)
	}
}

func TestVoloHistoryRejectsFutureOracleAndWrongParent(t *testing.T) {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	parent := naviAddress("0x22")
	rows := []suiHistoryObject{
		historyObjectFixture("0x11", "oracle", "", "", "", "", map[string]any{"aggregators": map[string]any{"id": parent}}),
		historyObjectFixture("0x33", "oraclePrice", "", parent, "2::sui::SUI", "", map[string]any{"name": "2::sui::SUI", "value": map[string]any{"decimals": "9", "price": "1000000000000000000", "last_updated": "1000001"}}),
	}
	source := naviFixtureServer(t, rows, nil, pin)
	if _, _, err := source.voloOraclePrices(context.Background(), pin, nil); err == nil {
		t.Fatal("future oracle accepted")
	}
	rows[1].Content = strings.ReplaceAll(rows[1].Content, "1000001", "1000000")
	rows[1].Parent = naviAddress("0x44")
	if _, _, err := source.voloOraclePrices(context.Background(), pin, nil); err == nil {
		t.Fatal("other oracle's entry accepted")
	}
}
