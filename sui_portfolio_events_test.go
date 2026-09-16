package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSuiDailyEventReader(t *testing.T) {
	input, owner, _ := dailyFixture(t, "suilend")
	c, err := NewSuiPortfolioCalculator(input)
	if err != nil {
		t.Fatal(err)
	}
	event, err := c.Calculate(owner.Hex())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := suiDailySnapshot{ID: fmt.Sprintf("%020s", input.Checkpoint), Checkpoint: input.Checkpoint, TimestampMs: input.TimestampMs, Digest: input.Digest, SchemaVersion: "3", StartTimestampMs: "1", AccountCount: "1", ErrorCount: "0", ObservedCount: "1", VariantCount: "1", ObservedErrors: "0"}
	rows := []suiSQLRow{sqlFixtureRow("snapshot", snapshot), sqlFixtureRow("event", event)}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"rows": rows}})
	}))
	defer server.Close()
	r, err := NewSuilendHistoryReader(SentioIndexerConfig{SQLURL: server.URL, ProcessorVersion: "1", SuiPortfolioSchemaVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"latest", "time", "checkpoint"} {
		var got SuiProtocolPositions
		switch mode {
		case "latest":
			got, err = r.ReadLatest(context.Background(), owner)
		case "time":
			got, err = r.ReadAtTime(context.Background(), owner, c.data.pin.Timestamp.Add(23*time.Hour))
		default:
			got, err = r.ReadAtCheckpoint(context.Background(), owner, c.data.pin.Sequence)
		}
		if err != nil || len(got.Groups) != len(event.Positions) || got.Groups[0].Metadata["sampleIntervalSeconds"] != 86400 {
			t.Fatal(got, err)
		}
	}
	if calls != 3 {
		t.Fatal(calls)
	}
	if _, err = r.ReadAtTime(context.Background(), owner, c.data.pin.Timestamp.Add(24*time.Hour)); err == nil {
		t.Fatal("stale day accepted")
	}
	if _, err = r.ReadAtCheckpoint(context.Background(), owner, c.data.pin.Sequence+1); err == nil {
		t.Fatal("uncertified checkpoint accepted")
	}
	for _, scenario := range []string{"missing certificate", "incomplete events", "conflict", "wrong owner", "future event", "bad quantity", "wrong asset", "calculation error"} {
		t.Run(scenario, func(t *testing.T) {
			s := snapshot
			raw, _ := json.Marshal(event)
			var e SuiPortfolioEvent
			_ = json.Unmarshal(raw, &e)
			switch scenario {
			case "incomplete events":
				s.ObservedCount = "0"
			case "conflict":
				s.VariantCount = "2"
			case "wrong owner":
				e.Account = naviAddress("0x123")
			case "future event":
				e.TimestampMs = "9999999999999"
			case "bad quantity":
				e.Positions[0].Components[0].AmountDenominatorRaw = "0"
			case "wrong asset":
				e.Positions[0].Components[0].Asset.SentioChainID = "ethereum"
			case "calculation error":
				e.Errors = []string{"missing state"}
			}
			rows = []suiSQLRow{sqlFixtureRow("snapshot", s), sqlFixtureRow("event", e)}
			if scenario == "missing certificate" {
				rows = rows[1:]
			}
			if _, err := r.ReadLatest(context.Background(), owner); err == nil {
				t.Fatal("invalid daily result accepted")
			}
		})
	}
	rows = []suiSQLRow{sqlFixtureRow("snapshot", snapshot)}
	got, err := r.ReadLatest(context.Background(), owner)
	if err != nil || len(got.Groups) != 0 {
		t.Fatal(got, err)
	}
}

func TestSuiDailyEventSQLExecution(t *testing.T) {
	binary := os.Getenv("PORTFOLIO_CLICKHOUSE_BINARY")
	if binary == "" {
		t.Skip("set PORTFOLIO_CLICKHOUSE_BINARY to execute SQL")
	}
	owner, _ := ParseSuiAddress("0xa")
	inputEvent := map[string]any{"schemaVersion": 3, "protocolId": "navi", "account": owner.Hex(), "checkpoint": "100", "timestampMs": "1000", "digest": suiTestDigest, "positions": []any{}, "errors": []any{}}
	raw, _ := json.Marshal(inputEvent)
	snapshot := map[string]string{"id": fmt.Sprintf("%020d", 100), "checkpoint": "100", "timestampMs": "1000", "digest": suiTestDigest, "schemaVersion": "3", "startTimestampMs": "1", "accountCount": "1", "errorCount": "0"}
	for _, scenario := range []string{"replay", "missing", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			events := []map[string]string{{"account": owner.Hex(), "timestampMs": "1000", "portfolio": string(raw)}, {"account": owner.Hex(), "timestampMs": "1000", "portfolio": string(raw)}}
			if scenario == "missing" {
				events = nil
			}
			if scenario == "conflict" {
				events[1]["portfolio"] = strings.Replace(string(raw), `"checkpoint":"100"`, `"checkpoint":"101"`, 1)
			}
			setup := sqlTestTable("DailyPortfolioSnapshot", "id digest", "checkpoint timestampMs schemaVersion startTimestampMs accountCount errorCount", []map[string]string{snapshot})
			setup += sqlTestTable("Portfolio", "account portfolio", "timestampMs", events)
			r := &suiHistoryIndex{protocolID: "navi"}
			query, err := r.dailyPortfolioSQL(owner, suiSQLSelection{})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "local", "--multiquery", "--query", setup+query+" FORMAT JSONEachRow")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v %s", err, output)
			}
			var cert suiDailySnapshot
			for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
				var row suiSQLRow
				if json.Unmarshal([]byte(line), &row) != nil {
					t.Fatal(string(output))
				}
				if row.RowType == "snapshot" {
					_ = json.Unmarshal([]byte(row.Payload), &cert)
				}
			}
			if scenario == "replay" && (cert.ObservedCount != "1" || cert.VariantCount != "1") {
				t.Fatal(cert)
			}
			if scenario == "missing" && cert.ObservedCount != "0" {
				t.Fatal(cert)
			}
			if scenario == "conflict" && cert.VariantCount != "2" {
				t.Fatal(cert)
			}
		})
	}
}
