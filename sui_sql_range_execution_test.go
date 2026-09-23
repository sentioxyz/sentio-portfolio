package portfolio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// suiSQLHistoryFixture is a processor index with several samples: one row per
// object version, the two read indexes next to it, and a certificate per sample
// that counts what the processor wrote in its interval.
type suiSQLHistoryFixture struct {
	objects, objectIndex, ownerIndex, values, metadata []map[string]string
	checkpoints                                        []int
}

// object writes one version. who is the owner for owned objects and the account a
// principal is named by; delay makes the row visible only at that checkpoint.
func (f *suiSQLHistoryFixture) object(id, kind string, cp, version int, state, who, related, parent, key, links string, delay int) map[string]string {
	terminal := state != "live"
	visible := cp + 1
	if delay > 0 {
		visible = delay
	}
	ownerKind := "ADDRESS"
	if kind == "principal" || kind == "receiptState" || kind == "reserve" || kind == "storage" || kind == "market" && who == "" {
		ownerKind = "OBJECT"
	}
	row := map[string]string{"id": fmt.Sprintf("%s:%020d:%s", kind, cp, suiTestVersionID(naviAddress(id), uint64(version), terminal)),
		"objectId": naviAddress(id), "kind": kind, "checkpoint": strconv.Itoa(cp), "version": strconv.Itoa(version),
		"state": state, "ownerKind": ownerKind, "owner": who, "relatedId": related, "parentId": parent,
		"key": key, "transactionDigest": suiTestDigest, "links": links, "materializedAtCheckpoint": strconv.Itoa(visible),
		"timestampMs": strconv.Itoa(cp * 1000), "content": fmt.Sprintf(`{"version":%d}`, version), "digest": suiTestDigest, "objectType": "0x1::test::Object"}
	f.objects = append(f.objects, row)
	tail := fmt.Sprintf("%020d:%020d:%d", cp, version, map[bool]int{false: 0, true: 1}[terminal])
	common := func(m map[string]string) map[string]string {
		m["objectId"], m["stateId"], m["checkpoint"], m["materializedAtCheckpoint"] = row["objectId"], row["id"], row["checkpoint"], row["materializedAtCheckpoint"]
		return m
	}
	f.objectIndex = append(f.objectIndex, common(map[string]string{"id": kind + ":" + row["objectId"] + ":" + tail, "kind": kind}))
	indexOwner := ""
	switch {
	case kind == "principal":
		indexOwner = key
	case ownerKind == "ADDRESS" && who != "":
		indexOwner = who
	}
	if indexOwner != "" {
		f.ownerIndex = append(f.ownerIndex, common(map[string]string{"id": kind + ":" + indexOwner + ":" + row["objectId"] + ":" + tail, "owner": indexOwner}))
	}
	return row
}

func (f *suiSQLHistoryFixture) value(account string, cp int, entered bool, delay int) {
	visible := cp + 1
	if delay > 0 {
		visible = delay
	}
	identity := "emode:0:" + account
	f.values = append(f.values, map[string]string{"id": fmt.Sprintf("emode:%020d:%s", cp, identity), "kind": "emode", "account": account, "market": "0",
		"content": fmt.Sprintf(`{"entered":%t}`, entered), "checkpoint": strconv.Itoa(cp), "materializedAtCheckpoint": strconv.Itoa(visible)})
}

func (f *suiSQLHistoryFixture) coin(coin string, cp, visible int, symbol string) {
	f.metadata = append(f.metadata, map[string]string{"id": fmt.Sprintf("%s:%020d", coin, cp), "status": "found", "coinType": coin, "symbol": symbol, "name": symbol,
		"decimals": "6", "checkpoint": strconv.Itoa(cp), "timestampMs": strconv.Itoa(cp * 1000), "materializedAtCheckpoint": strconv.Itoa(visible)})
}

// setup creates the tables. Each sample P owns (P-10, P]; its certificate counts
// the distinct rows of that interval visible by its bound, P+10.
func (f *suiSQLHistoryFixture) setup() string {
	snapshots := []map[string]string{}
	for i, cp := range f.checkpoints {
		previous := 0
		if i > 0 {
			previous = f.checkpoints[i-1]
		}
		next := cp + 10
		if i+1 < len(f.checkpoints) {
			next = f.checkpoints[i+1]
		}
		count := func(rows []map[string]string) int {
			ids := map[string]bool{}
			for _, row := range rows {
				source, _ := strconv.Atoi(row["checkpoint"])
				visible, _ := strconv.Atoi(row["materializedAtCheckpoint"])
				if source > previous && source <= cp && visible <= next {
					ids[row["id"]] = true
				}
			}
			return len(ids)
		}
		snapshots = append(snapshots, map[string]string{"id": fmt.Sprintf("%020d", cp), "checkpoint": strconv.Itoa(cp), "timestampMs": strconv.Itoa(cp * 1000), "digest": suiTestDigest,
			"schemaVersion": "2", "startCheckpoint": "1", "materializedAtCheckpoint": strconv.Itoa(next), "nextCheckpoint": strconv.Itoa(next),
			"nextTimestampMs": strconv.Itoa(next * 1000), "previousCheckpoint": strconv.Itoa(previous), "objectCount": strconv.Itoa(count(f.objects)), "valueCount": strconv.Itoa(count(f.values))})
	}
	setup := sqlTestTable("PortfolioObjectState_raw", "id objectId kind digest state ownerKind owner objectType content transactionDigest parentId key relatedId links", "version checkpoint timestampMs materializedAtCheckpoint", f.objects)
	setup += sqlTestTable("PortfolioObjectIndex_raw", "id objectId kind stateId", "checkpoint materializedAtCheckpoint", f.objectIndex)
	setup += sqlTestTable("PortfolioOwnerIndex_raw", "id owner objectId stateId", "checkpoint materializedAtCheckpoint", f.ownerIndex)
	setup += sqlTestTable("PortfolioSnapshot", "id digest", "checkpoint timestampMs schemaVersion startCheckpoint materializedAtCheckpoint nextCheckpoint nextTimestampMs previousCheckpoint objectCount valueCount", snapshots)
	setup += sqlTestTable("PortfolioValue", "id kind account market content", "checkpoint materializedAtCheckpoint", f.values)
	setup += sqlTestTable("PortfolioValue_raw", "id kind account market content", "checkpoint materializedAtCheckpoint", f.values)
	setup += sqlTestTable("PortfolioTokenMetadata", "id status coinType symbol name", "decimals checkpoint timestampMs materializedAtCheckpoint", f.metadata)
	return setup
}

func runSuiSQL(t *testing.T, binary, setup, query string) []suiSQLRow {
	t.Helper()
	cmd := exec.Command(binary, "local", "--multiquery", "--query", setup+query+" FORMAT JSONEachRow")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("SQL execution: %v %s", err, stderr.String())
	}
	rows := []suiSQLRow{}
	for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var row suiSQLRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

func normalizeSuiSQLResult(r suiSQLResult) suiSQLResult {
	sort.Slice(r.objects, func(i, j int) bool { return r.objects[i].ObjectID < r.objects[j].ObjectID })
	sort.Slice(r.metadata, func(i, j int) bool { return r.metadata[i].CoinType < r.metadata[j].CoinType })
	sort.Slice(r.values, func(i, j int) bool { return r.values[i].ID < r.values[j].ID })
	if len(r.objects) == 0 {
		r.objects = nil
	}
	if len(r.metadata) == 0 {
		r.metadata = nil
	}
	if len(r.values) == 0 {
		r.values = nil
	}
	return r
}

func suilendHistoryFixture(owner, other string) *suiSQLHistoryFixture {
	f := &suiSQLHistoryFixture{checkpoints: []int{10, 20, 30, 40, 50}}
	// A capability transferred away and back; it names one obligation, and the
	// obligation moves to a second market between samples.
	f.object("0x11", "cap", 5, 1, "live", owner, naviAddress("0x22"), "", "", "{}", 0)
	f.object("0x11", "cap", 24, 2, "live", other, naviAddress("0x22"), "", "", "{}", 0)
	f.object("0x11", "cap", 43, 3, "live", owner, naviAddress("0x22"), "", "", "{}", 0)
	// A second capability of the owner, deleted before the last sample.
	f.object("0x12", "cap", 15, 1, "live", owner, naviAddress("0x23"), "", "", "{}", 0)
	f.object("0x12", "cap", 35, 2, "deleted", "", "", "", "", "{}", 0)
	for i, cp := range []int{6, 12, 19, 28, 33, 41, 49} {
		market := naviAddress("0x44")
		if cp > 30 {
			market = naviAddress("0x45")
		}
		f.object("0x22", "obligation", cp, i+1, "live", "", market, "", "", "{}", 0)
	}
	f.object("0x23", "obligation", 15, 1, "live", "", naviAddress("0x44"), "", "", "{}", 0)
	for i, cp := range []int{2, 9, 11, 18, 21, 29, 32, 38, 44, 50} {
		f.object("0x44", "market", cp, i+1, "live", "", "", "", "", "{}", 0)
		f.object("0x45", "market", cp, i+1, "live", "", "", "", "", "{}", 0)
	}
	// A version only visible after its own sample's bound, and replays of rows.
	f.object("0x22", "obligation", 37, 20, "live", "", naviAddress("0x45"), "", "", "{}", 55)
	replay := f.object("0x44", "market", 20, 30, "live", "", "", "", "", "{}", 0)
	late := map[string]string{}
	for k, v := range replay {
		late[k] = v
	}
	late["materializedAtCheckpoint"] = "45"
	f.objects = append(f.objects, replay, late)
	f.coin("0xc::a::A", 3, 4, "A")
	f.coin("0xc::b::B", 26, 27, "B")
	f.coin("0xc::a::A", 36, 60, "A2")
	return f
}

func naviHistoryFixture(owner, other, account string) *suiSQLHistoryFixture {
	f := &suiSQLHistoryFixture{checkpoints: []int{10, 20, 30, 40, 50}}
	// An account capability transferred away and back, and a vault receipt that
	// is deleted between samples.
	f.object("0x11", "account", 5, 1, "live", owner, "", "", "", `{"accountAddress":"`+account+`"}`, 0)
	f.object("0x11", "account", 25, 2, "live", other, "", "", "", `{"accountAddress":"`+account+`"}`, 0)
	f.object("0x11", "account", 45, 3, "live", owner, "", "", "", `{"accountAddress":"`+account+`"}`, 0)
	f.object("0x12", "receipt", 15, 1, "live", owner, naviAddress("0x31"), "", "", "{}", 0)
	f.object("0x12", "receipt", 35, 2, "deleted", "", "", "", "", "{}", 0)
	for i, cp := range []int{5, 22, 38} {
		f.object("0x31", "vault", cp, i+1, "live", "", "", "", "", `{"usersTableId":"`+naviAddress("0x71")+`"}`, 0)
	}
	f.object("0x41", "storage", 1, 1, "live", "", "", "", "", "{}", 0)
	f.object("0x46", "market", 3, 1, "live", "", "", "", "", "{}", 0)
	f.object("0x46", "market", 24, 2, "live", "", "", "", "", "{}", 0)
	tables := `{"supplyTableId":"` + naviAddress("0x81") + `","borrowTableId":"` + naviAddress("0x82") + `"}`
	for i, cp := range []int{1, 12, 18, 33, 47} {
		f.object("0x42", "reserve", cp, i+1, "live", "", "", "", "", tables, 0)
	}
	f.object("0x43", "reserve", 1, 1, "live", "", "", "", "", `{"supplyTableId":"`+naviAddress("0x83")+`","borrowTableId":"`+naviAddress("0x84")+`"}`, 0)
	f.object("0x43", "reserve", 29, 2, "deleted", "", "", "", "", "{}", 0)
	// Principals named by the wallet, by its account and by another wallet, one of
	// them under the reserve deleted at 29.
	for i, cp := range []int{8, 16, 27, 44} {
		f.object("0x51", "principal", cp, i+1, "live", "", "", naviAddress("0x81"), owner, "{}", 0)
	}
	f.object("0x51", "principal", 19, 9, "live", "", "", naviAddress("0x81"), owner, "{}", 99)
	for i, cp := range []int{14, 36} {
		f.object("0x52", "principal", cp, i+1, "live", "", "", naviAddress("0x82"), account, "{}", 0)
	}
	f.object("0x53", "principal", 12, 1, "live", "", "", naviAddress("0x81"), other, "{}", 0)
	f.object("0x54", "principal", 7, 1, "live", "", "", naviAddress("0x83"), owner, "{}", 0)
	for i, cp := range []int{17, 26} {
		f.object("0x61", "receiptState", cp, i+1, "live", "", "", naviAddress("0x71"), naviAddress("0x12"), "{}", 0)
	}
	replay := f.object("0x42", "reserve", 34, 9, "live", "", "", "", "", tables, 0)
	late := map[string]string{}
	for k, v := range replay {
		late[k] = v
	}
	late["materializedAtCheckpoint"] = "46"
	f.objects = append(f.objects, replay, late)
	f.value(account, 13, true, 0)
	f.value(account, 37, false, 0)
	f.value(account, 42, true, 99)
	f.value(other, 21, true, 0)
	f.coin("0xc::a::A", 3, 4, "A")
	f.coin("0xc::b::B", 26, 27, "B")
	return f
}

// Opt in with PORTFOLIO_CLICKHOUSE_BINARY. Every sample a range statement returns
// must be exactly what portfolioSQL returns for it: the same certificate and
// counts, the same objects at the same versions and visibility, the same metadata
// and values. Windows start at every sample and hold one, two or all samples, so
// newest-version buckets see a different history at each start.
func TestSuiSQLRangeMatchesPointStatements(t *testing.T) {
	binary := os.Getenv("PORTFOLIO_CLICKHOUSE_BINARY")
	if binary == "" {
		t.Skip("set PORTFOLIO_CLICKHOUSE_BINARY to execute SQL")
	}
	owner, _ := ParseSuiAddress("0xa")
	other, _ := ParseSuiAddress("0xb")
	account, _ := ParseSuiAddress("0xacc")
	for _, tc := range []struct {
		protocol string
		fixture  *suiSQLHistoryFixture
	}{
		{"suilend", suilendHistoryFixture(owner.Hex(), other.Hex())},
		{"navi", naviHistoryFixture(owner.Hex(), other.Hex(), account.Hex())},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			setup := tc.fixture.setup()
			index := &suiHistoryIndex{protocolID: tc.protocol, start: 1}
			// The fixture must exercise every selection stage: each kind is read
			// at some sample, and some object is read at one sample but not another.
			seen, varies := map[string]bool{}, false
			defer func() {
				kinds, _ := suiSQLProtocolKinds(tc.protocol)
				for _, kind := range append(kinds.all(), kinds.values...) {
					if !seen[kind] && !t.Failed() {
						t.Errorf("fixture never selects %s", kind)
					}
				}
				if !varies && !t.Failed() {
					t.Error("fixture selects the same objects at every sample")
				}
			}()
			for _, who := range []SuiAddress{owner, other, account} {
				point := map[int]suiSQLResult{}
				for _, cp := range tc.fixture.checkpoints {
					at := time.UnixMilli(int64(cp * 1000))
					query, err := index.portfolioSQL(who, suiSQLSelection{at: &at})
					if err != nil {
						t.Fatal(err)
					}
					decoded, err := decodeSuiSQLRows(runSuiSQL(t, binary, setup, query))
					if err != nil {
						t.Fatal(err)
					}
					point[cp] = normalizeSuiSQLResult(decoded)
					for _, o := range point[cp].objects {
						seen[o.Kind] = true
					}
					for _, v := range point[cp].values {
						seen[v.Kind] = true
					}
					if first := point[tc.fixture.checkpoints[0]]; len(first.objects) != len(point[cp].objects) {
						varies = true
					}
				}
				for first := range tc.fixture.checkpoints {
					for _, limit := range []int{1, 2, len(tc.fixture.checkpoints)} {
						from := time.UnixMilli(int64(tc.fixture.checkpoints[first] * 1000))
						to := time.UnixMilli(int64(tc.fixture.checkpoints[len(tc.fixture.checkpoints)-1] * 1000))
						query, err := index.rangeSQL(who, from, to, limit)
						if err != nil {
							t.Fatal(err)
						}
						decoded, err := decodeSuiSQLRange(runSuiSQL(t, binary, setup, query))
						if err != nil {
							t.Fatal(err)
						}
						view, err := newSuiSQLRangeView(decoded)
						if err != nil {
							t.Fatal(err)
						}
						if want := min(limit, len(tc.fixture.checkpoints)-first); len(decoded.samples) != want {
							t.Fatalf("%s from %d limit %d: %d samples, want %d", who.Hex(), first, limit, len(decoded.samples), want)
						}
						for _, snapshot := range decoded.samples {
							cp, _ := strconv.Atoi(snapshot.Checkpoint)
							got, err := view.sample(tc.protocol, who, snapshot)
							if err != nil {
								t.Fatal(err)
							}
							if got = normalizeSuiSQLResult(got); !reflect.DeepEqual(got, point[cp]) {
								t.Fatalf("%s at %d (window from %d, limit %d): %s", who.Hex(), cp, first, limit, diffSuiSQLResults(got, point[cp]))
							}
						}
					}
				}
			}
		})
	}
}

func diffSuiSQLResults(got, want suiSQLResult) string {
	out := ""
	if !reflect.DeepEqual(got.snapshots, want.snapshots) {
		out += fmt.Sprintf("\nsnapshot range %+v\n         point %+v", got.snapshots, want.snapshots)
	}
	objects := func(r suiSQLResult) map[string]suiSQLObject {
		m := map[string]suiSQLObject{}
		for _, o := range r.objects {
			m[o.ObjectID] = o
		}
		return m
	}
	g, w := objects(got), objects(want)
	for id, o := range g {
		if p, ok := w[id]; !ok {
			out += fmt.Sprintf("\nonly in range: %s %s %s mat %s", o.Kind, id[len(id)-4:], o.ID[:30], o.MaterializedAtCheckpoint)
		} else if !reflect.DeepEqual(o, p) {
			out += fmt.Sprintf("\ndiffers: %s %s range %s/%s point %s/%s", o.Kind, id[len(id)-4:], o.ID[:30], o.MaterializedAtCheckpoint, p.ID[:30], p.MaterializedAtCheckpoint)
		}
	}
	for id, o := range w {
		if _, ok := g[id]; !ok {
			out += fmt.Sprintf("\nonly in point: %s %s %s mat %s", o.Kind, id[len(id)-4:], o.ID[:30], o.MaterializedAtCheckpoint)
		}
	}
	if !reflect.DeepEqual(got.metadata, want.metadata) {
		out += fmt.Sprintf("\nmetadata range %+v\n         point %+v", got.metadata, want.metadata)
	}
	if !reflect.DeepEqual(got.values, want.values) {
		out += fmt.Sprintf("\nvalues range %+v\n       point %+v", got.values, want.values)
	}
	return out
}
