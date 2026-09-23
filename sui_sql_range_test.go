package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// suiRangeServer is a SQL endpoint that answers range statements from a list of
// certified samples of an empty wallet. It honours each statement's window and
// LIMIT, so the reader's windowing is what the test observes.
type suiRangeServer struct {
	t       *testing.T
	start   uint64
	samples []suiSQLSnapshot
	limits  []int
	// respond may replace a statement's response; it returns false to keep it.
	respond func(statement int, limit int, w http.ResponseWriter) bool
	// pad inflates every sample row, to steer sizing by bytes.
	pad int
}

var (
	suiRangeUpper = regexp.MustCompile(`timestampMs <= (\d+) AND nextTimestampMs > (\d+) ORDER BY checkpoint LIMIT (\d+)`)
)

func newSuiRangeServer(t *testing.T, hours int) (*suiRangeServer, *suiHistoryIndex, time.Time) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	s := &suiRangeServer{t: t, start: suilendHistoryStart}
	base := time.Date(2026, 9, 20, 0, 0, 0, 200_000_000, time.UTC)
	for i := 0; i < hours; i++ {
		cp := s.start + 100 + uint64(i)*1000
		previous := s.start - 1
		if i > 0 {
			previous = cp - 1000
		}
		ts := base.Add(time.Duration(i) * time.Hour)
		s.samples = append(s.samples, suiSQLSnapshot{ID: fmt.Sprintf("%020d", cp), Checkpoint: fmt.Sprint(cp), TimestampMs: fmt.Sprint(ts.UnixMilli()), Digest: suiTestDigest,
			SchemaVersion: "2", StartCheckpoint: fmt.Sprint(s.start), MaterializedAtCheckpoint: fmt.Sprint(cp + 1000), NextCheckpoint: fmt.Sprint(cp + 1000),
			NextTimestampMs: fmt.Sprint(ts.Add(time.Hour).UnixMilli()), PreviousCheckpoint: fmt.Sprint(previous), ObjectCount: "0", ValueCount: "0"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			SQLQuery struct {
				SQL string `json:"sql"`
			} `json:"sqlQuery"`
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Error("invalid SQL request")
		}
		match := suiRangeUpper.FindStringSubmatch(body.SQLQuery.SQL)
		if match == nil {
			t.Errorf("not a range statement")
			return
		}
		upper, _ := strconv.ParseInt(match[1], 10, 64)
		lower, _ := strconv.ParseInt(match[2], 10, 64)
		limit, _ := strconv.Atoi(match[3])
		s.limits = append(s.limits, limit)
		if s.respond != nil && s.respond(len(s.limits)-1, limit, w) {
			return
		}
		rows := []suiSQLRow{}
		for _, sample := range s.samples {
			ts, _ := strconv.ParseInt(sample.TimestampMs, 10, 64)
			next, _ := strconv.ParseInt(sample.NextTimestampMs, 10, 64)
			if ts <= upper && next > lower && len(rows) < limit {
				payload := map[string]any{}
				raw, _ := json.Marshal(sample)
				_ = json.Unmarshal(raw, &payload)
				if s.pad > 0 {
					payload["padding"] = strings.Repeat("x", s.pad)
				}
				rows = append(rows, sqlFixtureRow("sample", payload))
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"rows": rows}})
	}))
	t.Cleanup(server.Close)
	index, err := newSuiHistoryIndex(SentioIndexerConfig{SQLURL: server.URL + "/sql/execute", ProcessorVersion: "1"}, "suilend", "Suilend", s.start)
	if err != nil {
		t.Fatal(err)
	}
	return s, index, base
}

func suiRangeOwner() SuiAddress {
	owner, _ := ParseSuiAddress("0x11")
	return owner
}

// A range reads its samples in checkpoint order, contiguous and certified, with
// a small probe first and then as many samples per statement as sizing allows.
func TestSuiHistoryRangeReadsWindowsInOrder(t *testing.T) {
	server, index, base := newSuiRangeServer(t, 20)
	samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(19*time.Hour))
	if err != nil || len(samples) != 20 {
		t.Fatalf("%d samples, %v", len(samples), err)
	}
	for i, sample := range samples {
		if sample.Err != nil || !sample.Start.Equal(base.Add(time.Duration(i)*time.Hour)) || !sample.Until.Equal(sample.Start.Add(time.Hour)) ||
			sample.Positions.Checkpoint.Timestamp != sample.Start || sample.Positions.ProtocolID != "suilend" {
			t.Fatalf("sample %d: %+v", i, sample)
		}
	}
	if fmt.Sprint(server.limits) != "[2 8 8 8]" {
		t.Fatalf("statement limits %v", server.limits)
	}
}

// Sizing follows the bytes a sample weighs: 400 kB samples leave room for three
// per statement under the target.
func TestSuiHistoryRangeSizesStatementsByBytes(t *testing.T) {
	server, index, base := newSuiRangeServer(t, 10)
	server.pad = 400_000
	samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(9*time.Hour))
	if err != nil || len(samples) != 10 {
		t.Fatalf("%d samples, %v", len(samples), err)
	}
	if fmt.Sprint(server.limits) != "[2 3 3 3]" {
		t.Fatalf("statement limits %v", server.limits)
	}
}

// A response that does not carry the whole statement, whether paged or past the
// body limit, is asked again for fewer samples, and sizing never grows back to a
// window that did not fit.
func TestSuiHistoryRangeShrinksIncompleteResponses(t *testing.T) {
	for _, scenario := range []string{"truncated", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			server, index, base := newSuiRangeServer(t, 6)
			server.respond = func(_ int, limit int, w http.ResponseWriter) bool {
				if limit <= 1 {
					return false
				}
				if scenario == "truncated" {
					_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"rows": []suiSQLRow{}, "truncated": true}})
				} else {
					_, _ = w.Write([]byte(`{"result":{"rows":[],"padding":"` + strings.Repeat("x", sentioResponseLimit) + `"}}`))
				}
				return true
			}
			samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(5*time.Hour))
			if err != nil || len(samples) != 6 {
				t.Fatalf("%d samples, %v", len(samples), err)
			}
			if fmt.Sprint(server.limits) != "[2 1 1 1 1 1 1]" {
				t.Fatalf("statement limits %v", server.limits)
			}
		})
	}
}

// A statement the client times out or the endpoint kills is asked again for
// fewer samples; a failure that says nothing about cost ends the range.
func TestSuiHistoryRangeShrinksStatementsThatDoNotFinish(t *testing.T) {
	for _, scenario := range []string{"timeout", "killed", "unauthorized"} {
		t.Run(scenario, func(t *testing.T) {
			server, index, base := newSuiRangeServer(t, 4)
			release := make(chan struct{})
			defer close(release)
			if scenario == "timeout" {
				index.api.httpClient.Timeout = 200 * time.Millisecond
			}
			server.respond = func(_ int, limit int, w http.ResponseWriter) bool {
				if limit <= 1 {
					return false
				}
				switch scenario {
				case "timeout":
					select {
					case <-release:
					case <-time.After(2 * time.Second):
					}
				case "killed":
					w.WriteHeader(499)
				default:
					w.WriteHeader(http.StatusUnauthorized)
				}
				return true
			}
			samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(3*time.Hour))
			if scenario == "unauthorized" {
				if err == nil || len(server.limits) != 1 {
					t.Fatalf("%d statements, %v", len(server.limits), err)
				}
				return
			}
			if err != nil || len(samples) != 4 || fmt.Sprint(server.limits) != "[2 1 1 1 1]" {
				t.Fatalf("%d samples, limits %v, %v", len(samples), server.limits, err)
			}
		})
	}
}

// A failed statement leaves the samples of earlier statements standing.
func TestSuiHistoryRangeReturnsEarlierSamplesWithAFailure(t *testing.T) {
	server, index, base := newSuiRangeServer(t, 12)
	server.respond = func(statement int, _ int, w http.ResponseWriter) bool {
		if statement == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(11*time.Hour))
	if err == nil || len(samples) != 2 || len(server.limits) != 2 {
		t.Fatalf("%d samples after %d statements, %v", len(samples), len(server.limits), err)
	}
}

// One sample whose certificate fails carries its own error; its neighbours are
// read as usual.
func TestSuiHistoryRangeIsolatesASampleFailure(t *testing.T) {
	server, index, base := newSuiRangeServer(t, 4)
	server.samples[2].SchemaVersion = "1"
	samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base, base.Add(3*time.Hour))
	if err != nil || len(samples) != 4 {
		t.Fatalf("%d samples, %v", len(samples), err)
	}
	for i, sample := range samples {
		if (sample.Err != nil) != (i == 2) {
			t.Fatalf("sample %d error %v", i, sample.Err)
		}
	}
	if _, err := samples[2].At(samples[2].Start); err == nil {
		t.Fatal("At answered from a failed sample")
	}
}

// Nothing certified in the window is no samples and no error: every instant in
// it is simply unanswered, as a single read would find.
func TestSuiHistoryRangeWithoutSamples(t *testing.T) {
	server, index, base := newSuiRangeServer(t, 2)
	samples, err := index.ReadRange(context.Background(), suiRangeOwner(), base.Add(5*time.Hour), base.Add(9*time.Hour))
	if err != nil || len(samples) != 0 || len(server.limits) != 1 {
		t.Fatalf("%d samples after %d statements, %v", len(samples), len(server.limits), err)
	}
	if _, err := index.ReadRange(context.Background(), suiRangeOwner(), base.Add(time.Hour), base); err == nil {
		t.Fatal("accepted a reversed range")
	}
}

// At answers only inside the sample, stamps the instant asked about, and leaves
// the sample itself unchanged for the next instant.
func TestSuiHistorySampleAt(t *testing.T) {
	start := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	sample := SuiHistorySample{Start: start, Until: start.Add(time.Hour), Positions: SuiProtocolPositions{ProtocolID: "navi",
		Groups: []SuiProtocolGroup{{ID: "g", Metadata: map[string]any{"stateMode": "historical"}}, {ID: "h"}}}}
	at := start.Add(10 * time.Minute)
	got, err := sample.At(at)
	if err != nil || got.Groups[0].Metadata["requestedTimestamp"] != at.Format(time.RFC3339Nano) || got.Groups[1].Metadata["requestedTimestamp"] == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if _, stamped := sample.Positions.Groups[0].Metadata["requestedTimestamp"]; stamped || sample.Positions.Groups[1].Metadata != nil {
		t.Fatal("At changed the sample")
	}
	for _, outside := range []time.Time{start.Add(-time.Millisecond), start.Add(time.Hour)} {
		if _, err := sample.At(outside); err == nil {
			t.Fatalf("answered %s outside the sample", outside)
		}
	}
	failed := sample
	failed.Err = errors.New("certificate")
	if _, err := failed.At(at); err == nil {
		t.Fatal("answered from a failed sample")
	}
}
