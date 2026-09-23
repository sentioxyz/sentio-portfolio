package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A range statement's cost is its response: the endpoint produces a body at
// roughly a second per hundred kilobytes, and each sample carries a whole
// protocol state, NAVI's reserve topology or every version of the Suilend markets
// an account's obligations name. So each statement is sized from the bytes per
// sample the previous one returned, toward suiSQLRangeBytes, starting from a
// small probe and never above suiSQLRangeSamples. A statement that still does not
// fit, in its response or in the time the client or the endpoint allows it, is
// asked again for half as many samples.
const (
	suiSQLRangeSamples      = 8
	suiSQLRangeProbeSamples = 2
	suiSQLRangeBytes        = 1_500_000
)

// SuiHistorySample is one completed sample of a range read. It owns the interval
// [Start, Until): a single read at any instant inside it is answered from this
// sample, and At reports it the way that read would.
type SuiHistorySample struct {
	Start, Until time.Time
	Positions    SuiProtocolPositions
	// Err is this sample's own certificate or calculation failure. It does not
	// invalidate the other samples of the range.
	Err error
}

// At returns the positions a ReadAtTime(at) answered from this sample reports.
func (s SuiHistorySample) At(at time.Time) (SuiProtocolPositions, error) {
	if at.Before(s.Start) || !at.Before(s.Until) {
		return SuiProtocolPositions{ProtocolID: s.Positions.ProtocolID, ProtocolName: s.Positions.ProtocolName}, fmt.Errorf("Sui index has no completed sample for the requested hour")
	}
	if s.Err != nil {
		return s.Positions, s.Err
	}
	out := s.Positions
	out.Groups = slices.Clone(s.Positions.Groups)
	for i := range out.Groups {
		metadata := maps.Clone(out.Groups[i].Metadata)
		if metadata == nil {
			metadata = map[string]any{}
		}
		out.Groups[i].Metadata = metadata
		metadata["requestedTimestamp"] = at.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// ReadRange returns, in checkpoint order, the completed samples whose intervals
// meet [from, to]. Each statement answers up to suiSQLRangeSamples of them with
// the rows a single read of each would select, and every sample is certified
// exactly as a single read certifies it. An instant no returned sample covers has
// no completed sample, as a single read at it would report. When a statement
// fails, the samples of the statements before it are returned with the error:
// they stand on their own, and only the rest of the range is unanswered.
func (r *suiHistoryIndex) ReadRange(ctx context.Context, owner SuiAddress, from, to time.Time) ([]SuiHistorySample, error) {
	if to.Before(from) || from.UnixMilli() < 0 {
		return nil, fmt.Errorf("invalid Sui history range")
	}
	// One memo per range: consecutive samples share market versions a Suilend
	// obligation names, and each version is decoded once.
	markets := newSuilendMarketMemo()
	var out []SuiHistorySample
	// ceiling is below the smallest window a response has not fitted, so sizing
	// from bytes never asks for one that failed again.
	limit, ceiling := suiSQLRangeProbeSamples, suiSQLRangeSamples
	for !from.After(to) {
		samples, size, err := r.readRangeSQL(ctx, owner, from, to, limit, markets)
		if err != nil && limit > 1 && ctx.Err() == nil && suiSQLRangeTooCostly(err) {
			ceiling = limit / 2
			limit = ceiling
			continue
		}
		if err != nil {
			return out, err
		}
		if len(samples) == 0 {
			break
		}
		out = append(out, samples...)
		// Every sample ends after from, so each statement moves the window forward.
		from = samples[len(samples)-1].Until
		limit = min(suiSQLRangeLimit(size, len(samples)), ceiling)
	}
	return out, nil
}

// suiSQLRangeTooCostly reports a statement that did not fit: a response past what
// one carries, or one the client or the endpoint gave up on. Each is asked again
// for fewer samples, which is a smaller statement rather than the same one twice;
// any other failure ends the range.
func suiSQLRangeTooCostly(err error) bool {
	if errors.Is(err, errSuiSQLPaged) {
		return true
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return true
	}
	var status sentioHTTPError
	if errors.As(err, &status) {
		switch status.status {
		case 499, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// suiSQLRangeLimit sizes the next statement from the last one's bytes per sample.
func suiSQLRangeLimit(size, samples int) int {
	perSample := max(size/max(samples, 1), 1)
	return min(max(suiSQLRangeBytes/perSample, 1), suiSQLRangeSamples)
}

// readRangeSQL executes one range statement and also reports the size of its rows.
func (r *suiHistoryIndex) readRangeSQL(ctx context.Context, owner SuiAddress, from, to time.Time, limit int, markets *suilendMarketMemo) ([]SuiHistorySample, int, error) {
	query, err := r.rangeSQL(owner, from, to, limit)
	if err != nil {
		return nil, 0, err
	}
	rows, err := r.executeSQL(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	size := 0
	for _, row := range rows {
		size += len(row.Payload) + len(row.Content)
	}
	decoded, err := decodeSuiSQLRange(rows)
	if err != nil {
		return nil, 0, err
	}
	samples, err := r.rangeSamples(ctx, owner, decoded, from, to, markets)
	return samples, size, err
}

// rangeSQL returns every sample whose interval meets [from, to], up to limit, with
// the raw material to reconstruct each one: per-sample certificate counts from one
// scan of the samples' joint interval; the candidates of every selection stage with
// the first checkpoint each qualifies at; and for every candidate object, the
// newest version at or before each sample. Each stage selects the union over the
// samples of what portfolioSQL selects at one; rangeSamples then repeats
// portfolioSQL's selection at each sample over these rows.
func (r *suiHistoryIndex) rangeSQL(owner SuiAddress, from, to time.Time, limit int) (string, error) {
	kinds, err := suiSQLProtocolKinds(r.protocolID)
	if err != nil {
		return "", err
	}
	if from.UnixMilli() < 0 || to.Before(from) || limit < 1 {
		return "", fmt.Errorf("invalid Sui history range")
	}
	lowerMs, upperMs := strconv.FormatInt(from.UnixMilli(), 10), strconv.FormatInt(to.UnixMilli(), 10)
	ctes := []string{
		`samples AS (SELECT * FROM "PortfolioSnapshot" WHERE schemaVersion=` + suiPortfolioSchemaVersion + ` AND timestampMs <= ` + upperMs + ` AND nextTimestampMs > ` + lowerMs + ` ORDER BY checkpoint LIMIT ` + strconv.Itoa(limit) + `)`,
		// One scalar, sorted by checkpoint: every bucket below indexes these
		// arrays, so they must agree position by position.
		`(SELECT arraySort(groupArray(tuple(checkpoint, previousCheckpoint, materializedAtCheckpoint))) FROM samples) AS sample_bounds`,
		`arrayMap(b -> tupleElement(b, 1), sample_bounds) AS sample_checkpoints`,
		`arrayMap(b -> tupleElement(b, 2), sample_bounds) AS sample_previous`,
		`arrayMap(b -> tupleElement(b, 3), sample_bounds) AS sample_materialized`,
		`arrayMax(sample_checkpoints) AS last_checkpoint`,
		`arrayMax(sample_materialized) AS last_materialized`,
	}
	// Range bounds are built whole inside a scalar, as suiSQLBound explains, but
	// from the sample arrays: a CTE is expanded again at every reference, and
	// these bounds are referenced once per kind.
	bound := func(kind, value string) string {
		return `(SELECT concat(` + sqlString(kind+":") + `,leftPad(toString(` + value + `),20,'0'),':~'))`
	}
	// A version's bucket is the first sample at or after it. The newest version
	// in each bucket, per object, is what every sample from that bucket on reads
	// until a later bucket holds a newer one.
	bucket := func(column string) string { return `arrayFirstIndex(c -> c >= ` + column + `, sample_checkpoints)` }
	versions := []string{}
	stage := func(name, ids string, objectKinds []string) {
		selection := `SELECT objectId, kind, ` + bucket("checkpoint") + ` AS bucket, max(stateId) AS selectedId, argMax(checkpoint, stateId) AS selectedCheckpoint FROM "PortfolioObjectIndex_raw" WHERE ` +
			suiSQLKindPrefixRange(objectKinds) + ` AND checkpoint <= last_checkpoint`
		if ids != "" {
			selection += ` AND objectId IN ` + ids
		}
		ctes = append(ctes, `(SELECT groupArray(tuple(objectId, kind, selectedId, selectedCheckpoint)) FROM (`+selection+` GROUP BY objectId, kind, bucket)) AS `+name+`_versions`)
		// The stage's rows over every sample feed the next stage's candidates.
		ctes = append(ctes, name+` AS (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(arrayMap(v -> tupleElement(v, 3), `+name+`_versions))) AND materializedAtCheckpoint <= last_materialized)`)
		versions = append(versions, name+"_versions")
	}
	candidates := func(alias string) string {
		return `(SELECT arrayJoin(arrayMap(c -> tupleElement(c, 1), ` + alias + `)))`
	}
	ranges := make([]string, 0, len(kinds.roots))
	for _, kind := range kinds.roots {
		ranges = append(ranges, `(id > `+sqlString(kind+":"+owner.Hex()+":")+` AND id <= `+sqlString(kind+":"+owner.Hex()+":~")+`)`)
	}
	ctes = append(ctes, `(SELECT groupArray(tuple(objectId, first)) FROM (SELECT objectId, min(checkpoint) AS first FROM "PortfolioOwnerIndex_raw" WHERE (`+strings.Join(ranges, ` OR `)+`) AND checkpoint <= last_checkpoint GROUP BY objectId)) AS root_candidates`)
	stage("root_states", candidates("root_candidates"), kinds.roots)
	ctes = append(ctes, `roots AS (SELECT * FROM root_states WHERE state='live' AND owner=`+sqlString(owner.Hex())+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS'))`)
	stage("direct_states", `(SELECT relatedId FROM roots WHERE relatedId != '')`, kinds.direct)
	if len(kinds.second) > 0 {
		stage("second_states", `(SELECT relatedId FROM direct_states WHERE state='live' AND relatedId != '')`, kinds.second)
	}
	output := []string{
		`SELECT 'candidate' AS rowType,toJSONString(map('stage','root','objectId',tupleElement(c,1),'first',toString(tupleElement(c,2)))) AS payload FROM (SELECT arrayJoin(root_candidates) AS c)`,
	}
	valueBranch := ""
	if r.protocolID == "navi" {
		stage("topology", "", kinds.topology)
		ctes = append(ctes, `accounts AS (SELECT `+sqlString(owner.Hex())+` AS account UNION DISTINCT SELECT JSONExtractString(links,'accountAddress') FROM roots WHERE kind='account')`)
		ctes = append(ctes, `(SELECT groupArray(tuple(objectId, owner, first)) FROM (SELECT objectId, owner, min(checkpoint) AS first FROM "PortfolioOwnerIndex_raw" WHERE `+suiSQLKindPrefixRange([]string{"principal"})+` AND owner IN (SELECT account FROM accounts) AND checkpoint <= last_checkpoint GROUP BY objectId, owner)) AS principal_candidates`)
		// Only the table IDs of live reserves are needed here. Reading them through
		// the topology stage would fetch every sample's reserve content once per
		// reference, which is most of what a whole range's rows weigh.
		ctes = append(ctes, `(SELECT arrayDistinct(arrayConcat(groupArray(JSONExtractString(links,'supplyTableId')),groupArray(JSONExtractString(links,'borrowTableId')))) FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(arrayMap(v -> tupleElement(v, 3), arrayFilter(v -> tupleElement(v, 2) = 'reserve', topology_versions)))) AND materializedAtCheckpoint <= last_materialized AND state='live') AS reserve_tables`)
		reserveTables := `SELECT arrayJoin(reserve_tables)`
		// portfolioSQL admits a child through any of its rows at or before the
		// sample, so the first checkpoint of each distinct key and parent is what
		// decides it at every sample.
		ctes = append(ctes, `(SELECT groupArray(tuple(objectId, kind, key, parentId, first)) FROM (SELECT objectId, kind, key, parentId, min(checkpoint) AS first FROM "PortfolioObjectState_raw" WHERE checkpoint <= last_checkpoint AND (`+
			`(kind='principal' AND objectId IN `+candidates("principal_candidates")+` AND key IN (SELECT account FROM accounts) AND parentId IN (`+reserveTables+`))`+
			` OR (kind='receiptState' AND key IN (SELECT objectId FROM roots WHERE kind='receipt') AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live' AND kind='vault')))`+
			` GROUP BY objectId, kind, key, parentId)) AS child_candidates`)
		stage("child_states", candidates("child_candidates"), kinds.children)
		output = append(output,
			`SELECT 'candidate' AS rowType,toJSONString(map('stage','principal','objectId',tupleElement(c,1),'owner',tupleElement(c,2),'first',toString(tupleElement(c,3)))) AS payload FROM (SELECT arrayJoin(principal_candidates) AS c)`,
			`SELECT 'candidate' AS rowType,toJSONString(map('stage','child','objectId',tupleElement(c,1),'kind',tupleElement(c,2),'key',tupleElement(c,3),'parentId',tupleElement(c,4),'first',toString(tupleElement(c,5)))) AS payload FROM (SELECT arrayJoin(child_candidates) AS c)`,
		)
		valueBranch = `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") +
			` AS payload FROM (SELECT * FROM "PortfolioValue" WHERE (id >= 'emode:' AND id <= ` + bound("emode", "last_checkpoint") + `) AND materializedAtCheckpoint <= last_materialized` +
			` AND account IN (SELECT account FROM accounts))`
	}
	all := `arrayConcat(` + strings.Join(versions, ",") + `)`
	output = append(output,
		`SELECT 'version' AS rowType,toJSONString(map('objectId',tupleElement(v,1),'kind',tupleElement(v,2),'stateId',tupleElement(v,3),'checkpoint',toString(tupleElement(v,4)))) AS payload FROM (SELECT arrayJoin(`+all+`) AS v)`,
		// Every visibility variant of a selected version: which one a sample reads
		// depends on that sample's materialization bound.
		`SELECT 'object' AS rowType,`+sqlPayload(suiSQLObjectFieldsWithoutContent()...)+` AS payload,toString(content) AS objectContent FROM (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(arrayMap(v -> tupleElement(v, 3), `+all+`))) AND materializedAtCheckpoint <= last_materialized ORDER BY id, materializedAtCheckpoint LIMIT 1 BY id, materializedAtCheckpoint)`,
		`SELECT 'metadata' AS rowType,`+sqlPayload("coinType", "decimals", "symbol", "name", "checkpoint", "materializedAtCheckpoint", "status")+` AS payload FROM (SELECT * FROM "PortfolioTokenMetadata" WHERE checkpoint <= last_materialized AND materializedAtCheckpoint <= last_materialized)`,
	)
	if valueBranch != "" {
		output = append(output, valueBranch)
	}
	// Certificate counts: one scan of the samples' joint interval, each row
	// counted for the sample whose own (previousCheckpoint, checkpoint] holds its
	// source checkpoint and whose materialization bound it is visible by.
	count := func(table, source string, countKinds []string) string {
		ranges := make([]string, 0, len(countKinds))
		for _, kind := range countKinds {
			ranges = append(ranges, `(id > `+bound(kind, "arrayMin(sample_previous)")+` AND id <= `+bound(kind, "last_checkpoint")+`)`)
		}
		return `SELECT 'count' AS rowType,toJSONString(map('table',` + sqlString(table) + `,'checkpoint',toString(sample_checkpoints[bucket]),'count',toString(uniqExact(id)))) AS payload FROM (` +
			`SELECT id, materializedAtCheckpoint, toInt64OrZero(substring(id, position(id, ':') + 1, 20)) AS source, ` + bucket("source") + ` AS bucket FROM "` + source + `" WHERE (` + strings.Join(ranges, ` OR `) + `)` +
			`) WHERE bucket > 0 AND source > sample_previous[bucket] AND materializedAtCheckpoint <= sample_materialized[bucket] GROUP BY bucket`
	}
	output = append(output, count("object", "PortfolioObjectState_raw", kinds.all()))
	if len(kinds.values) > 0 {
		output = append(output, count("value", "PortfolioValue_raw", kinds.values))
	}
	snapshotFields := []string{"id", "checkpoint", "timestampMs", "digest", "schemaVersion", "startCheckpoint", "materializedAtCheckpoint", "nextCheckpoint", "nextTimestampMs", "previousCheckpoint", "objectCount", "valueCount"}
	output = append([]string{`SELECT 'sample' AS rowType,` + sqlPayload(snapshotFields...) + ` AS payload FROM samples`}, output...)
	// Every branch has the same three columns; only object rows fill the third.
	// It must not be named after a table column: an alias shadows the column
	// everywhere in its SELECT, the payload of that branch included.
	for i, branch := range output {
		if !strings.Contains(branch, ` AS objectContent FROM `) {
			output[i] = strings.Replace(branch, ` AS payload FROM `, ` AS payload,'' AS objectContent FROM `, 1)
		}
	}
	return "WITH " + strings.Join(ctes, ",\n") + "\n" + strings.Join(output, "\nUNION ALL\n"), nil
}

func suiSQLObjectFieldsWithoutContent() []string {
	return []string{"id", "objectId", "kind", "version", "digest", "state", "ownerKind", "owner", "objectType", "checkpoint", "timestampMs", "transactionDigest", "materializedAtCheckpoint", "parentId", "key", "relatedId", "links"}
}

type suiSQLCandidate struct {
	Stage, ObjectID, Owner, Kind, Key, ParentID, First string
}
type suiSQLVersion struct{ ObjectID, Kind, StateID, Checkpoint string }
type suiSQLCount struct{ Table, Checkpoint, Count string }

// suiSQLRange is one range statement's rows, decoded.
type suiSQLRange struct {
	samples    []suiSQLSnapshot
	counts     []suiSQLCount
	candidates []suiSQLCandidate
	versions   []suiSQLVersion
	objects    []suiSQLObject
	metadata   []suiSQLMetadata
	values     []suiSQLValue
}

func decodeSuiSQLRange(rows []suiSQLRow) (suiSQLRange, error) {
	var out suiSQLRange
	for _, row := range rows {
		var err error
		switch row.RowType {
		case "sample":
			var v suiSQLSnapshot
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.samples = append(out.samples, v)
		case "count":
			var v suiSQLCount
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.counts = append(out.counts, v)
		case "candidate":
			var v suiSQLCandidate
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.candidates = append(out.candidates, v)
		case "version":
			var v suiSQLVersion
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.versions = append(out.versions, v)
		case "object":
			var v suiSQLObject
			err = json.Unmarshal([]byte(row.Payload), &v)
			v.Content = row.Content
			out.objects = append(out.objects, v)
		case "metadata":
			var v suiSQLMetadata
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.metadata = append(out.metadata, v)
		case "value":
			var v suiSQLValue
			err = json.Unmarshal([]byte(row.Payload), &v)
			out.values = append(out.values, v)
		default:
			return out, fmt.Errorf("unknown Sui SQL row type")
		}
		if err != nil {
			return out, fmt.Errorf("invalid Sui SQL %s row", row.RowType)
		}
	}
	return out, nil
}

// rangeSamples certifies and calculates every sample of one range statement. A
// sample whose own rows fail is returned with its error; a statement whose
// samples cannot be placed in time fails whole, since no instant could be
// attributed to the right sample.
func (r *suiHistoryIndex) rangeSamples(ctx context.Context, owner SuiAddress, rows suiSQLRange, from, to time.Time, markets *suilendMarketMemo) ([]SuiHistorySample, error) {
	view, err := newSuiSQLRangeView(rows)
	if err != nil {
		return nil, err
	}
	type placed struct {
		snapshot    suiSQLSnapshot
		checkpoint  uint64
		start, next time.Time
	}
	samples := make([]placed, 0, len(rows.samples))
	seen := map[uint64]int{}
	for _, s := range rows.samples {
		cp, e1 := historyUint(s.Checkpoint)
		start, e2 := historyUint(s.TimestampMs)
		next, e3 := historyUint(s.NextTimestampMs)
		if e1 != nil || e2 != nil || e3 != nil || next > 253402300799999 || start > 253402300799999 ||
			int64(start) > to.UnixMilli() || int64(next) <= from.UnixMilli() {
			return nil, fmt.Errorf("invalid Sui SQL completion certificate")
		}
		seen[cp]++
		samples = append(samples, placed{s, cp, time.UnixMilli(int64(start)).UTC(), time.UnixMilli(int64(next)).UTC()})
	}
	sort.SliceStable(samples, func(i, j int) bool { return samples[i].checkpoint < samples[j].checkpoint })
	out := make([]SuiHistorySample, 0, len(samples))
	for _, s := range samples {
		sample := SuiHistorySample{Start: s.start, Until: s.next, Positions: r.result(SuiCheckpoint{})}
		if seen[s.checkpoint] > 1 {
			// Two certificates for one checkpoint cannot both describe it.
			sample.Err = fmt.Errorf("Sui index has no completed sample for the requested hour")
			out = append(out, sample)
			continue
		}
		at := s.start
		selection := suiSQLSelection{at: &at}
		sampleRows, err := view.sample(r.protocolID, owner, s.snapshot)
		if err == nil {
			var data *suiSQLData
			if data, err = r.sqlData(owner, sampleRows, selection); err == nil {
				data.markets = markets
				sample.Positions, err = r.samplePortfolio(ctx, owner, data, selection)
			}
		}
		if err != nil {
			sample.Positions = r.result(SuiCheckpoint{})
			sample.Err = err
		}
		// The selection time is the sample's own start; At stamps the instant a
		// caller asks about instead.
		for i := range sample.Positions.Groups {
			delete(sample.Positions.Groups[i].Metadata, "requestedTimestamp")
		}
		out = append(out, sample)
	}
	return out, nil
}

// suiSQLRangeView indexes a range statement's rows for per-sample selection.
type suiSQLRangeView struct {
	counts     map[string]map[string]string // table -> sample checkpoint -> count
	roots      map[string]uint64            // object -> first checkpoint in the owner index
	principals []suiSQLCandidate
	children   []suiSQLCandidate
	versions   map[string][]suiSQLVersionAt // object -> per-bucket newest versions
	states     map[string][]suiSQLObject    // state ID -> visibility variants
	metadata   []suiSQLMetadata
	values     []suiSQLValue
}
type suiSQLVersionAt struct {
	kind, stateID string
	checkpoint    uint64
}

func newSuiSQLRangeView(rows suiSQLRange) (*suiSQLRangeView, error) {
	v := &suiSQLRangeView{counts: map[string]map[string]string{}, roots: map[string]uint64{}, versions: map[string][]suiSQLVersionAt{}, states: map[string][]suiSQLObject{}, metadata: rows.metadata, values: rows.values}
	invalid := fmt.Errorf("invalid Sui SQL range rows")
	for _, c := range rows.counts {
		if v.counts[c.Table] == nil {
			v.counts[c.Table] = map[string]string{}
		}
		if _, dup := v.counts[c.Table][c.Checkpoint]; dup {
			return nil, invalid
		}
		v.counts[c.Table][c.Checkpoint] = c.Count
	}
	for _, c := range rows.candidates {
		first, err := historyUint(c.First)
		if err != nil {
			return nil, invalid
		}
		switch c.Stage {
		case "root":
			if _, dup := v.roots[c.ObjectID]; dup {
				return nil, invalid
			}
			v.roots[c.ObjectID] = first
		case "principal":
			v.principals = append(v.principals, c)
		case "child":
			v.children = append(v.children, c)
		default:
			return nil, invalid
		}
	}
	for _, version := range rows.versions {
		cp, err := historyUint(version.Checkpoint)
		if err != nil || version.StateID == "" {
			return nil, invalid
		}
		v.versions[version.ObjectID] = append(v.versions[version.ObjectID], suiSQLVersionAt{version.Kind, version.StateID, cp})
	}
	for _, obj := range rows.objects {
		v.states[obj.ID] = append(v.states[obj.ID], obj)
	}
	return v, nil
}

// latest is portfolioSQL's per-stage selection at one sample: per candidate the
// newest version of the stage's kinds at or before the sample, then that
// version's newest row visible by the sample's materialization bound. A version
// with no visible row leaves the object out, as it does there. nil candidates
// select every object with a version of the kinds.
func (v *suiSQLRangeView) latest(candidates map[string]bool, kinds []string, checkpoint, materialized uint64) map[string]suiSQLObject {
	out := map[string]suiSQLObject{}
	for objectID, versions := range v.versions {
		if candidates != nil && !candidates[objectID] {
			continue
		}
		best := ""
		for _, version := range versions {
			if version.checkpoint <= checkpoint && slices.Contains(kinds, version.kind) && version.stateID > best {
				best = version.stateID
			}
		}
		if best == "" {
			continue
		}
		if row, ok := v.visible(best, materialized); ok {
			out[objectID] = row
		}
	}
	return out
}

func (v *suiSQLRangeView) visible(stateID string, materialized uint64) (suiSQLObject, bool) {
	var best suiSQLObject
	var bestAt uint64
	found := false
	for _, row := range v.states[stateID] {
		at, err := historyUint(row.MaterializedAtCheckpoint)
		if err != nil || at > materialized {
			continue
		}
		if !found || at > bestAt {
			best, bestAt, found = row, at, true
		}
	}
	return best, found
}

// jsonString is JSONExtractString: the named field when it is a JSON string,
// otherwise empty.
func jsonString(raw, field string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(fields[field], &s) != nil {
		return ""
	}
	return s
}

// sample repeats portfolioSQL's selection for one sample over the range's rows
// and returns what that statement would have returned for it.
func (v *suiSQLRangeView) sample(protocol string, owner SuiAddress, snapshot suiSQLSnapshot) (suiSQLResult, error) {
	kinds, err := suiSQLProtocolKinds(protocol)
	if err != nil {
		return suiSQLResult{}, err
	}
	checkpoint, e1 := historyUint(snapshot.Checkpoint)
	materialized, e2 := historyUint(snapshot.MaterializedAtCheckpoint)
	if e1 != nil || e2 != nil {
		return suiSQLResult{}, fmt.Errorf("invalid Sui SQL completion certificate")
	}
	snapshot.ObservedObjectCount = v.count("object", snapshot.Checkpoint)
	snapshot.ObservedValueCount = "0"
	if len(kinds.values) > 0 {
		snapshot.ObservedValueCount = v.count("value", snapshot.Checkpoint)
	}
	selected := map[string][]suiSQLObject{}
	keep := func(stage map[string]suiSQLObject) {
		for id, row := range stage {
			selected[id] = append(selected[id], row)
		}
	}
	rootCandidates := map[string]bool{}
	for id, first := range v.roots {
		if first <= checkpoint {
			rootCandidates[id] = true
		}
	}
	rootStates := v.latest(rootCandidates, kinds.roots, checkpoint, materialized)
	keep(rootStates)
	roots := []suiSQLObject{}
	for _, row := range rootStates {
		if row.State == "live" && row.Owner == owner.Hex() && (row.OwnerKind == "ADDRESS" || row.OwnerKind == "CONSENSUS_ADDRESS") {
			roots = append(roots, row)
		}
	}
	related := func(rows []suiSQLObject, live bool) map[string]bool {
		out := map[string]bool{}
		for _, row := range rows {
			if row.RelatedID != "" && (!live || row.State == "live") {
				out[row.RelatedID] = true
			}
		}
		return out
	}
	directStates := v.latest(related(roots, false), kinds.direct, checkpoint, materialized)
	keep(directStates)
	if len(kinds.second) > 0 {
		keep(v.latest(related(slices.Collect(maps.Values(directStates)), true), kinds.second, checkpoint, materialized))
	}
	var values []suiSQLValue
	if protocol == "navi" {
		topology := v.latest(nil, kinds.topology, checkpoint, materialized)
		keep(topology)
		accounts := map[string]bool{owner.Hex(): true}
		receipts := map[string]bool{}
		for _, row := range roots {
			switch row.Kind {
			case "account":
				accounts[jsonString(row.Links, "accountAddress")] = true
			case "receipt":
				receipts[row.ObjectID] = true
			}
		}
		principals := map[string]bool{}
		for _, c := range v.principals {
			if first, _ := historyUint(c.First); first <= checkpoint && accounts[c.Owner] {
				principals[c.ObjectID] = true
			}
		}
		reserveTables, usersTables := map[string]bool{}, map[string]bool{}
		for _, row := range topology {
			if row.Kind == "reserve" && row.State == "live" {
				reserveTables[jsonString(row.Links, "supplyTableId")] = true
				reserveTables[jsonString(row.Links, "borrowTableId")] = true
			}
		}
		for _, row := range directStates {
			if row.State == "live" && row.Kind == "vault" {
				usersTables[jsonString(row.Links, "usersTableId")] = true
			}
		}
		children := map[string]bool{}
		for _, c := range v.children {
			first, _ := historyUint(c.First)
			if first > checkpoint {
				continue
			}
			if (c.Kind == "principal" && principals[c.ObjectID] && accounts[c.Key] && reserveTables[c.ParentID]) ||
				(c.Kind == "receiptState" && receipts[c.Key] && usersTables[c.ParentID]) {
				children[c.ObjectID] = true
			}
		}
		keep(v.latest(children, kinds.children, checkpoint, materialized))
		values = v.sampleValues(accounts, checkpoint, materialized)
	}
	// The object output keeps, per object, the newest selected version.
	result := suiSQLResult{snapshots: []suiSQLSnapshot{snapshot}, values: values}
	for _, rows := range selected {
		best := rows[0]
		for _, row := range rows[1:] {
			if row.ID > best.ID {
				best = row
			}
		}
		result.objects = append(result.objects, best)
	}
	sort.Slice(result.objects, func(i, j int) bool { return result.objects[i].ObjectID < result.objects[j].ObjectID })
	result.metadata = v.sampleMetadata(materialized)
	return result, nil
}

func (v *suiSQLRangeView) count(table, checkpoint string) string {
	if n, ok := v.counts[table][checkpoint]; ok {
		return n
	}
	// uniqExact over no rows: the grouped count has no row for that sample.
	return "0"
}

// sampleValues is the value branch at one sample: per kind, account and market
// the newest row at or before the sample and visible by it.
func (v *suiSQLRangeView) sampleValues(accounts map[string]bool, checkpoint, materialized uint64) []suiSQLValue {
	upper := fmt.Sprintf("emode:%020d:~", checkpoint)
	newest := map[string]suiSQLValue{}
	for _, row := range v.values {
		cp, e1 := historyUint(row.Checkpoint)
		at, e2 := historyUint(row.MaterializedAtCheckpoint)
		if e1 != nil || e2 != nil || row.ID < "emode:" || row.ID > upper || at > materialized || !accounts[row.Account] {
			continue
		}
		key := row.Kind + "\x00" + row.Account + "\x00" + row.Market
		if current, ok := newest[key]; ok {
			if held, _ := historyUint(current.Checkpoint); held > cp || (held == cp && current.ID >= row.ID) {
				continue
			}
		}
		newest[key] = row
	}
	out := slices.Collect(maps.Values(newest))
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sampleMetadata is the metadata branch at one sample: per coin the newest row
// at or before the sample's materialization bound and visible by it.
func (v *suiSQLRangeView) sampleMetadata(materialized uint64) []suiSQLMetadata {
	newest := map[string]suiSQLMetadata{}
	for _, row := range v.metadata {
		cp, e1 := historyUint(row.Checkpoint)
		at, e2 := historyUint(row.MaterializedAtCheckpoint)
		if e1 != nil || e2 != nil || cp > materialized || at > materialized {
			continue
		}
		if current, ok := newest[row.CoinType]; ok {
			if held, _ := historyUint(current.Checkpoint); held >= cp {
				continue
			}
		}
		newest[row.CoinType] = row
	}
	out := slices.Collect(maps.Values(newest))
	sort.Slice(out, func(i, j int) bool { return out[i].CoinType < out[j].CoinType })
	return out
}
