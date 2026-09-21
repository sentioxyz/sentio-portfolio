package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// naviIndexRows restates the processor's projection: which kind an object type
// is, and which key, parent, relation and links its state row carries. Feeding
// the node fixture's own objects through it is what lets one holding be read
// both ways and compared, so a drift between the two paths fails here.
func naviIndexRows(t *testing.T, f *latestSuiFixture, pin SuiCheckpoint) []suiSQLRow {
	t.Helper()
	type projected struct {
		kind, key, related string
		links              map[string]any
	}
	field := func(content string) map[string]any {
		var out map[string]any
		if err := json.Unmarshal([]byte(content), &out); err != nil {
			t.Fatalf("fixture content is not an object: %v", err)
		}
		return out
	}
	table := func(value any) string {
		inner, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("fixture table reference is not an object")
		}
		return naviAddress(fmt.Sprint(inner["id"]))
	}
	project := func(object SuiObject) (projected, bool) {
		content := field(object.Content)
		switch object.ObjectType {
		case suiType(naviStorageType):
			return projected{kind: "storage", links: map[string]any{"reserveTableId": table(content["reserves"])}}, true
		case suiType(naviAccountType):
			return projected{kind: "account", links: map[string]any{"accountAddress": naviAddress(fmt.Sprint(content["owner"]))}}, true
		case suiType(suiFieldType(naviMarketKeyType, "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::MarketInfo")):
			value := content["value"].(map[string]any)
			id := fmt.Sprint(value["market_id"])
			return projected{kind: "market", key: id, links: map[string]any{"marketId": id, "isMainMarket": value["is_main_market"]}}, true
		case suiType(suiFieldType("u8", "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::ReserveData")):
			value := content["value"].(map[string]any)
			coin, _ := NormalizeMoveType("0x" + strings.TrimPrefix(fmt.Sprint(value["coin_type"]), "0x"))
			return projected{kind: "reserve", key: fmt.Sprint(content["name"]), links: map[string]any{
				"coinTypes":     []string{coin},
				"supplyTableId": table(value["supply_balance"].(map[string]any)["user_state"]),
				"borrowTableId": table(value["borrow_balance"].(map[string]any)["user_state"]),
			}}, true
		case suiType(suiFieldType("address", "u256")):
			return projected{kind: "principal", key: naviAddress(fmt.Sprint(content["name"]))}, true
		}
		return projected{}, false
	}
	rows := []suiSQLRow{}
	emit := func(object SuiObject, parent string) {
		p, ok := project(object)
		if !ok {
			return
		}
		links, _ := json.Marshal(p.links)
		if p.links == nil {
			links = []byte("{}")
		}
		rows = append(rows, sqlFixtureRow("object", suiSQLObject{
			ID:       fmt.Sprintf("%s:%020d:%s", p.kind, pin.Sequence, suiTestVersionID(object.ID, object.Version, false)),
			ObjectID: object.ID, Kind: p.kind, Version: fmt.Sprint(object.Version), Digest: suiTestDigest, State: "live",
			OwnerKind: object.OwnerKind, Owner: object.Owner, ObjectType: object.ObjectType, Content: object.Content,
			Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()),
			TransactionDigest: suiTestDigest, MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100),
			ParentID: parent, Key: p.key, RelatedID: p.related, Links: string(links),
		}))
	}
	for _, object := range f.objects {
		emit(object, object.Owner)
	}
	for parent, objects := range f.tables {
		for _, object := range objects {
			emit(object, naviAddress(parent))
		}
	}
	return rows
}

// newNaviIndexFixture serves those rows as the one SQL statement the reader
// issues, with a certificate that counts exactly what the rows contain.
func newNaviIndexFixture(t *testing.T, f *latestSuiFixture, values []suiSQLValue) (*sqlPortfolioFixture, *NaviHistoryReader) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	pin := f.pin
	pin.Sequence = naviHistoryStart + 100
	fixture := &sqlPortfolioFixture{pin: pin}
	objects := naviIndexRows(t, f, pin)
	fixture.rows = append(fixture.rows, sqlFixtureRow("snapshot", suiSQLSnapshot{
		ID: fmt.Sprintf("%020d", pin.Sequence), Checkpoint: fmt.Sprint(pin.Sequence),
		TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), Digest: suiTestDigest, SchemaVersion: "2",
		ObjectCount: fmt.Sprint(len(objects)), ObservedObjectCount: fmt.Sprint(len(objects)),
		ValueCount: fmt.Sprint(len(values)), ObservedValueCount: fmt.Sprint(len(values)),
		StartCheckpoint: fmt.Sprint(naviHistoryStart), PreviousCheckpoint: fmt.Sprint(naviHistoryStart - 1),
		MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), NextCheckpoint: fmt.Sprint(pin.Sequence + 100),
		NextTimestampMs: fmt.Sprint(pin.Timestamp.Add(time.Hour).UnixMilli()),
	}))
	fixture.rows = append(fixture.rows, objects...)
	for _, coin := range f.metadata {
		fixture.rows = append(fixture.rows, sqlFixtureRow("metadata", suiSQLMetadata{Status: "found", CoinType: coin.CoinType,
			Decimals: fmt.Sprint(coin.Decimals), Symbol: coin.Symbol, Name: coin.Name,
			Checkpoint: fmt.Sprint(pin.Sequence), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100)}))
	}
	for _, value := range values {
		fixture.rows = append(fixture.rows, sqlFixtureRow("value", value))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fixture.calls++
		response := map[string]any{"result": map[string]any{"rows": fixture.rows, "cursor": ""}}
		if fixture.mutate != nil {
			fixture.mutate(response)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	reader, err := NewNaviHistoryReader(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, reader
}

func naviEmodeValue(pin SuiCheckpoint, market, account string) suiSQLValue {
	identity := "emode:" + market + ":" + account
	return suiSQLValue{
		suiProtocolValue: suiProtocolValue{ID: fmt.Sprintf("emode:%020d:%s", pin.Sequence, identity), Kind: "emode",
			Account: account, Market: market, Content: `{"emode":"9","entered":true}`},
		Checkpoint: fmt.Sprint(pin.Sequence), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100),
	}
}

// The index and the node answer with the same positions for the same objects:
// both run naviLending over a suiProtocolState, and only its source differs.
// Head observations and stateMode necessarily differ and are not compared.
func TestNaviIndexedHistoryMatchesTheNodePath(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	node := latestFixture()
	node.addMarket(0, 1, owner.Hex(), "1000000000", false)
	node.addMarket(1, 1, naviAddress("0x22"), "2000000000", true)
	for _, id := range []string{"0x22", "0x23"} {
		object := latestObject(id, naviAccountType, "ADDRESS", owner.Hex(), map[string]any{"owner": naviAddress("0x22")})
		node.objects[object.ID] = object
	}
	want, err := NewSuiProtocolReader().ReadLatest(context.Background(), "navi", owner, node)
	if err != nil || len(want.Errors) > 0 || len(want.Groups) != 2 {
		t.Fatalf("node path: %+v %v", want, err)
	}

	indexed := latestFixture()
	indexed.addMarket(0, 1, owner.Hex(), "1000000000", false)
	indexed.addMarket(1, 1, naviAddress("0x22"), "2000000000", true)
	for _, id := range []string{"0x22", "0x23"} {
		object := latestObject(id, naviAccountType, "ADDRESS", owner.Hex(), map[string]any{"owner": naviAddress("0x22")})
		indexed.objects[object.ID] = object
	}
	pin := indexed.pin
	pin.Sequence = naviHistoryStart + 100
	fixture, reader := newNaviIndexFixture(t, indexed, []suiSQLValue{naviEmodeValue(pin, "1", naviAddress("0x22"))})
	got, err := reader.ReadAtCheckpoint(context.Background(), owner, pin.Sequence)
	if err != nil || len(got.Groups) != len(want.Groups) {
		t.Fatalf("indexed path: %+v %v", got, err)
	}
	if fixture.calls != 1 {
		t.Fatalf("expected one SQL call, got %d", fixture.calls)
	}
	if got.Checkpoint != pin {
		t.Fatalf("pin = %+v, want %+v", got.Checkpoint, pin)
	}
	for i := range want.Groups {
		a, b := want.Groups[i], got.Groups[i]
		if a.ID != b.ID || a.MarketID != b.MarketID || a.Label != b.Label || len(a.Components) != len(b.Components) {
			t.Fatalf("group %d: node %+v vs indexed %+v", i, a, b)
		}
		for j := range a.Components {
			if a.Components[j].AmountRaw != b.Components[j].AmountRaw || a.Components[j].Kind != b.Components[j].Kind ||
				a.Components[j].Coin.CoinType != b.Components[j].Coin.CoinType {
				t.Fatalf("group %s component %d: node %+v vs indexed %+v", a.ID, j, a.Components[j], b.Components[j])
			}
		}
		if b.Metadata["stateMode"] != "historical" || b.Metadata["materializedAtCheckpoint"] != fmt.Sprint(pin.Sequence+100) {
			t.Fatalf("indexed group %s lost its sample metadata: %+v", b.ID, b.Metadata)
		}
	}
}

// A principal is object-owned by its reserve table, so the only skip-indexed
// way to a wallet's positions is the owner index the processor writes for it.
// Scanning the kind's ID range is the shape that cost Suilend seconds a read,
// and the principal kind is an order of magnitude larger than any other.
func TestNaviIndexedSQLResolvesPrincipalsThroughTheOwnerIndex(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	index := &suiHistoryIndex{protocolID: "navi", start: naviHistoryStart}
	query, err := index.portfolioSQL(owner, suiSQLSelection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`FROM "PortfolioOwnerIndex_raw" WHERE ((id >= 'principal:' AND id <= 'principal:~')) AND owner IN (SELECT account FROM accounts)`,
		`objectId IN (SELECT arrayJoin(principal_candidates))`,
		`JSONExtractString(links,'supplyTableId')`,
		`JSONExtractString(links,'usersTableId')`,
		`AS observedValueCount`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query is missing %q:\n%s", want, query)
		}
	}
	// Every kind the processor certifies has to be counted back, or the sample
	// looks incomplete forever.
	for _, kind := range []string{"storage", "market", "reserve", "principal", "account", "vault", "receipt", "receiptState"} {
		if !strings.Contains(query, `id > (SELECT concat('`+kind+`:'`) {
			t.Fatalf("certificate count omits kind %q:\n%s", kind, query)
		}
	}
}

// NAVI does certify value rows, so a sample whose e-mode rows are not all
// visible yet is incomplete rather than silently read without them.
func TestNaviIndexedRejectsUncertifiedValues(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	f := latestFixture()
	f.addMarket(0, 0, owner.Hex(), "1000000000", false)
	fixture, reader := newNaviIndexFixture(t, f, nil)
	for i, row := range fixture.rows {
		var snapshot suiSQLSnapshot
		if row.RowType != "snapshot" || json.Unmarshal([]byte(row.Payload), &snapshot) != nil {
			continue
		}
		snapshot.ValueCount = "1"
		fixture.rows[i] = sqlFixtureRow("snapshot", snapshot)
	}
	if _, err := reader.ReadLatest(context.Background(), owner); err == nil {
		t.Fatal("a sample missing a certified e-mode row was accepted")
	}
}
