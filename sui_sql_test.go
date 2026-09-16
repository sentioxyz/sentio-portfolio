package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sqlPortfolioFixture struct {
	pin    SuiCheckpoint
	rows   []suiSQLRow
	calls  int
	mutate func(map[string]any)
}

func sqlFixtureRow(kind string, payload any) suiSQLRow {
	raw, _ := json.Marshal(payload)
	return suiSQLRow{kind, string(raw)}
}
func newSQLPortfolioFixture(t *testing.T, protocol string, base *latestSuiFixture) (*sqlPortfolioFixture, *suiHistoryIndex) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	starts := map[string]uint64{"navi": naviHistoryStart, "suilend": suilendHistoryStart, "volo-vaults": voloHistoryStart, "cetus": 1579561, "bluefin": 71783891}
	names := map[string]string{"navi": "NAVI", "suilend": "Suilend", "volo-vaults": "Volo Vaults", "cetus": "Cetus", "bluefin": "Bluefin"}
	pin := base.pin
	pin.Sequence = starts[protocol] + 100
	fixture := &sqlPortfolioFixture{pin: pin}
	fixture.rows = append(fixture.rows, sqlFixtureRow("snapshot", suiSQLSnapshot{ID: fmt.Sprintf("%020d", pin.Sequence), Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), Digest: suiTestDigest, SchemaVersion: "1", ObjectCount: "0", ValueCount: "0", ObservedObjectCount: "0", ObservedValueCount: "0", StartCheckpoint: fmt.Sprint(starts[protocol]), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), NextCheckpoint: fmt.Sprint(pin.Sequence + 100), NextTimestampMs: fmt.Sprint(pin.Timestamp.Add(time.Hour).UnixMilli())}))
	normalize := func(object SuiObject) (suiSQLObject, bool) {
		kind := ""
		switch protocol {
		case "suilend":
			for _, k := range []string{"cap", "market", "obligation"} {
				if strings.HasPrefix(object.ObjectType, suilendHistoryType(k)+"<") {
					kind = k
					break
				}
			}
		case "navi":
			for _, k := range []string{"storage", "market", "reserve", "principal", "account", "vault", "receipt", "receiptState"} {
				if suiObjectTypeMatches(object.ObjectType, naviHistoryType(k)) {
					kind = k
					break
				}
			}
		case "volo-vaults":
			for _, k := range []string{"vault", "receipt", "receiptState", "oracle", "oraclePrice", "navValue", "navTimestamp"} {
				if suiObjectTypeMatches(object.ObjectType, voloHistoryType(k)) {
					kind = k
					break
				}
			}
		default:
			if strings.Contains(object.ObjectType, "::position::PositionInfo") {
				kind = "accounting"
			} else if strings.Contains(object.ObjectType, "::position::Position") {
				kind = "position"
			} else if strings.Contains(object.ObjectType, "::pool::Pool") {
				kind = "pool"
			} else {
				kind = "tick"
			}
		}
		if kind == "" {
			return suiSQLObject{}, false
		}
		fields, _ := suiObjectFields(object.Content)
		key := ""
		related := ""
		switch kind {
		case "principal", "receiptState":
			key, _ = fields.address("name")
		case "reserve":
			v, _ := fields.uint("name")
			if v != nil {
				key = v.String()
			}
		case "market":
			if protocol == "navi" {
				value, _ := fields.object("value")
				v, _ := value.uint("market_id")
				if v != nil {
					key = v.String()
				}
			}
		case "receipt":
			related, _ = suiReceiptVault(fields, protocol)
		case "navValue", "navTimestamp":
			key, _ = fields.text("name")
			object.ID, _ = suiASCIIFieldID(object.Owner, key)
			fields["id"] = object.ID
			raw, _ := json.Marshal(fields)
			object.Content = string(raw)
		}
		if object.PreviousTransaction == "" {
			object.PreviousTransaction = suiTestDigest
		}
		if object.Digest == "" {
			object.Digest = suiTestDigest
		}
		return suiSQLObject{ID: naviVersionID(object.ID, object.Version, false), ObjectID: object.ID, Kind: kind, Version: fmt.Sprint(object.Version), Digest: object.Digest, State: "live", OwnerKind: object.OwnerKind, Owner: object.Owner, ObjectType: object.ObjectType, Content: object.Content, Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), TransactionDigest: object.PreviousTransaction, MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), ParentID: object.Owner, Key: key, RelatedID: related, Links: "{}"}, true
	}
	objects := map[string]suiSQLObject{}
	for _, object := range base.objects {
		if row, ok := normalize(object); ok {
			objects[row.ObjectID] = row
		}
	}
	for _, table := range base.tables {
		for _, object := range table {
			if row, ok := normalize(object); ok {
				objects[row.ObjectID] = row
			}
		}
	}
	for _, row := range objects {
		fixture.rows = append(fixture.rows, sqlFixtureRow("object", row))
		if row.Kind == "oraclePrice" {
			fixture.rows = append(fixture.rows, sqlFixtureRow("quote", row))
		}
	}
	for _, object := range base.versions {
		if row, ok := normalize(object); ok {
			fixture.rows = append(fixture.rows, sqlFixtureRow("quote", row))
		}
	}
	for _, coin := range base.metadata {
		fixture.rows = append(fixture.rows, sqlFixtureRow("metadata", suiSQLMetadata{Status: "found", CoinType: coin.CoinType, Decimals: fmt.Sprint(coin.Decimals), Symbol: coin.Symbol, Name: coin.Name, Checkpoint: fmt.Sprint(pin.Sequence), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100)}))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fixture.calls++
		var body struct {
			Version  uint64 `json:"version"`
			SQLQuery struct {
				SQL  string `json:"sql"`
				Size int    `json:"size"`
			} `json:"sqlQuery"`
			Sync bool `json:"sync_v1"`
		}
		if request.Method != "POST" || request.URL.Path != "/sql/execute" {
			t.Errorf("unexpected non-SQL request %s %s", request.Method, request.URL.Path)
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.Version != 1 || !body.Sync || body.SQLQuery.Size != suiSQLRowLimit || !strings.HasPrefix(body.SQLQuery.SQL, "WITH ") {
			t.Error("invalid version-pinned SQL request")
		}
		response := map[string]any{"result": map[string]any{"rows": fixture.rows, "cursor": ""}}
		if fixture.mutate != nil {
			fixture.mutate(response)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	reader, err := newSuiHistoryIndex(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"}, protocol, names[protocol], starts[protocol])
	if err != nil {
		t.Fatal(err)
	}
	return fixture, reader
}
func newSuilendIndexFixture(t *testing.T, base *latestSuiFixture) (*sqlPortfolioFixture, *SuilendHistoryReader) {
	f, r := newSQLPortfolioFixture(t, "suilend", base)
	return f, &SuilendHistoryReader{r}
}
func readSuilendFixture(t *testing.T, f *latestSuiFixture, owner SuiAddress) (SuiProtocolPositions, error) {
	fixture, r := newSuilendIndexFixture(t, f)
	result, err := r.ReadLatest(context.Background(), owner)
	if fixture.calls != 1 {
		t.Fatalf("expected one SQL call, got %d", fixture.calls)
	}
	return result, err
}
func readVoloFixture(t *testing.T, f *latestSuiFixture, owner SuiAddress) (SuiProtocolPositions, error) {
	fixture, r := newSQLPortfolioFixture(t, "volo-vaults", f)
	result, err := r.ReadLatest(context.Background(), owner)
	if fixture.calls != 1 {
		t.Fatalf("expected one SQL call, got %d", fixture.calls)
	}
	return result, err
}
func readCLMMFixture(t *testing.T, protocol string, f *latestSuiFixture, owner SuiAddress) (SuiProtocolPositions, error) {
	fixture, r := newSQLPortfolioFixture(t, protocol, f)
	result, err := r.ReadLatest(context.Background(), owner)
	if fixture.calls != 1 {
		t.Fatalf("expected one SQL call, got %d", fixture.calls)
	}
	return result, err
}

func TestSuiSQLFiveProtocolsSingleRequestNoNode(t *testing.T) {
	for _, protocol := range []string{"navi", "suilend", "cetus", "bluefin", "volo-vaults"} {
		t.Run(protocol, func(t *testing.T) {
			var base *latestSuiFixture
			var owner SuiAddress
			switch protocol {
			case "suilend":
				base, owner = suilendFixture()
			case "cetus", "bluefin":
				base, owner = clmmFixture(protocol)
			case "volo-vaults":
				base, owner = voloValuationFixture()
			default:
				base = latestFixture()
				owner, _ = ParseSuiAddress("0x11")
				base.addMarket(0, 0, owner.Hex(), "1000000000", false)
			}
			fixture, index := newSQLPortfolioFixture(t, protocol, base)
			for _, mode := range []string{"latest", "checkpoint", "time"} {
				var got SuiProtocolPositions
				var err error
				before := fixture.calls
				switch mode {
				case "latest":
					got, err = index.ReadLatest(context.Background(), owner)
				case "checkpoint":
					got, err = index.ReadAtCheckpoint(context.Background(), owner, fixture.pin.Sequence+1)
				default:
					got, err = index.ReadAtTime(context.Background(), owner, fixture.pin.Timestamp.Add(30*time.Minute))
				}
				if err != nil || len(got.Groups) != 1 || got.Checkpoint != fixture.pin || fixture.calls-before != 1 {
					t.Fatalf("%s %+v %v SQLcalls=%d", mode, got, err, fixture.calls-before)
				}
			}
		})
	}
}
func TestSuiSQLRejectsIncompleteEnvelopes(t *testing.T) {
	for _, scenario := range []string{"error", "cursor", "no sentinel", "duplicate sentinel", "truncated", "wrong schema", "unmaterialized", "missing object count", "missing value count", "extra object count", "absent count"} {
		t.Run(scenario, func(t *testing.T) {
			base, owner := suilendFixture()
			fixture, index := newSQLPortfolioFixture(t, "suilend", base)
			fixture.mutate = func(response map[string]any) {
				result := response["result"].(map[string]any)
				switch scenario {
				case "error":
					response["error"] = map[string]any{"message": "private"}
				case "cursor":
					result["cursor"] = "next"
				case "no sentinel":
					result["rows"] = fixture.rows[1:]
				case "duplicate sentinel":
					result["rows"] = append(fixture.rows, fixture.rows[0])
				case "truncated":
					result["truncated"] = true
				default:
					var s suiSQLSnapshot
					_ = json.Unmarshal([]byte(fixture.rows[0].Payload), &s)
					switch scenario {
					case "wrong schema":
						s.SchemaVersion = "0"
					case "missing object count":
						s.ObjectCount = "1"
					case "missing value count":
						s.ValueCount = "1"
					case "extra object count":
						s.ObservedObjectCount = "1"
					case "absent count":
						s.ObservedValueCount = ""
					default:
						s.MaterializedAtCheckpoint = s.Checkpoint
					}
					fixture.rows[0] = sqlFixtureRow("snapshot", s)
				}
			}
			got, err := index.ReadLatest(context.Background(), owner)
			if err == nil || len(got.Groups) != 0 || fixture.calls != 1 {
				t.Fatalf("accepted incomplete result: %+v %v", got, err)
			}
		})
	}
}
func TestSuiSQLHaltAndEmptyWallet(t *testing.T) {
	base, _ := suilendFixture()
	base.objects = map[string]SuiObject{}
	owner, _ := ParseSuiAddress("0x11")
	fixture, index := newSQLPortfolioFixture(t, "suilend", base)
	var snapshot suiSQLSnapshot
	_ = json.Unmarshal([]byte(fixture.rows[0].Payload), &snapshot)
	snapshot.NextTimestampMs = strconv.FormatInt(fixture.pin.Timestamp.Add(8*time.Hour).UnixMilli(), 10)
	fixture.rows[0] = sqlFixtureRow("snapshot", snapshot)
	got, err := index.ReadAtTime(context.Background(), owner, fixture.pin.Timestamp.Add(6*time.Hour))
	if err != nil || len(got.Groups) != 0 || got.Checkpoint != fixture.pin || fixture.calls != 1 {
		t.Fatalf("chain halt/empty: %+v %v", got, err)
	}
	if _, err = index.ReadAtTime(context.Background(), owner, fixture.pin.Timestamp.Add(8*time.Hour)); err == nil {
		t.Fatal("accepted next-hour boundary")
	}
}
func TestSuiSQLTerminalAndTransferredCaps(t *testing.T) {
	for _, state := range []string{"deleted", "wrapped", "transferred"} {
		t.Run(state, func(t *testing.T) {
			base, owner := suilendFixture()
			fixture, index := newSQLPortfolioFixture(t, "suilend", base)
			for i, row := range fixture.rows {
				if row.RowType != "object" {
					continue
				}
				var obj suiSQLObject
				_ = json.Unmarshal([]byte(row.Payload), &obj)
				if obj.Kind != "cap" {
					continue
				}
				if state == "transferred" {
					obj.Owner = naviAddress("0xff")
				} else {
					obj.State = state
					obj.Content = ""
				}
				fixture.rows[i] = sqlFixtureRow("object", obj)
			}
			got, err := index.ReadLatest(context.Background(), owner)
			if err != nil || len(got.Groups) != 0 || fixture.calls != 1 {
				t.Fatalf("lifecycle %+v %v", got, err)
			}
		})
	}
}
func TestSuiSQLUnconfiguredProtocolsNeverReadNode(t *testing.T) {
	base, owner := suilendFixture()
	node := &latestHeadFixture{latestSuiFixture: base}
	reader := NewSuiProtocolReader()
	for _, protocol := range reader.ProtocolIDs() {
		if _, err := reader.ReadLatest(context.Background(), protocol, owner, node); err == nil {
			t.Fatalf("%s accepted missing index", protocol)
		}
	}
	if node.headReads != 0 {
		t.Fatal("protocol invoked node")
	}
}

func TestSuiSQLCompletedSnapshotCertifiesEmptyWallet(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	for _, protocol := range []string{"navi", "suilend", "cetus", "bluefin", "volo-vaults"} {
		t.Run(protocol, func(t *testing.T) {
			base := latestFixture()
			base.metadata = nil
			fixture, index := newSQLPortfolioFixture(t, protocol, base)
			result, err := index.ReadLatest(context.Background(), owner)
			if err != nil || len(result.Groups) != 0 || result.Checkpoint != fixture.pin || fixture.calls != 1 {
				t.Fatalf("empty indexed wallet: %+v %v", result, err)
			}
		})
	}
}

func TestSuiSQLNeverRetriesHTTPExecution(t *testing.T) {
	for _, scenario := range []string{"server error", "rate limit", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "timeout":
					<-release
				case "rate limit":
					w.WriteHeader(http.StatusTooManyRequests)
				default:
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			reader, err := NewSuilendHistoryReader(SentioIndexerConfig{SQLURL: server.URL, ProcessorVersion: "1"})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "timeout" {
				reader.api.httpClient.Timeout = 20 * time.Millisecond
			}
			owner, _ := ParseSuiAddress("0x11")
			_, err = reader.ReadLatest(context.Background(), owner)
			close(release)
			if err == nil || calls.Load() != 1 {
				t.Fatalf("%s executed SQL %d times, error %v", scenario, calls.Load(), err)
			}
		})
	}
}
