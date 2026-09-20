package portfolio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Opt in with PORTFOLIO_CLICKHOUSE_BINARY. Execute the generated statement so
// lifecycle ordering, dependency selection and replay deduplication are tested
// by the SQL engine, independently of the HTTP response fixtures.
func TestSuiSQLExecution(t *testing.T) {
	binary := os.Getenv("PORTFOLIO_CLICKHOUSE_BINARY")
	if binary == "" {
		t.Skip("set PORTFOLIO_CLICKHOUSE_BINARY to execute SQL")
	}
	owner, _ := ParseSuiAddress("0xa")
	other, _ := ParseSuiAddress("0xb")
	const protocol = "suilend"
	kinds, _ := suiSQLProtocolKinds(protocol)
	objects := []map[string]string{}
	add := func(id, kind string, cp, version int, state, who, related, parent, key, tx, links string) map[string]string {
		row := map[string]string{"id": fmt.Sprintf("%s:%020d:%s", kind, cp, suiTestVersionID(naviAddress(id), uint64(version), state != "live")),
			"objectId": naviAddress(id), "kind": kind, "checkpoint": strconv.Itoa(cp), "version": strconv.Itoa(version),
			"state": state, "ownerKind": "ADDRESS", "owner": who, "relatedId": related, "parentId": parent,
			"key": key, "transactionDigest": tx, "links": links, "materializedAtCheckpoint": strconv.Itoa(cp + 1),
			"timestampMs": strconv.Itoa(cp * 1000), "content": "{}", "digest": suiTestDigest, "objectType": "0x1::test::Object"}
		objects = append(objects, row)
		return row
	}
	rootKind := kinds.roots[0]
	links := "{}"
	add("0x11", rootKind, 10, 1, "live", owner.Hex(), naviAddress("0x22"), "", "", "", links)
	add("0x11", rootKind, 20, 2, "live", other.Hex(), naviAddress("0x22"), "", "", "", links)
	add("0x11", rootKind, 30, 2, "live", owner.Hex(), naviAddress("0x22"), "", "", "", links)
	add("0x11", rootKind, 30, 2, "deleted", "", "", "", "", "", "{}")
	add("0x11", rootKind, 40, 3, "live", owner.Hex(), naviAddress("0x22"), "", "", "", links)
	delayed := add("0x11", rootKind, 25, 3, "live", other.Hex(), "", "", "", "", "{}")
	delayed["materializedAtCheckpoint"] = "99"
	// The capability names the obligation, the obligation its market.
	add("0x22", kinds.direct[0], 5, 1, "live", "", naviAddress("0x44"), "", "", "", "{}")
	add("0x44", kinds.second[0], 5, 1, "live", "", "", "", "", "", "{}")
	// Deterministic retry writes are separate raw rows with the same ID.
	objects = append(objects, objects[0], objects[len(objects)-1])
	snapshots := []map[string]string{}
	for _, cp := range []int{10, 20, 30, 40} {
		ids := map[string]bool{}
		for _, row := range objects {
			source, _ := strconv.Atoi(row["checkpoint"])
			visible, _ := strconv.Atoi(row["materializedAtCheckpoint"])
			if source > cp-10 && source <= cp && visible <= cp+10 {
				ids[row["id"]] = true
			}
		}
		snapshots = append(snapshots, map[string]string{"id": fmt.Sprintf("%020d", cp), "checkpoint": strconv.Itoa(cp), "timestampMs": strconv.Itoa(cp * 1000), "digest": suiTestDigest, "schemaVersion": "2", "startCheckpoint": "1", "materializedAtCheckpoint": strconv.Itoa(cp + 10), "nextCheckpoint": strconv.Itoa(cp + 10), "nextTimestampMs": strconv.Itoa((cp + 10) * 1000), "previousCheckpoint": strconv.Itoa(cp - 10), "objectCount": strconv.Itoa(len(ids)), "valueCount": "0"})
	}
	// The processor publishes the narrow read indexes next to every state
	// row; the owner index only lists account-owned roots.
	objectIndex, ownerIndex := []map[string]string{}, []map[string]string{}
	for _, row := range objects {
		terminal := "0"
		if row["state"] != "live" {
			terminal = "1"
		}
		cp, _ := strconv.Atoi(row["checkpoint"])
		version, _ := strconv.Atoi(row["version"])
		tail := fmt.Sprintf("%020d:%020d:%s", cp, version, terminal)
		common := map[string]string{"objectId": row["objectId"], "stateId": row["id"], "checkpoint": row["checkpoint"], "materializedAtCheckpoint": row["materializedAtCheckpoint"]}
		object := map[string]string{"id": row["kind"] + ":" + row["objectId"] + ":" + tail, "kind": row["kind"]}
		for k, v := range common {
			object[k] = v
		}
		objectIndex = append(objectIndex, object)
		indexOwner := ""
		if row["kind"] == rootKind && row["ownerKind"] == "ADDRESS" {
			indexOwner = row["owner"]
		}
		if indexOwner != "" {
			index := map[string]string{"id": row["kind"] + ":" + indexOwner + ":" + row["objectId"] + ":" + tail, "owner": indexOwner}
			for k, v := range common {
				index[k] = v
			}
			ownerIndex = append(ownerIndex, index)
		}
	}
	setup := sqlTestTable("PortfolioObjectState_raw", "id objectId kind digest state ownerKind owner objectType content transactionDigest parentId key relatedId links", "version checkpoint timestampMs materializedAtCheckpoint", objects)
	setup += sqlTestTable("PortfolioObjectIndex_raw", "id objectId kind stateId", "checkpoint materializedAtCheckpoint", objectIndex)
	setup += sqlTestTable("PortfolioOwnerIndex_raw", "id owner objectId stateId", "checkpoint materializedAtCheckpoint", ownerIndex)
	setup += sqlTestTable("PortfolioSnapshot", "id digest", "checkpoint timestampMs schemaVersion startCheckpoint materializedAtCheckpoint nextCheckpoint nextTimestampMs previousCheckpoint objectCount valueCount", snapshots)
	setup += sqlTestTable("PortfolioValue", "id kind account market content", "checkpoint materializedAtCheckpoint", nil)
	setup += sqlTestTable("PortfolioTokenMetadata", "id status coinType symbol name", "decimals checkpoint timestampMs materializedAtCheckpoint", nil)
	for _, tc := range []struct {
		cp   uint64
		who  SuiAddress
		held bool
	}{{10, owner, true}, {20, owner, false}, {20, other, true}, {30, owner, false}, {40, owner, true}} {
		t.Run(fmt.Sprintf("%d-%s", tc.cp, tc.who.Hex()), func(t *testing.T) {
			index := &suiHistoryIndex{protocolID: protocol, start: 1}
			at := time.UnixMilli(int64(tc.cp*1000 + 500))
			query, err := index.portfolioSQL(tc.who, suiSQLSelection{at: &at})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "local", "--multiquery", "--query", setup+query+" FORMAT JSONEachRow")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			if err != nil {
				t.Fatalf("SQL execution: %v %s", err, stderr.String())
			}
			rows := []suiSQLRow{}
			for _, line := range bytes.Split(bytes.TrimSpace(output), []byte("\n")) {
				var row suiSQLRow
				if err := json.Unmarshal(line, &row); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, row)
			}
			seen := map[string]suiSQLObject{}
			held := false
			certificates := 0
			for _, row := range rows {
				switch row.RowType {
				case "snapshot":
					certificates++
					var s suiSQLSnapshot
					_ = json.Unmarshal([]byte(row.Payload), &s)
					if s.Checkpoint != fmt.Sprint(tc.cp) || s.ObjectCount != s.ObservedObjectCount || s.ValueCount != "0" {
						t.Fatalf("invalid certificate: %+v", s)
					}
				case "object":
					var o suiSQLObject
					_ = json.Unmarshal([]byte(row.Payload), &o)
					if _, exists := seen[o.ObjectID]; exists {
						t.Fatal("duplicate replay row")
					}
					seen[o.ObjectID] = o
					if o.ObjectID == naviAddress("0x11") && o.State == "live" && o.Owner == tc.who.Hex() {
						held = true
					}
				}
			}
			if certificates != 1 || held != tc.held {
				t.Fatalf("certificates=%d held=%t expected=%t", certificates, held, tc.held)
			}
			if tc.held && seen[naviAddress("0x22")].Kind != kinds.direct[0] {
				t.Fatal("missing obligation")
			}
			if tc.held && seen[naviAddress("0x44")].Kind != kinds.second[0] {
				t.Fatal("missing lending market")
			}
		})
	}
}

func sqlTestTable(name, stringsList, integersList string, rows []map[string]string) string {
	fields := map[string]string{}
	for _, field := range strings.Fields(stringsList) {
		fields[field] = "String"
	}
	for _, field := range strings.Fields(integersList) {
		fields[field] = "Int64"
	}
	keys := make([]string, 0, len(fields))
	for field := range fields {
		keys = append(keys, field)
	}
	sort.Strings(keys)
	columns := make([]string, 0, len(keys))
	for _, field := range keys {
		columns = append(columns, field+" "+fields[field])
	}
	sql := `CREATE TABLE "` + name + `" (` + strings.Join(columns, ",") + `) ENGINE=Memory;`
	for _, row := range rows {
		values := make([]string, 0, len(keys))
		for _, field := range keys {
			value := row[field]
			if fields[field] == "String" {
				value = sqlString(value)
			} else if value == "" {
				value = "0"
			}
			values = append(values, value)
		}
		sql += `INSERT INTO "` + name + `" VALUES (` + strings.Join(values, ",") + `);`
	}
	return sql
}
