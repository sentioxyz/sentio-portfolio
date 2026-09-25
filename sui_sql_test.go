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
	return suiSQLRow{RowType: kind, Payload: string(raw)}
}

// suiTestVersionID spells the immutable version suffix of a state row ID the
// way the processor writes it: fixed-width version, then the terminal flag.
func suiTestVersionID(id string, version uint64, terminal bool) string {
	suffix := "0"
	if terminal {
		suffix = "1"
	}
	return fmt.Sprintf("%s:%020d:%s", id, version, suffix)
}

func newSQLPortfolioFixture(t *testing.T, protocol string, base *latestSuiFixture) (*sqlPortfolioFixture, *suiHistoryIndex) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	start := suilendHistoryStart
	pin := base.pin
	pin.Sequence = start + 100
	fixture := &sqlPortfolioFixture{pin: pin}
	fixture.rows = append(fixture.rows, sqlFixtureRow("snapshot", suiSQLSnapshot{ID: fmt.Sprintf("%020d", pin.Sequence), Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), Digest: suiTestDigest, SchemaVersion: "2", ObjectCount: "0", ValueCount: "0", ObservedObjectCount: "0", ObservedValueCount: "0", StartCheckpoint: fmt.Sprint(start), PreviousCheckpoint: fmt.Sprint(start - 1), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), NextCheckpoint: fmt.Sprint(pin.Sequence + 100), NextTimestampMs: fmt.Sprint(pin.Timestamp.Add(time.Hour).UnixMilli())}))
	normalize := func(object SuiObject) (suiSQLObject, bool) {
		kind := ""
		for _, k := range []string{"cap", "market", "obligation"} {
			if strings.HasPrefix(object.ObjectType, suilendHistoryType(k)+"<") {
				kind = k
				break
			}
		}
		if kind == "" {
			return suiSQLObject{}, false
		}
		key, related := "", ""
		if object.PreviousTransaction == "" {
			object.PreviousTransaction = suiTestDigest
		}
		if object.Digest == "" {
			object.Digest = suiTestDigest
		}
		return suiSQLObject{ID: fmt.Sprintf("%s:%020d:%s", kind, pin.Sequence, suiTestVersionID(object.ID, object.Version, false)), ObjectID: object.ID, Kind: kind, Version: fmt.Sprint(object.Version), Digest: object.Digest, State: "live", OwnerKind: object.OwnerKind, Owner: object.Owner, ObjectType: object.ObjectType, Content: object.Content, Checkpoint: fmt.Sprint(pin.Sequence), TimestampMs: fmt.Sprint(pin.Timestamp.UnixMilli()), TransactionDigest: object.PreviousTransaction, MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100), ParentID: object.Owner, Key: key, RelatedID: related, Links: "{}"}, true
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
	}
	for _, coin := range base.metadata {
		fixture.rows = append(fixture.rows, sqlFixtureRow("metadata", suiSQLMetadata{Status: "found", CoinType: coin.CoinType, Decimals: fmt.Sprint(coin.Decimals), Symbol: coin.Symbol, Name: coin.Name, Checkpoint: fmt.Sprint(pin.Sequence), MaterializedAtCheckpoint: fmt.Sprint(pin.Sequence + 100)}))
	}
	server := httptest.NewServer(asyncSQLHandler(t, func(w http.ResponseWriter, request *http.Request) {
		fixture.calls++
		var body struct {
			Version  uint64 `json:"version"`
			SQLQuery struct {
				SQL  string `json:"sql"`
				Size int    `json:"size"`
			} `json:"sqlQuery"`
			Engine string `json:"engine"`
		}
		if request.Method != "POST" || request.URL.Path != "/sql/execute" {
			t.Errorf("unexpected non-SQL request %s %s", request.Method, request.URL.Path)
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil || body.Version != 1 || body.Engine != "LARGE" || body.SQLQuery.Size != suiSQLRowLimit || !strings.HasPrefix(body.SQLQuery.SQL, "WITH ") {
			t.Error("invalid version-pinned SQL request")
		}
		response := map[string]any{"result": map[string]any{"rows": fixture.rows, "cursor": ""}}
		if fixture.mutate != nil {
			fixture.mutate(response)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	reader, err := newSuiHistoryIndex(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"}, protocol, "Suilend", start)
	if err != nil {
		t.Fatal(err)
	}
	fastSQL(reader.api)
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

// Latest and historical reads are the same statement under a different sample
// selection, so one SQL execution answers either, and neither touches a node.
func TestSuiSQLLatestAndHistoryAreOneRequestWithNoNode(t *testing.T) {
	base, owner := suilendFixture()
	fixture, index := newSQLPortfolioFixture(t, "suilend", base)
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
}

// Both dependency levels resolve through the narrow index tables rather than a
// scan of a kind's ID range: that scan is what cost seconds per stage at six
// million state rows.
func TestSuiSQLUsesReadIndexes(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	index := &suiHistoryIndex{protocolID: "suilend", start: suilendHistoryStart}
	query, err := index.portfolioSQL(owner, suiSQLSelection{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`FROM "PortfolioOwnerIndex_raw"`,
		`FROM "PortfolioObjectIndex_raw"`,
		`(id > 'cap:` + owner.Hex() + `:'`,
		`) AS root_candidates`,
		`second_states AS (`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("indexed query is missing %q", want)
		}
	}
	if strings.Contains(query, `max(id) AS selectedId`) {
		t.Fatal("a dependency level still scans a kind ID range")
	}
}

func TestSuiSQLRejectsIncompleteEnvelopes(t *testing.T) {
	for _, scenario := range []string{"error", "cursor", "no sentinel", "duplicate sentinel", "truncated", "wrong schema", "unmaterialized", "missing object count", "certified value rows", "absent count", "missing previous checkpoint", "reversed interval", "before publication", "timestamp overflow"} {
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
						s.SchemaVersion = "1"
					case "missing object count":
						s.ObjectCount = "1"
					case "certified value rows":
						s.ValueCount = "1"
					case "absent count":
						s.ObservedObjectCount = ""
					case "missing previous checkpoint":
						s.PreviousCheckpoint = ""
					case "reversed interval":
						s.PreviousCheckpoint = s.Checkpoint
					case "before publication":
						s.PreviousCheckpoint = "0"
					case "timestamp overflow":
						s.NextTimestampMs = "18446744073709551615"
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
func TestSuiSQLAcceptsUndercountedCertificate(t *testing.T) {
	// The processor counts its rows with a store list at the closing sample; that
	// list can miss rows a concurrent commit is flushing, so a certificate may
	// undercount. Every row is visible to the reader, which must accept it.
	base, owner := suilendFixture()
	fixture, index := newSQLPortfolioFixture(t, "suilend", base)
	var s suiSQLSnapshot
	_ = json.Unmarshal([]byte(fixture.rows[0].Payload), &s)
	s.ObservedObjectCount = "1"
	fixture.rows[0] = sqlFixtureRow("snapshot", s)
	got, err := index.ReadLatest(context.Background(), owner)
	if err != nil || len(got.Groups) != 1 || fixture.calls != 1 {
		t.Fatalf("rejected an undercounted certificate: %+v %v", got, err)
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

// Suilend is served by its index or not at all: an unconfigured index is an
// error, never a node read that would silently answer for the head instead.
func TestSuiSQLUnconfiguredSuilendNeverReadsNode(t *testing.T) {
	base, owner := suilendFixture()
	node := &latestHeadFixture{latestSuiFixture: base}
	if _, err := NewSuiProtocolReader().ReadLatest(context.Background(), "suilend", owner, node); err == nil {
		t.Fatal("suilend accepted a missing index")
	}
	if node.headReads != 0 {
		t.Fatal("suilend invoked the node")
	}
}

// An address that holds nothing is still answered from a completed sample, so
// an empty result cannot be confused with an index that has not caught up.
func TestSuiSQLCompletedSnapshotCertifiesEmptyWallet(t *testing.T) {
	owner, _ := ParseSuiAddress("0x11")
	base := latestFixture()
	base.metadata = nil
	fixture, index := newSQLPortfolioFixture(t, "suilend", base)
	result, err := index.ReadLatest(context.Background(), owner)
	if err != nil || len(result.Groups) != 0 || result.Checkpoint != fixture.pin || fixture.calls != 1 {
		t.Fatalf("empty indexed wallet: %+v %v", result, err)
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
			reader, err := NewSuilendHistoryReader(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"})
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
func TestSuiSampleIntervalSecondsRoundsToTheMinute(t *testing.T) {
	pin := SuiCheckpoint{Timestamp: time.UnixMilli(1_700_000_000_000)}
	for _, tc := range []struct {
		next string
		want int64
	}{{"1700003600000", 3600}, {"1700003600123", 3600}, {"1700086400480", 86400}, {"1700086399700", 86400}, {"1700000060000", 60}, {"1700000000500", 60}, {"1700000000000", 3600}, {"18446744073709551615", 3600}, {"x", 3600}} {
		data := &suiSQLData{pin: pin, snapshot: suiSQLSnapshot{NextTimestampMs: tc.next}}
		if got := suiSampleIntervalSeconds(data); got != tc.want {
			t.Fatalf("%s: got %d, want %d", tc.next, got, tc.want)
		}
	}
}
