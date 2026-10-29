package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

const (
	indexTestBlock     = uint64(1_000)
	indexTestTimestamp = uint64(1_800_000_000)
)

var indexTestAccount = common.HexToAddress("0x00000000000000000000000000000000000000aa")

// fakeIndex is an index server holding entity rows, answering the processor status read and the
// time-travel queries the indexed read module renders. Rows are served by id order under the
// stream's id_starts_with and id_gt, a page at most first long.
type fakeIndex struct {
	t         *testing.T
	processed uint64
	// rows holds each entity's rows; every row carries "id".
	rows map[string][]map[string]any
	// checkpoint answers the checkpoint rows of the n-th GraphQL request (from 0). Nil answers one
	// row at the query block, one second before indexTestTimestamp.
	checkpoint func(n int, queryBlock uint64) []map[string]any

	mu      sync.Mutex
	queries []string
}

var (
	// Both match a field with or without an alias; the presence probe aliases one per chain.
	fakeCheckpointField = regexp.MustCompile(`(?:(\w+): )?indexerCheckpoints\(first: 2, block: \{ number: (\d+) \}, where: \{ id: "(\d+)" \}\)`)
	fakeStreamField     = regexp.MustCompile(`(?:(\w+): )?(\w+)\(first: (\d+), orderBy: id, orderDirection: asc, block: \{ number: (\d+) \}, where: \{ ([^}]*) \}\)`)
	fakeWhereString     = func(field string) *regexp.Regexp { return regexp.MustCompile(field + `: "([^"]*)"`) }
)

func newFakeIndex(t *testing.T, processed uint64) *fakeIndex {
	return &fakeIndex{t: t, processed: processed, rows: make(map[string][]map[string]any)}
}

func (f *fakeIndex) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	if request.Method == http.MethodGet {
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("1", []ChainID{Ethereum, BSC, Base}, f.processed))
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		f.t.Errorf("decode GraphQL request: %v", err)
		return
	}
	f.mu.Lock()
	n := len(f.queries)
	f.queries = append(f.queries, body.Query)
	f.mu.Unlock()
	data := make(map[string]any)
	for _, match := range fakeCheckpointField.FindAllStringSubmatch(body.Query, -1) {
		key := fieldKey(match[1], "indexerCheckpoints")
		queryBlock, _ := strconv.ParseUint(match[2], 10, 64)
		if f.checkpoint != nil {
			data[key] = f.checkpoint(n, queryBlock)
		} else {
			data[key] = []map[string]any{fakeCheckpointRow(queryBlock, indexTestTimestamp-1)}
		}
	}
	for _, match := range fakeStreamField.FindAllStringSubmatch(body.Query, -1) {
		entity, where := match[2], match[5]
		if entity == "indexerCheckpoints" {
			continue
		}
		first, _ := strconv.Atoi(match[3])
		prefix, after := "", ""
		if found := fakeWhereString("id_starts_with").FindStringSubmatch(where); found != nil {
			prefix = found[1]
		}
		if found := fakeWhereString("id_gt").FindStringSubmatch(where); found != nil {
			after = found[1]
		}
		rows := make([]map[string]any, 0)
		for _, row := range f.sortedRows(entity) {
			id := row["id"].(string)
			if strings.HasPrefix(id, prefix) && id > after && len(rows) < first {
				rows = append(rows, row)
			}
		}
		data[fieldKey(match[1], entity)] = rows
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
}

func fieldKey(alias, field string) string {
	if alias != "" {
		return alias
	}
	return field
}

func (f *fakeIndex) sortedRows(entity string) []map[string]any {
	rows := append([]map[string]any(nil), f.rows[entity]...)
	sort.Slice(rows, func(left, right int) bool { return rows[left]["id"].(string) < rows[right]["id"].(string) })
	return rows
}

func (f *fakeIndex) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func fakeCheckpointRow(block, timestamp uint64) map[string]any {
	return map[string]any{
		"blockNumber": strconv.FormatUint(block, 10),
		"timestampMs": strconv.FormatUint(timestamp*1_000, 10),
	}
}

// serve starts the fake and returns an index on it, declared for Ethereum, BSC and Base.
func (f *fakeIndex) serve(name string) *sentioIndex {
	f.t.Helper()
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)
	return &sentioIndex{
		name: name,
		api: &sentioAPIClient{
			apiKey:     "test",
			httpClient: server.Client(),
			statuses:   make(map[string]sentioStatusCache),
		},
		config: SentioIndexerConfig{
			GraphQLURL:       server.URL + "/graphql",
			StatusURL:        server.URL + "/status",
			ProcessorVersion: "1",
		},
		chains: []ChainID{Ethereum, BSC, Base},
	}
}

// indexTestRow is the row of the toy account-keyed entity the module tests read.
type indexTestRow struct {
	Account string `json:"account"`
	Value   string `json:"value"`
}

func indexTestPrefix(chainID ChainID) string {
	return fmt.Sprintf("%d:%s:", chainID, strings.ToLower(indexTestAccount.Hex()))
}

// indexTestRows fills entity with count rows of the test account on chainID.
func (f *fakeIndex) indexTestRows(entity string, chainID ChainID, count int) {
	for index := 0; index < count; index++ {
		f.rows[entity] = append(f.rows[entity], map[string]any{
			"id":      fmt.Sprintf("%s%04d", indexTestPrefix(chainID), index),
			"account": strings.ToLower(indexTestAccount.Hex()),
			"value":   strconv.Itoa(index),
		})
	}
}

// indexTestStream reads entity's rows of the test account, keeping each row's value.
func indexTestStream(entity string, maxRows int) *indexStream[indexTestRow, string] {
	return &indexStream[indexTestRow, string]{
		Entity:  entity,
		Account: indexTestAccount,
		MaxRows: maxRows,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexTestPrefix(chainID), Terms: []indexTerm{
				{"chainId", int(chainID)},
				{"account", strings.ToLower(indexTestAccount.Hex())},
			}}
		},
		Decode: func(_ ChainID, _ string, row indexTestRow) (string, error) {
			return row.Value, nil
		},
	}
}

func indexTestPin(chainID ChainID) BlockRef {
	return BlockRef{ChainID: chainID, Number: indexTestBlock, Timestamp: indexTestTimestamp}
}

// A read pages a stream to its end, every page at the block the processor status allows: the pin,
// or the processor's last block when it trails the pin.
func TestIndexReadPagesAStreamToItsEnd(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock-10)
	fake.indexTestRows("testRefs", Ethereum, indexPageSize+3)
	index := fake.serve("test")
	stream := indexTestStream("testRefs", 4_096)

	at, err := index.read(context.Background(), nil, indexTestPin(Ethereum), stream)
	if err != nil {
		t.Fatal(err)
	}
	if at.QueryBlock != indexTestBlock-10 {
		t.Fatalf("query block = %d, want the processed block %d", at.QueryBlock, indexTestBlock-10)
	}
	rows := stream.Rows()
	if len(rows) != indexPageSize+3 || rows[0] != "0" || rows[len(rows)-1] != strconv.Itoa(indexPageSize+2) {
		t.Fatalf("read %d rows (%v … %v), want all %d in id order", len(rows), rows[0], rows[len(rows)-1], indexPageSize+3)
	}
	requests := fake.requests()
	if len(requests) != 2 {
		t.Fatalf("sent %d requests, want 2 pages", len(requests))
	}
	for _, query := range requests {
		if !strings.Contains(query, fmt.Sprintf("block: { number: %d }", indexTestBlock-10)) {
			t.Fatalf("page not at the query block:\n%s", query)
		}
	}
}

// Streams of one read share each page's request, each under its own cursor, and a stream that has
// ended drops out of the pages after it.
func TestIndexReadPagesStreamsTogether(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock)
	fake.indexTestRows("longRefs", Ethereum, indexPageSize+1)
	fake.indexTestRows("shortRefs", Ethereum, 2)
	index := fake.serve("test")
	long, short := indexTestStream("longRefs", 4_096), indexTestStream("shortRefs", 4_096)

	if _, err := index.read(context.Background(), nil, indexTestPin(Ethereum), long, short); err != nil {
		t.Fatal(err)
	}
	if len(long.Rows()) != indexPageSize+1 || len(short.Rows()) != 2 {
		t.Fatalf("read %d and %d rows, want %d and 2", len(long.Rows()), len(short.Rows()), indexPageSize+1)
	}
	requests := fake.requests()
	if len(requests) != 2 {
		t.Fatalf("sent %d requests, want one per page", len(requests))
	}
	if !strings.Contains(requests[0], "shortRefs(") || strings.Contains(requests[1], "shortRefs(") {
		t.Fatalf("the ended stream should ride the first page only:\n%s\n%s", requests[0], requests[1])
	}
	lastFirstPage := fmt.Sprintf("%s%04d", indexTestPrefix(Ethereum), indexPageSize-1)
	if !strings.Contains(requests[1], `id_gt: "`+lastFirstPage+`"`) {
		t.Fatalf("second page does not continue after %q:\n%s", lastFirstPage, requests[1])
	}
}

// What the server answers is checked against what was asked: rows in id order, within the
// stream's prefix, no more than a page, and no more than the stream may hold.
func TestIndexReadRejectsRowsItDidNotAskFor(t *testing.T) {
	for _, test := range []struct {
		name  string
		rows  func(prefix string) []map[string]any
		max   int
		error string
	}{
		{
			name: "out of order",
			rows: func(prefix string) []map[string]any {
				return []map[string]any{{"id": prefix + "2"}, {"id": prefix + "1"}}
			},
			max: 10, error: "testRefs rows are not ordered",
		},
		{
			name:  "outside the prefix",
			rows:  func(string) []map[string]any { return []map[string]any{{"id": "1:0xother:1"}} },
			max:   10,
			error: "foreign testRefs row",
		},
		{
			name: "past what the stream may hold",
			rows: func(prefix string) []map[string]any {
				return []map[string]any{{"id": prefix + "1"}, {"id": prefix + "2"}, {"id": prefix + "3"}}
			},
			max: 2, error: "index holds more than 2 testRefs rows",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := test.rows(indexTestPrefix(Ethereum))
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				if request.Method == http.MethodGet {
					_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("1", []ChainID{Ethereum}, indexTestBlock))
					return
				}
				_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
					"indexerCheckpoints": []map[string]any{fakeCheckpointRow(indexTestBlock, indexTestTimestamp)},
					"testRefs":           rows,
				}})
			}))
			t.Cleanup(server.Close)
			index := newFakeIndex(t, indexTestBlock).serve("test")
			index.config.GraphQLURL, index.config.StatusURL = server.URL+"/graphql", server.URL+"/status"
			index.api.httpClient = server.Client()

			_, err := index.read(context.Background(), nil, indexTestPin(Ethereum), indexTestStream("testRefs", test.max))
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("error = %v, want %q", err, test.error)
			}
		})
	}
}

// Every page of a read is certified by the same checkpoint row, the first page's, which can never
// be past the block the read asked at.
func TestIndexReadChecksItsCheckpointOnEveryPage(t *testing.T) {
	for _, test := range []struct {
		name       string
		checkpoint func(n int, queryBlock uint64) []map[string]any
		error      string
	}{
		{
			name: "changed between pages",
			checkpoint: func(n int, queryBlock uint64) []map[string]any {
				return []map[string]any{fakeCheckpointRow(queryBlock-uint64(n), indexTestTimestamp-1)}
			},
			error: "GraphQL pagination changed checkpoint",
		},
		{
			name: "two rows",
			checkpoint: func(_ int, queryBlock uint64) []map[string]any {
				return []map[string]any{
					fakeCheckpointRow(queryBlock, indexTestTimestamp), fakeCheckpointRow(queryBlock, indexTestTimestamp),
				}
			},
			error: "GraphQL returned 2 checkpoints",
		},
		{
			name:       "none, with no start to stand in",
			checkpoint: func(int, uint64) []map[string]any { return []map[string]any{} },
			error:      "GraphQL returned 0 checkpoints",
		},
		{
			name: "past the query block",
			checkpoint: func(_ int, queryBlock uint64) []map[string]any {
				return []map[string]any{fakeCheckpointRow(queryBlock+1, indexTestTimestamp)}
			},
			error: "is ahead of query block",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeIndex(t, indexTestBlock-5)
			fake.indexTestRows("testRefs", Ethereum, indexPageSize+1)
			fake.checkpoint = test.checkpoint
			_, err := fake.serve("test").read(
				context.Background(), nil, indexTestPin(Ethereum), indexTestStream("testRefs", 4_096),
			)
			if err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("error = %v, want %q", err, test.error)
			}
		})
	}
}

// A processor writes its first checkpoint up to a week after its index starts, so a read before it
// finds none. The index's start block then stands in, read from the chain once per read and with no
// lane slot held, and is validated like a written checkpoint: past the backfill lag at a fixed pin,
// a missing checkpoint is stale.
func TestIndexReadStandsTheStartInForAnUnwrittenCheckpoint(t *testing.T) {
	const start, startTime = uint64(900), uint64(1_799_000_000)
	lane := newIndexerLane(1)
	node := &startBlockRPC{t: t, chainID: Ethereum, block: start, timestamp: startTime}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if inUse := lane.stats().InUse; inUse != 0 {
			t.Errorf("the start block was read holding %d lane slots", inUse)
		}
		node.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	for _, test := range []struct {
		name     string
		sinceRun uint64
		accepted bool
	}{
		{name: "within the backfill lag", sinceRun: 3 * 24 * 3600, accepted: true},
		{name: "past the backfill lag", sinceRun: uint64(indexBackfillMaxLag.Seconds()) + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeIndex(t, indexTestBlock)
			fake.indexTestRows("testRefs", Ethereum, indexPageSize+1)
			fake.checkpoint = func(int, uint64) []map[string]any { return []map[string]any{} }
			index := fake.serve("test")
			index.start = func(ChainID) uint64 { return start }
			pin := BlockRef{ChainID: Ethereum, Number: indexTestBlock, Timestamp: startTime + test.sinceRun, Fixed: true}
			stream := indexTestStream("testRefs", 4_096)
			reads := node.reads.Load()

			at, err := index.read(withScan(context.Background(), nil, lane), client, pin, stream)
			if !test.accepted {
				if err == nil || !strings.Contains(err.Error(), "stale") {
					t.Fatalf("error = %v, want the checkpoint staleness error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if at.QueryBlock != indexTestBlock || len(stream.Rows()) != indexPageSize+1 {
				t.Fatalf("read %d rows at %d, want every row at %d", len(stream.Rows()), at.QueryBlock, indexTestBlock)
			}
			if got := node.reads.Load() - reads; got != 1 {
				t.Fatalf("read the start block %d times over two pages, want once", got)
			}
		})
	}
}

// A live read whose checkpoint trails the pin past indexMaxLag still answers, and records one lag
// note for the scan to report as coverage. A read that fails records none.
func TestIndexReadRecordsItsLagForTheScan(t *testing.T) {
	stale := func(_ int, queryBlock uint64) []map[string]any {
		return []map[string]any{fakeCheckpointRow(queryBlock-100, indexTestTimestamp-uint64(2*indexMaxLag.Seconds()))}
	}

	fake := newFakeIndex(t, indexTestBlock)
	fake.indexTestRows("testRefs", Ethereum, indexPageSize+1)
	fake.checkpoint = stale
	ctx, coverage := withCoverage(context.Background())
	stream := indexTestStream("testRefs", 4_096)
	if _, err := fake.serve("test").read(ctx, nil, indexTestPin(Ethereum), stream); err != nil {
		t.Fatal(err)
	}
	notes := coverage.take()
	var lag *indexLagNote
	if len(notes) != 1 || !errors.As(notes[0], &lag) || lag.checkpoint != indexTestBlock-100 {
		t.Fatalf("notes = %v, want one lag note at checkpoint %d", notes, indexTestBlock-100)
	}
	if len(stream.Rows()) != indexPageSize+1 {
		t.Fatalf("read %d rows, want the lagging read's rows all the same", len(stream.Rows()))
	}

	failing := newFakeIndex(t, indexTestBlock)
	failing.indexTestRows("testRefs", Ethereum, 3)
	failing.checkpoint = stale
	ctx, coverage = withCoverage(context.Background())
	if _, err := failing.serve("test").read(ctx, nil, indexTestPin(Ethereum), indexTestStream("testRefs", 2)); err == nil {
		t.Fatal("read past MaxRows succeeded")
	}
	if notes := coverage.take(); len(notes) != 0 {
		t.Fatalf("a failed read recorded %v", notes)
	}
}

// An index that probes asks every chain of the scan at once whether the account holds anything,
// and a read on a chain proven empty sends no page and answers at the block the proof holds at.
// A read with a stream not keyed by the account alone can never be proven empty.
func TestIndexReadSkipsAChainProvenEmpty(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock)
	fake.indexTestRows("testRefs", Base, 2)
	index := fake.serve("test")
	index.probe = true
	pinned := map[ChainID]BlockRef{Ethereum: indexTestPin(Ethereum), BSC: indexTestPin(BSC), Base: indexTestPin(Base)}
	ctx := presenceScanContext(pinned)

	empty := indexTestStream("testRefs", 4_096)
	at, err := index.read(ctx, nil, pinned[Ethereum], empty)
	if err != nil {
		t.Fatal(err)
	}
	requests := fake.requests()
	if len(requests) != 1 || !strings.Contains(requests[0], "query Presence") {
		t.Fatalf("sent %d requests, want the probe alone", len(requests))
	}
	if at.QueryBlock != indexTestBlock || len(empty.Rows()) != 0 {
		t.Fatalf("read %d rows at %d, want none at the proof's block", len(empty.Rows()), at.QueryBlock)
	}
	probe := requests[0]

	held := indexTestStream("testRefs", 4_096)
	if _, err := index.read(ctx, nil, pinned[Base], held); err != nil {
		t.Fatal(err)
	}
	requests = fake.requests()
	if len(requests) != 2 || len(held.Rows()) != 2 {
		t.Fatalf("sent %d requests for %d rows, want one page after the probe", len(requests), len(held.Rows()))
	}
	// The probe asked Base for exactly the rows the page read: the same clause, prefix included.
	var probeWhere, pageWhere string
	for _, match := range fakeStreamField.FindAllStringSubmatch(probe, -1) {
		if match[1] == "f0_8453" {
			probeWhere = match[5]
		}
	}
	for _, match := range fakeStreamField.FindAllStringSubmatch(requests[1], -1) {
		if match[2] == "testRefs" {
			pageWhere = match[5]
		}
	}
	if !strings.Contains(probeWhere, "id_starts_with") ||
		pageWhere != probeWhere+`, id_gt: "`+indexTestPrefix(Base)+`"` {
		t.Fatalf("probe where %q is not the page's %q without its cursor", probeWhere, pageWhere)
	}

	unkeyed := indexTestStream("testRefs", 4_096)
	unkeyed.Account = common.Address{}
	if _, err := index.read(ctx, nil, pinned[Ethereum], unkeyed); err != nil {
		t.Fatal(err)
	}
	if requests = fake.requests(); len(requests) != 3 || strings.Contains(requests[2], "query Presence") {
		t.Fatalf("a stream not keyed by the account was answered by the probe: %d requests", len(requests))
	}
}

// A page holds its lane slot for one attempt's round trip: while a failed attempt backs off, the
// slot is free for any other request of the scan.
func TestIndexReadGivesItsSlotBackWhileARetryWaits(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock)
	fake.indexTestRows("testRefs", Ethereum, 2)
	failed := make(chan struct{})
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			if posts.Add(1) == 1 {
				http.Error(writer, "unavailable", http.StatusServiceUnavailable)
				close(failed)
				return
			}
		}
		fake.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	index := fake.serve("test")
	index.config.GraphQLURL, index.config.StatusURL = server.URL+"/graphql", server.URL+"/status"
	index.api.httpClient = server.Client()
	lane := newIndexerLane(1)
	stream := indexTestStream("testRefs", 4_096)

	done := make(chan error, 1)
	go func() {
		_, err := index.read(withScan(context.Background(), nil, lane), nil, indexTestPin(Ethereum), stream)
		done <- err
	}()
	<-failed
	waitForLaneStats(t, lane, IndexerLaneStats{Capacity: 1})
	if sent := posts.Load(); sent != 1 {
		t.Fatalf("the slot came free only after the retry was sent (%d requests)", sent)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(stream.Rows()) != 2 {
		t.Fatalf("read %d rows after the retry, want 2", len(stream.Rows()))
	}
}

// A lookup that depends on what a read found asks at the read's block, and must find the read's
// checkpoint there: an index that moved under it would mix two snapshots.
func TestIndexLookupStaysOnItsRead(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock-7)
	fake.indexTestRows("testRefs", Ethereum, 1)
	fake.indexTestRows("lookupRefs", Ethereum, 3)
	index := fake.serve("test")
	at, err := index.read(context.Background(), nil, indexTestPin(Ethereum), indexTestStream("testRefs", 4_096))
	if err != nil {
		t.Fatal(err)
	}
	lookup := indexTestStream("lookupRefs", 4_096)
	if err := at.read(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	requests := fake.requests()
	if len(lookup.Rows()) != 3 || !strings.Contains(requests[len(requests)-1], fmt.Sprintf("number: %d", indexTestBlock-7)) {
		t.Fatalf("lookup read %d rows:\n%s", len(lookup.Rows()), requests[len(requests)-1])
	}

	fake.checkpoint = func(_ int, queryBlock uint64) []map[string]any {
		return []map[string]any{fakeCheckpointRow(queryBlock-1, indexTestTimestamp-1)}
	}
	err = at.read(context.Background(), indexTestStream("lookupRefs", 4_096))
	if err == nil || !strings.Contains(err.Error(), "changed checkpoint") {
		t.Fatalf("error = %v, want the moved checkpoint rejected", err)
	}
	if err := (indexAt{}).read(context.Background(), indexTestStream("lookupRefs", 4_096)); err == nil {
		t.Fatal("a lookup outside any read succeeded")
	}
}

// A retried request answers as its last attempt did: an attempt that failed to decode leaves
// nothing behind for the one that succeeds after it.
func TestIndexReadRetryAnswersAsItsLastAttempt(t *testing.T) {
	fake := newFakeIndex(t, indexTestBlock)
	fake.indexTestRows("testRefs", Ethereum, 2)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && posts.Add(1) == 1 {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"errors":[{"message":5}]}`))
			return
		}
		fake.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	index := fake.serve("test")
	index.config.GraphQLURL, index.config.StatusURL = server.URL+"/graphql", server.URL+"/status"
	index.api.httpClient = server.Client()
	stream := indexTestStream("testRefs", 4_096)

	if _, err := index.read(context.Background(), nil, indexTestPin(Ethereum), stream); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 2 || len(stream.Rows()) != 2 {
		t.Fatalf("read %d rows over %d requests, want both rows from the retry", len(stream.Rows()), posts.Load())
	}
}

// A read is declared with distinct entities, each with a bound on its rows; the module refuses one
// that is not rather than sending a query whose answer it could not attribute.
func TestIndexReadRefusesAnIllDeclaredStream(t *testing.T) {
	index := newFakeIndex(t, indexTestBlock).serve("test")
	unbounded := indexTestStream("testRefs", 0)
	if _, err := index.read(context.Background(), nil, indexTestPin(Ethereum), unbounded); err == nil ||
		!strings.Contains(err.Error(), "no row bound") {
		t.Fatalf("error = %v, want a stream without a row bound refused", err)
	}
	twice := []indexStreamer{indexTestStream("testRefs", 10), indexTestStream("testRefs", 10)}
	if _, err := index.read(context.Background(), nil, indexTestPin(Ethereum), twice...); err == nil ||
		!strings.Contains(err.Error(), "testRefs twice") {
		t.Fatalf("error = %v, want an entity read twice refused", err)
	}
}
