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

// cetusIndexRows restates the processor's projection: which kind an object type is, and which
// key, relation and links its state row carries — a position names its pool and the derived IDs
// of its PositionInfo record and boundary ticks. Feeding the node fixture's own objects through it
// is what lets one holding be read both ways and compared.
func cetusIndexRows(t *testing.T, f *latestSuiFixture, pin SuiCheckpoint) []suiSQLRow {
	t.Helper()
	content := func(object SuiObject) map[string]any {
		var out map[string]any
		if err := json.Unmarshal([]byte(object.Content), &out); err != nil {
			t.Fatalf("fixture content is not an object: %v", err)
		}
		return out
	}
	table := func(value any, field string) string {
		return naviAddress(fmt.Sprint(value.(map[string]any)[field].(map[string]any)["id"]))
	}
	tick := func(value any) int32 {
		return int32(uint32(value.(map[string]any)["bits"].(float64)))
	}
	coin := func(value any) string {
		if fields, ok := value.(map[string]any); ok {
			value = fields["name"]
		}
		normalized, err := NormalizeMoveType(fmt.Sprint(value))
		if err != nil {
			t.Fatal(err)
		}
		return normalized
	}
	pools := map[string]map[string]any{}
	for _, object := range f.objects {
		if strings.HasPrefix(object.ObjectType, suiType(cetusCLMMPackage+"::pool::Pool")+"<") {
			pools[object.ID] = content(object)
		}
	}
	infoType := suiFieldType("0x2::object::ID", cetusTablePackage+"::linked_table::Node<0x2::object::ID,"+cetusCLMMPackage+"::position::PositionInfo>")
	tickType := suiFieldType("u64", cetusTablePackage+"::skip_list::Node<"+cetusCLMMPackage+"::tick::Tick>")
	rows := []suiSQLRow{}
	for _, object := range f.objects {
		fields := content(object)
		kind, key, related, parent := "", "", "", ""
		links := map[string]any{}
		switch {
		case object.ObjectType == suiType(cetusCLMMPackage+"::position::Position"):
			kind, related = "position", naviAddress(fmt.Sprint(fields["pool"]))
			pool := pools[related]
			ticks, positions := table(pool["tick_manager"], "ticks"), table(pool["position_manager"], "positions")
			lower, upper := tick(fields["tick_lower_index"]), tick(fields["tick_upper_index"])
			address, _ := ParseSuiAddress(object.ID)
			info, _ := suiCLMMFieldID(positions, "0x2::object::ID", address[:])
			lowerID, _, _, _ := suiCLMMTickKey(ticks, lower, cetusCLMMPackage)
			upperID, _, _, _ := suiCLMMTickKey(ticks, upper, cetusCLMMPackage)
			links = map[string]any{"coinTypes": []string{coin(fields["coin_type_a"]), coin(fields["coin_type_b"])}, "lowerTick": fmt.Sprint(lower),
				"upperTick": fmt.Sprint(upper), "positionInfoId": info, "lowerTickId": lowerID, "upperTickId": upperID}
		case strings.HasPrefix(object.ObjectType, suiType(cetusCLMMPackage+"::pool::Pool")+"<"):
			kind = "pool"
			coins, rewards := []string{}, []string{}
			pair := strings.TrimSuffix(strings.SplitN(object.ObjectType, "<", 2)[1], ">")
			coins = append(coins, strings.Split(pair, ",")...)
			for _, reward := range fields["rewarder_manager"].(map[string]any)["rewarders"].([]any) {
				rewards = append(rewards, coin(reward.(map[string]any)["reward_coin"]))
			}
			links = map[string]any{"tickTableId": table(fields["tick_manager"], "ticks"), "positionTableId": table(fields["position_manager"], "positions"),
				"coinTypes": append(coins, rewards...), "rewardCoinTypes": rewards}
		case object.ObjectType == infoType:
			kind, key, parent = "positionInfo", naviAddress(fmt.Sprint(fields["name"])), object.Owner
		case object.ObjectType == tickType:
			kind, parent = "tick", object.Owner
			key = fmt.Sprint(tick(fields["value"].(map[string]any)["value"].(map[string]any)["index"]))
		default:
			continue
		}
		encoded, _ := json.Marshal(links)
		rows = append(rows, sqlFixtureRow("object", suiSQLObject{
			ID:       fmt.Sprintf("%s:%020d:%s", kind, pin.Sequence, suiTestVersionID(object.ID, object.Version, false)),
			ObjectID: object.ID, Kind: kind, Version: fmt.Sprint(object.Version), Digest: suiTestDigest, State: "live",
			OwnerKind: object.OwnerKind, Owner: object.Owner, ObjectType: object.ObjectType, Content: object.Content,
			Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()),
			TransactionDigest: suiTestDigest, MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100),
			ParentID: parent, Key: key, RelatedID: related, Links: string(encoded),
		}))
	}
	return rows
}

// newCetusIndexFixture serves those rows as the one SQL statement the reader issues, with a
// certificate that counts exactly what the rows contain.
func newCetusIndexFixture(t *testing.T, f *latestSuiFixture) (*sqlPortfolioFixture, *suiHistoryIndex, SuiCheckpoint) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	pin := f.pin
	pin.Sequence = cetusHistoryStart + 100
	fixture := &sqlPortfolioFixture{pin: pin}
	objects := cetusIndexRows(t, f, pin)
	fixture.rows = append(fixture.rows, sqlFixtureRow("snapshot", suiSQLSnapshot{
		ID: fmt.Sprintf("%020d", pin.Sequence), Checkpoint: fmt.Sprint(pin.Sequence),
		TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), Digest: suiTestDigest, SchemaVersion: "2",
		ObjectCount: fmt.Sprint(len(objects)), ObservedObjectCount: fmt.Sprint(len(objects)), ValueCount: "0", ObservedValueCount: "0",
		StartCheckpoint: fmt.Sprint(cetusHistoryStart), PreviousCheckpoint: fmt.Sprint(cetusHistoryStart - 1),
		MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), NextCheckpoint: fmt.Sprint(pin.Sequence + 100),
		NextTimestampMs: fmt.Sprint(pin.Timestamp.Add(24 * time.Hour).UnixMilli()),
	}))
	fixture.rows = append(fixture.rows, objects...)
	for _, coin := range f.metadata {
		fixture.rows = append(fixture.rows, sqlFixtureRow("metadata", suiSQLMetadata{Status: "found", CoinType: coin.CoinType,
			Decimals: fmt.Sprint(coin.Decimals), Symbol: coin.Symbol, Name: coin.Name,
			Checkpoint: fmt.Sprint(pin.Sequence), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100)}))
	}
	server := httptest.NewServer(asyncSQLHandler(t, func(w http.ResponseWriter, request *http.Request) {
		fixture.calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"rows": fixture.rows, "cursor": ""}})
	}))
	t.Cleanup(server.Close)
	reader, err := newCetusIndex(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	fastSQL(reader.api)
	return fixture, reader, pin
}

// The index and the node answer with the same positions for the same objects: both run
// loadSuiCLMM and suiCLMMGroups, and only where the objects come from differs. The fixture's
// PositionInfo liquidity is what both read, rewards accrue to the same timestamp, and head
// observations and stateMode necessarily differ and are not compared.
func TestCetusIndexedHistoryMatchesTheNodePath(t *testing.T) {
	node, owner := clmmFixture("cetus")
	want, err := newSuiNodeProtocols().ReadLatest(context.Background(), "cetus", owner, node)
	if err != nil || len(want.Errors) > 0 || len(want.Groups) != 1 {
		t.Fatalf("node path: %+v %v", want, err)
	}
	indexed, _ := clmmFixture("cetus")
	fixture, reader, pin := newCetusIndexFixture(t, indexed)
	got, err := reader.ReadAtCheckpoint(context.Background(), owner, pin.Sequence)
	if err != nil || len(got.Groups) != len(want.Groups) {
		t.Fatalf("indexed path: %+v %v", got, err)
	}
	if fixture.calls != 1 || got.Checkpoint != pin {
		t.Fatalf("calls %d, pin %+v want %+v", fixture.calls, got.Checkpoint, pin)
	}
	for i := range want.Groups {
		a, b := want.Groups[i], got.Groups[i]
		if a.ID != b.ID || a.MarketID != b.MarketID || a.Label != b.Label || len(a.Components) != len(b.Components) {
			t.Fatalf("group %d: node %+v vs indexed %+v", i, a, b)
		}
		for j := range a.Components {
			if a.Components[j].AmountRaw != b.Components[j].AmountRaw || a.Components[j].Kind != b.Components[j].Kind ||
				a.Components[j].Coin != b.Components[j].Coin {
				t.Fatalf("group %s component %d: node %+v vs indexed %+v", a.ID, j, a.Components[j], b.Components[j])
			}
		}
		for _, field := range []string{"positionId", "positionInfoId", "liquidity", "tickLower", "tickUpper", "currentTick", "sqrtPriceX64", "ownership"} {
			if fmt.Sprint(a.Metadata[field]) != fmt.Sprint(b.Metadata[field]) {
				t.Fatalf("group %s metadata %s: node %v vs indexed %v", a.ID, field, a.Metadata[field], b.Metadata[field])
			}
		}
		if b.Metadata["stateMode"] != "historical" || b.Metadata["materializedAtCheckpoint"] != fmt.Sprint(pin.Sequence+100) ||
			b.Metadata["sampleIntervalSeconds"] != int64(86400) {
			t.Fatalf("indexed group %s lost its sample metadata: %+v", b.ID, b.Metadata)
		}
	}
}

// A position with liquidity is priced from its boundary ticks, so a sample that holds the position
// without one of them fails rather than reporting principal and fees computed without it. A
// position the wallet no longer holds at the sample reads as nothing.
func TestCetusIndexedSampleFailsClosedAndFollowsOwnership(t *testing.T) {
	f, owner := clmmFixture("cetus")
	fixture, reader, pin := newCetusIndexFixture(t, f)
	full := fixture.rows
	tickType := suiFieldType("u64", cetusTablePackage+"::skip_list::Node<"+cetusCLMMPackage+"::tick::Tick>")
	dropped := false
	fixture.rows = nil
	for _, row := range full {
		var object suiSQLObject
		if !dropped && row.RowType == "object" && json.Unmarshal([]byte(row.Payload), &object) == nil && object.ObjectType == tickType {
			dropped = true
			continue
		}
		fixture.rows = append(fixture.rows, row)
	}
	if !dropped {
		t.Fatal("fixture has no tick row")
	}
	if got, err := reader.ReadAtCheckpoint(context.Background(), owner, pin.Sequence); err == nil {
		t.Fatalf("a sample missing a boundary tick was read: %+v", got)
	}
	fixture.rows = full
	other, _ := ParseSuiAddress("0x12")
	if got, err := reader.ReadAtCheckpoint(context.Background(), other, pin.Sequence); err != nil || len(got.Groups) != 0 {
		t.Fatalf("another wallet read %+v %v", got, err)
	}
}

// PositionInfo records and ticks are owned by the pool's tables, never by the wallet, so the
// statement reaches them only through the IDs each position links, after selecting the position's
// latest state. It counts every kind the processor certifies, and reads only the coins of the
// pools it selected: anyone can open a pool, and the whole metadata table is past a statement's
// row limit.
func TestCetusIndexedSQLFollowsPositionLinks(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	index := &suiHistoryIndex{protocolID: "cetus", start: cetusHistoryStart}
	point, err := index.portfolioSQL(owner, suiSQLSelection{})
	if err != nil {
		t.Fatal(err)
	}
	ranged, err := index.rangeSQL(owner, time.UnixMilli(1_700_000_000_000), time.UnixMilli(1_700_086_400_000), 2)
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{"point": point, "range": ranged} {
		for _, want := range []string{
			`(id > 'position:` + owner.Hex() + `:' AND id <= 'position:` + owner.Hex() + `:~')`,
			`objectId IN (SELECT arrayJoin([JSONExtractString(links,'positionInfoId'),JSONExtractString(links,'lowerTickId'),JSONExtractString(links,'upperTickId')]) FROM roots)`,
			`coinType IN (SELECT arrayJoin(JSONExtract(links,'coinTypes','Array(String)')) FROM direct_states WHERE kind='pool')`,
		} {
			if !strings.Contains(query, want) {
				t.Fatalf("%s query is missing %q:\n%s", name, want, query)
			}
		}
		for _, kind := range []string{"position", "pool", "positionInfo", "tick"} {
			if !strings.Contains(query, `concat('`+kind+`:'`) {
				t.Fatalf("%s certificate count omits kind %q:\n%s", name, kind, query)
			}
		}
	}
	// The other indexes keep reading their few coins whole.
	navi := &suiHistoryIndex{protocolID: "navi", start: naviHistoryStart}
	if query, _ := navi.portfolioSQL(owner, suiSQLSelection{}); strings.Contains(query, `JSONExtract(links,'coinTypes'`) {
		t.Fatal("NAVI's metadata read was restricted")
	}
}

// Cetus history is answered from its index and its head from the node, like NAVI.
func TestCetusRoutesItsPastToTheIndex(t *testing.T) {
	for _, route := range suiRoutes {
		if route.id == "cetus" && (route.latest != suiFromNode || route.pinned != suiFromIndex) {
			t.Fatalf("cetus route %+v", route)
		}
	}
	if suiIndexes["cetus"] == nil {
		t.Fatal("cetus index is not registered")
	}
}
