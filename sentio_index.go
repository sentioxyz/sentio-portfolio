package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// indexPageSize is the most rows one page asks for of one entity stream.
const indexPageSize = 500

// sentioIndex is one deployed index — a protocol's processor project at its version — as the
// kernel reads it. It owns the order every index read follows: the processor status
// (indexQueryBlock), the presence proof, the pages, and the checkpoint that certifies them
// (checkIndexCheckpoint). A caller declares what to read as entity streams and never sends a
// request itself.
type sentioIndex struct {
	// name labels the index; one per deployment. It keys the index's presence probe.
	name   string
	api    *sentioAPIClient
	config SentioIndexerConfig
	// chains are the chains the processor indexes: the status read is bounded to them and the
	// presence probe asks about them.
	chains []ChainID
	// probe lets a read whose streams are all the account's alone be answered by the presence probe
	// (sentio_presence.go), which asks every chain of the scan in one request.
	probe bool
	// start is the block the processor's checkpoint binding starts at on a chain, which is also the
	// adapter's own activation for the index. It stands in for the checkpoint a processor has not
	// written yet. Nil, or zero for a chain, requires a written checkpoint.
	//
	// A processor writes its checkpoint on a time interval, a week long while it backfills, and the
	// driver fires the interval on weekly boundaries rather than at the start block. The first row
	// therefore lands up to seven days after the index starts, and a query at a block before it
	// finds no checkpoint, although the chain status proved the processor has passed that block.
	// The start block then serves as the checkpoint, with that block's time, and is validated like
	// a written one (checkIndexCheckpoint): at a fixed pin it is accepted only within the backfill
	// lag of the start, and later a missing checkpoint fails as a stale one.
	start func(ChainID) uint64
}

// indexWhere selects a stream's rows on one chain, paging aside.
type indexWhere struct {
	// Prefix starts the id of every row the stream selects, and bounds the rows the server reads
	// (id_starts_with). Empty only for an index whose ids do not carry what the rows are keyed by.
	Prefix string
	// Terms are the where clause's other fields, in order.
	Terms []indexTerm
}

// indexTerm is one field of a where clause. Value is an int, a uint64, a string or a []string,
// rendered as a literal: every value is one the kernel produced.
type indexTerm struct {
	Field string
	Value any
}

// indexStream is one entity paged by id under its own cursor. Rows of type R are decoded into T
// once the read has admitted them: within the stream's prefix, after the cursor, no more than
// MaxRows in all. A stream fills once; build one per read.
type indexStream[R, T any] struct {
	// Entity is the GraphQL field; streams of one read name distinct entities.
	Entity string
	// Account is the account every row the stream selects belongs to, and zero when the rows are
	// not one account's alone.
	Account common.Address
	Where   func(ChainID) indexWhere
	MaxRows int
	// Decode checks what only the protocol knows of a row — its chain, its account, the id its
	// fields rebuild — and converts it.
	Decode func(chainID ChainID, id string, row R) (T, error)

	rows []T
}

// Rows returns what the read decoded, in id order.
func (s *indexStream[R, T]) Rows() []T { return s.rows }

func (s *indexStream[R, T]) spec() *indexStreamSpec {
	return &indexStreamSpec{entity: s.Entity, account: s.Account, where: s.Where, maxRows: s.MaxRows}
}

func (s *indexStream[R, T]) fields() ([]string, error) {
	return jsonFields(reflect.TypeFor[R]())
}

func (s *indexStream[R, T]) take(chainID ChainID, id string, raw json.RawMessage) error {
	var row R
	if err := json.Unmarshal(raw, &row); err != nil {
		return fmt.Errorf("GraphQL returned malformed %s row %q", s.Entity, id)
	}
	value, err := s.Decode(chainID, id, row)
	if err != nil {
		return err
	}
	s.rows = append(s.rows, value)
	return nil
}

// indexStreamer is an indexStream of any row type, as a read pages it.
type indexStreamer interface {
	spec() *indexStreamSpec
	fields() ([]string, error)
	take(chainID ChainID, id string, raw json.RawMessage) error
}

type indexStreamSpec struct {
	entity  string
	account common.Address
	where   func(ChainID) indexWhere
	maxRows int
}

// jsonFields lists the GraphQL fields a row type selects: its json names, embedded structs' too.
func jsonFields(rowType reflect.Type) ([]string, error) {
	fields := make([]string, 0, rowType.NumField())
	for index := range rowType.NumField() {
		field := rowType.Field(index)
		if field.Anonymous && field.Type.Kind() == reflect.Struct {
			embedded, err := jsonFields(field.Type)
			if err != nil {
				return nil, err
			}
			fields = append(fields, embedded...)
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			return nil, fmt.Errorf("row field %s.%s has no json name", rowType.Name(), field.Name)
		}
		if name != "id" {
			fields = append(fields, name)
		}
	}
	return fields, nil
}

// indexAt is where a read answered: the block every request asked at and the checkpoint that
// certified it.
type indexAt struct {
	index *sentioIndex
	block BlockRef
	// QueryBlock is the block every request of the read asked at.
	QueryBlock uint64
	checkpoint indexCheckpoint
}

// indexCheckpoint is the checkpoint rows one time-travel query returned.
type indexCheckpoint struct {
	// written is false when the processor had written none by the query block.
	written bool
	block   uint64
	ms      uint64
}

// read reads streams on block's chain, every one to its end, and returns where it answered. client
// reads the start block when it stands in for the checkpoint, and may be nil for an index with no
// start.
func (x *sentioIndex) read(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	streams ...indexStreamer,
) (indexAt, error) {
	statuses, queryBlock, err := x.queryBlock(ctx, block)
	if err != nil {
		return indexAt{}, err
	}
	at := indexAt{index: x, block: block, QueryBlock: queryBlock}
	if probe, account, provable := x.presenceProbe(streams); provable {
		if proof, empty := x.api.chainProvenEmpty(ctx, probe, block, account, statuses); empty {
			// The proof may predate the status read; it is the block the answer holds at.
			at.QueryBlock = proof.QueryBlock
			at.checkpoint = indexCheckpoint{written: true, block: proof.Checkpoint, ms: proof.CheckpointMS}
			return at, nil
		}
	}
	var lag *indexLagNote
	if at.checkpoint, lag, err = x.pages(ctx, client, at, streams, nil); err != nil {
		return indexAt{}, err
	}
	// The index chose the rows as of its checkpoint, so a position opened after it is missing.
	if lag != nil {
		noteCoverage(ctx, lag)
	}
	return at, nil
}

// read reads streams at the block of the read that returned at, held to that read's checkpoint: a
// lookup that depends on what the read found. It neither probes nor records lag; the read did both.
func (at indexAt) read(ctx context.Context, streams ...indexStreamer) error {
	if at.index == nil {
		return fmt.Errorf("index lookup outside an index read")
	}
	_, _, err := at.index.pages(ctx, nil, at, streams, &at.checkpoint)
	return err
}

// prefetchPresence starts the presence probe a read of streams would ask, ahead of the read, so its
// one request is off the scan's critical path (presencePrefetcher). It does nothing for streams the
// probe cannot answer for.
func (x *sentioIndex) prefetchPresence(ctx context.Context, streams ...indexStreamer) {
	if probe, account, provable := x.presenceProbe(streams); provable {
		x.api.prefetchPresence(ctx, probe, account)
	}
}

// presenceProbe is the probe for a read of streams, and the account it keys on. Only an index that
// probes, reading streams every one of which selects one account's rows under a prefix, can be
// proven empty: a probe on any other where clause would prove nothing the pages would read. The
// probe renders each stream's where clause as the pages do, without the cursor.
func (x *sentioIndex) presenceProbe(streams []indexStreamer) (presenceProbe, string, bool) {
	if !x.probe || len(streams) == 0 {
		return presenceProbe{}, "", false
	}
	account := streams[0].spec().account
	fields := make([]presenceField, 0, len(streams))
	// The probe's name keys its proof in the scan's memo, so it names every clause the probe asks:
	// a read of other rows must not take this proof for its own.
	var name strings.Builder
	name.WriteString(x.name)
	for _, stream := range streams {
		spec := stream.spec()
		if spec.account == (common.Address{}) || spec.account != account {
			return presenceProbe{}, "", false
		}
		fmt.Fprintf(&name, "/%s", spec.entity)
		for _, chainID := range x.chains {
			where := spec.where(chainID)
			if where.Prefix == "" {
				return presenceProbe{}, "", false
			}
			fmt.Fprintf(&name, "{%s}", renderIndexWhere(where, ""))
		}
		fields = append(fields, presenceField{entity: spec.entity, where: func(chainID ChainID) string {
			return renderIndexWhere(spec.where(chainID), "")
		}})
	}
	return presenceProbe{
		name:           name.String(),
		config:         x.config,
		requiredChains: x.chains,
		fields:         fields,
	}, strings.ToLower(account.Hex()), true
}

// queryBlock reads the processor status of block's chain, taking no lane slot, and returns it with
// the block a read for block asks at (indexQueryBlock).
func (x *sentioIndex) queryBlock(ctx context.Context, block BlockRef) (map[ChainID]sentioChainStatus, uint64, error) {
	return indexQueryBlock(block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
		return x.api.chainStatusesForScan(ctx, x.config, x.chains, block.ChainID, forceRefresh)
	})
}

// indexCursor is one stream's progress through a read.
type indexCursor struct {
	stream indexStreamer
	spec   *indexStreamSpec
	where  indexWhere
	fields []string
	after  string
	rows   int
	done   bool
}

// first is how many rows the cursor's next page asks for: a page, or one past what the stream
// may hold, so a stream with too many rows is caught without reading them all.
func (c *indexCursor) first() int {
	return min(indexPageSize, c.spec.maxRows+1-c.rows)
}

// pages reads streams to their ends at at.QueryBlock and returns the checkpoint the pages carried.
// Every page carries the same checkpoint as want, or, when want is nil, as the first page, which
// is then validated against the pin and may come with a lag note.
func (x *sentioIndex) pages(
	ctx context.Context,
	client *RPCClient,
	at indexAt,
	streams []indexStreamer,
	want *indexCheckpoint,
) (indexCheckpoint, *indexLagNote, error) {
	if len(streams) == 0 {
		return indexCheckpoint{}, nil, fmt.Errorf("index read of no stream")
	}
	chainID := at.block.ChainID
	cursors := make([]*indexCursor, 0, len(streams))
	entities := make(map[string]struct{}, len(streams))
	for _, stream := range streams {
		spec := stream.spec()
		// A page keys each stream's rows by its entity, and a stream with no bound would fail on
		// its first row.
		if _, twice := entities[spec.entity]; twice {
			return indexCheckpoint{}, nil, fmt.Errorf("index read names %s twice", spec.entity)
		}
		entities[spec.entity] = struct{}{}
		if spec.maxRows <= 0 {
			return indexCheckpoint{}, nil, fmt.Errorf("index stream %s declares no row bound", spec.entity)
		}
		fields, err := stream.fields()
		if err != nil {
			return indexCheckpoint{}, nil, err
		}
		where := spec.where(chainID)
		cursors = append(cursors, &indexCursor{stream: stream, spec: spec, where: where, fields: fields, after: where.Prefix})
	}
	var lag *indexLagNote
	for {
		open := make([]*indexCursor, 0, len(cursors))
		for _, cursor := range cursors {
			if !cursor.done {
				open = append(open, cursor)
			}
		}
		if len(open) == 0 {
			return *want, lag, nil
		}
		var data map[string]json.RawMessage
		if err := x.api.graphql(ctx, x.config.GraphQLURL, indexPageQuery(chainID, at.QueryBlock, open), &data); err != nil {
			return indexCheckpoint{}, nil, err
		}
		checkpoint, err := pageCheckpoint(data)
		if err != nil {
			return indexCheckpoint{}, nil, err
		}
		if want == nil {
			if lag, err = x.certify(ctx, client, at, checkpoint); err != nil {
				return indexCheckpoint{}, nil, err
			}
			want = &checkpoint
		} else if checkpoint != *want {
			return indexCheckpoint{}, nil, fmt.Errorf("GraphQL pagination changed checkpoint")
		}
		for _, cursor := range open {
			if err := cursor.page(chainID, data); err != nil {
				return indexCheckpoint{}, nil, err
			}
		}
	}
}

// certify validates the checkpoint a read's first page carried, or the start standing in for it.
// It runs after the page's request has given its lane slot back, so the start block is read
// holding none.
func (x *sentioIndex) certify(
	ctx context.Context,
	client *RPCClient,
	at indexAt,
	checkpoint indexCheckpoint,
) (*indexLagNote, error) {
	block, ms := checkpoint.block, checkpoint.ms
	if !checkpoint.written {
		var start uint64
		if x.start != nil {
			start = x.start(at.block.ChainID)
		}
		if start == 0 || client == nil {
			return nil, fmt.Errorf("GraphQL returned 0 checkpoints")
		}
		header, err := client.BlockByNumber(ctx, start)
		if err != nil {
			return nil, fmt.Errorf("GraphQL returned 0 checkpoints; read index start %d: %w", start, err)
		}
		block, ms = start, header.Timestamp*1_000
	}
	if block > at.QueryBlock {
		return nil, fmt.Errorf("indexer checkpoint %d is ahead of query block %d", block, at.QueryBlock)
	}
	return checkIndexCheckpoint(at.block, block, ms)
}

// page admits one page of the cursor's rows from data and advances the cursor past them.
func (c *indexCursor) page(chainID ChainID, data map[string]json.RawMessage) error {
	raw, exists := data[c.spec.entity]
	if !exists {
		return fmt.Errorf("GraphQL omitted %s", c.spec.entity)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return fmt.Errorf("GraphQL returned malformed %s rows", c.spec.entity)
	}
	first := c.first()
	if len(rows) > first {
		return fmt.Errorf("GraphQL returned %d %s rows for a page of %d", len(rows), c.spec.entity, first)
	}
	for _, raw := range rows {
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.ID == "" {
			return fmt.Errorf("GraphQL returned a %s row without an id", c.spec.entity)
		}
		if !strings.HasPrefix(row.ID, c.where.Prefix) {
			return fmt.Errorf("GraphQL returned foreign %s row %q", c.spec.entity, row.ID)
		}
		if row.ID <= c.after {
			return fmt.Errorf("%s rows are not ordered", c.spec.entity)
		}
		if err := c.stream.take(chainID, row.ID, raw); err != nil {
			return err
		}
		c.after = row.ID
		c.rows++
	}
	if c.rows > c.spec.maxRows {
		return fmt.Errorf("index holds more than %d %s rows", c.spec.maxRows, c.spec.entity)
	}
	c.done = len(rows) < first
	return nil
}

// pageCheckpoint reads the checkpoint rows a page carried.
func pageCheckpoint(data map[string]json.RawMessage) (indexCheckpoint, error) {
	raw, exists := data["indexerCheckpoints"]
	if !exists {
		return indexCheckpoint{}, fmt.Errorf("GraphQL omitted indexerCheckpoints")
	}
	return parseIndexCheckpoint(raw)
}

// indexCheckpointField renders the field that reads a chain's checkpoint rows at block, under alias
// when one is given: a page asks for one chain's, the presence probe for every chain's.
func indexCheckpointField(alias string, chainID ChainID, block uint64) string {
	if alias != "" {
		alias += ": "
	}
	return fmt.Sprintf(
		"%sindexerCheckpoints(first: 2, block: { number: %d }, where: { id: \"%d\" }) { blockNumber timestampMs }",
		alias, block, chainID,
	)
}

// parseIndexCheckpoint reads the checkpoint rows one time-travel query returned: none, or one.
func parseIndexCheckpoint(raw json.RawMessage) (indexCheckpoint, error) {
	var rows []struct {
		BlockNumber string `json:"blockNumber"`
		TimestampMS string `json:"timestampMs"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return indexCheckpoint{}, fmt.Errorf("GraphQL returned malformed checkpoints")
	}
	switch len(rows) {
	case 0:
		return indexCheckpoint{}, nil
	case 1:
	default:
		return indexCheckpoint{}, fmt.Errorf("GraphQL returned %d checkpoints", len(rows))
	}
	block, err := strconv.ParseUint(rows[0].BlockNumber, 10, 64)
	if err != nil {
		return indexCheckpoint{}, fmt.Errorf("invalid checkpoint block %q", rows[0].BlockNumber)
	}
	ms, err := strconv.ParseUint(rows[0].TimestampMS, 10, 64)
	if err != nil {
		return indexCheckpoint{}, fmt.Errorf("invalid checkpoint timestamp %q", rows[0].TimestampMS)
	}
	return indexCheckpoint{written: true, block: block, ms: ms}, nil
}

// indexPageQuery renders one page: the chain's checkpoint and each open stream after its cursor,
// all at block.
func indexPageQuery(chainID ChainID, block uint64, cursors []*indexCursor) string {
	var query strings.Builder
	query.WriteString("query IndexPage {\n")
	query.WriteString("  " + indexCheckpointField("", chainID, block) + "\n")
	for _, cursor := range cursors {
		fmt.Fprintf(
			&query,
			"  %s(first: %d, orderBy: id, orderDirection: asc, block: { number: %d }, where: { %s }) { %s }\n",
			cursor.spec.entity, cursor.first(), block, renderIndexWhere(cursor.where, cursor.after),
			strings.Join(append([]string{"id"}, cursor.fields...), " "),
		)
	}
	query.WriteString("}")
	return query.String()
}

// renderIndexWhere renders a where clause's body: its terms, its prefix, and the cursor when after
// is set. The presence probe renders the same clause without a cursor, so the probe asks for
// exactly the rows the pages would read.
func renderIndexWhere(where indexWhere, after string) string {
	parts := make([]string, 0, len(where.Terms)+2)
	for _, term := range where.Terms {
		parts = append(parts, term.Field+": "+indexLiteral(term.Value))
	}
	if where.Prefix != "" {
		parts = append(parts, "id_starts_with: "+indexLiteral(where.Prefix))
	}
	if after != "" {
		parts = append(parts, "id_gt: "+indexLiteral(after))
	}
	return strings.Join(parts, ", ")
}

// indexLiteral renders a value as a GraphQL literal. JSON's string escapes are GraphQL's.
func indexLiteral(value any) string {
	switch value := value.(type) {
	case string:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	case []string:
		items := make([]string, 0, len(value))
		for _, item := range value {
			items = append(items, indexLiteral(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	case int, int64, uint64:
		return fmt.Sprint(value)
	default:
		panic(fmt.Sprintf("index where value of type %T", value))
	}
}

// indexAccountPrefix starts the id of every row an index keys by account on a chain.
func indexAccountPrefix(chainID ChainID, account common.Address) string {
	return fmt.Sprintf("%d:%s:", chainID, strings.ToLower(account.Hex()))
}
