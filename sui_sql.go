package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const suiSQLRowLimit = 10000

type suiSQLSelection struct {
	checkpoint *uint64
	at         *time.Time
}
type suiSQLRow struct {
	RowType string `json:"rowType"`
	Payload string `json:"payload"`
	// Content carries a range statement's object content outside the payload,
	// so the largest field is escaped once rather than twice.
	Content string `json:"objectContent,omitempty"`
}
type suiSQLSnapshot struct {
	ObjectCount, ValueCount, ObservedObjectCount, ObservedValueCount    string
	ID, Checkpoint, TimestampMs, Digest, StartCheckpoint, SchemaVersion string
	MaterializedAtCheckpoint, NextCheckpoint, NextTimestampMs           string
	PreviousCheckpoint                                                  string
}
type suiSQLObject struct {
	ID, ObjectID, Kind, Version, Digest, State, OwnerKind, Owner    string
	ObjectType, Content, Checkpoint, TimestampMs, TransactionDigest string
	MaterializedAtCheckpoint, ParentID, Key, RelatedID, Links       string
}
type suiSQLMetadata struct{ Status, CoinType, Decimals, Symbol, Name, Checkpoint, MaterializedAtCheckpoint string }

// suiSQLValue is a PortfolioValue row: a protocol fact that is not an object,
// such as NAVI's per-account e-mode setting, which only its events report.
type suiSQLValue struct {
	suiProtocolValue
	Checkpoint               string `json:"checkpoint"`
	MaterializedAtCheckpoint string `json:"materializedAtCheckpoint"`
}
type suiSQLData struct {
	pin      SuiCheckpoint
	snapshot suiSQLSnapshot
	owner    SuiAddress
	objects  map[string]suiSQLObject
	metadata map[string]SuiCoinMetadata
	values   []suiProtocolValue
	// Shared by every account of one snapshot calculator; nil parses per read.
	markets *suilendMarketMemo
}

func (d *suiSQLData) suilendMarket(market SuiObject) (suilendParsedMarket, error) {
	return d.markets.market(market)
}

// Each read is a single version-pinned SQL statement. The snapshot row survives
// an empty address selection, so an empty wallet cannot hide missing history.
func (r *suiHistoryIndex) readSQLPortfolio(ctx context.Context, owner SuiAddress, selection suiSQLSelection) (SuiProtocolPositions, error) {
	data, err := r.readSQL(ctx, owner, selection)
	if err != nil {
		return r.result(SuiCheckpoint{}), err
	}
	return r.samplePortfolio(ctx, owner, data, selection)
}

// samplePortfolio calculates one certified sample and labels its groups with the
// sample they were answered from and the selection that chose it.
func (r *suiHistoryIndex) samplePortfolio(ctx context.Context, owner SuiAddress, data *suiSQLData, selection suiSQLSelection) (SuiProtocolPositions, error) {
	result, err := r.calculatePortfolio(ctx, owner, data)
	if err != nil {
		return result, err
	}
	for i := range result.Groups {
		m := result.Groups[i].Metadata
		if m == nil {
			m = map[string]any{}
			result.Groups[i].Metadata = m
		}
		m["stateMode"] = "indexed"
		if selection.at != nil || selection.checkpoint != nil {
			m["stateMode"] = "historical"
		}
		m["sampleIntervalSeconds"] = suiSampleIntervalSeconds(data)
		m["materializedAtCheckpoint"] = data.snapshot.MaterializedAtCheckpoint
		if selection.at != nil {
			m["requestedTimestamp"] = selection.at.UTC().Format(time.RFC3339Nano)
		}
		if selection.checkpoint != nil {
			m["requestedCheckpoint"] = strconv.FormatUint(*selection.checkpoint, 10)
		}
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}

// A completed sample owns [timestampMs, nextTimestampMs). Samples land on the
// first checkpoint at or after an interval boundary, so the bound is the
// interval plus a sub-second offset; round to the minute both sides use.
func suiSampleIntervalSeconds(data *suiSQLData) int64 {
	next, err := historyUint(data.snapshot.NextTimestampMs)
	start := uint64(data.pin.Timestamp.UnixMilli())
	if err != nil || next <= start {
		return 3600
	}
	minutes := (next - start + 30_000) / 60_000
	if minutes == 0 {
		return 60
	}
	if minutes > uint64(1<<40) {
		return 3600
	}
	return int64(minutes) * 60
}

// calculatePortfolio turns one snapshot's indexed objects into position groups.
// Quantities are computed locally from those objects; no further read happens.
func (r *suiHistoryIndex) calculatePortfolio(ctx context.Context, owner SuiAddress, data *suiSQLData) (SuiProtocolPositions, error) {
	result := r.result(data.pin)
	switch r.protocolID {
	case "suilend":
		state, err := loadSuilend(ctx, owner, data)
		if err != nil {
			return result, err
		}
		result.Groups, err = suilendLending(ctx, owner, data.pin, data, state)
		if err != nil {
			return result, err
		}
	case "navi":
		// One indexed snapshot answers both products, and either failing fails
		// the read: unlike the node path there is no partial head state to
		// salvage, and a sample that cannot be read whole is not a portfolio.
		state, err := data.naviState()
		if err != nil {
			return result, err
		}
		lending, err := naviLending(ctx, owner, data.pin, data, state)
		if err != nil {
			return result, err
		}
		vaults, err := suiVaults(ctx, r.protocolID, data, state)
		if err != nil {
			return result, err
		}
		result.Groups = append(lending, vaults...)
	default:
		return result, fmt.Errorf("unsupported Sui processor protocol")
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}

func (r *suiHistoryIndex) readSQL(ctx context.Context, owner SuiAddress, selection suiSQLSelection) (*suiSQLData, error) {
	query, err := r.portfolioSQL(owner, selection)
	if err != nil {
		return nil, err
	}
	rows, err := r.executeSQL(ctx, query)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeSuiSQLRows(rows)
	if err != nil {
		return nil, err
	}
	return r.sqlData(owner, decoded, selection)
}

// errSuiSQLPaged is a response that did not carry every row of its statement: a
// cursor, truncation, or the row limit. A range read answers it by asking for
// fewer samples; a single-sample read has nothing smaller to ask for.
var errSuiSQLPaged = errors.New("Sui SQL query failed or returned incomplete data")

// executeSQL runs one version-pinned statement, once, and returns its rows only
// when the response is the statement's whole result.
func (r *suiHistoryIndex) executeSQL(ctx context.Context, query string) ([]suiSQLRow, error) {
	var response struct {
		Result *struct {
			Rows       []suiSQLRow     `json:"rows"`
			Cursor     json.RawMessage `json:"cursor"`
			NextCursor json.RawMessage `json:"nextCursor"`
			Truncated  bool            `json:"truncated"`
			HasMore    bool            `json:"hasMore"`
		} `json:"result"`
		Error  json.RawMessage   `json:"error"`
		Errors []json.RawMessage `json:"errors"`
		Cursor json.RawMessage   `json:"cursor"`
	}
	version, _ := strconv.ParseUint(r.config.ProcessorVersion, 10, 64)
	body := map[string]any{"version": version, "sqlQuery": map[string]any{"sql": query, "size": suiSQLRowLimit}, "sync_v1": true}
	if err := r.request(ctx, http.MethodPost, r.config.SQLURL, body, &response); err != nil {
		if errors.Is(err, errSentioResponseTooLarge) {
			return nil, errSuiSQLPaged
		}
		return nil, err
	}
	nonempty := func(v json.RawMessage) bool {
		return len(v) > 0 && string(v) != "null" && string(v) != "\"\"" && string(v) != "{}"
	}
	if nonempty(response.Error) || len(response.Errors) > 0 || response.Result == nil {
		return nil, fmt.Errorf("Sui SQL query failed or returned incomplete data")
	}
	if nonempty(response.Cursor) || nonempty(response.Result.Cursor) || nonempty(response.Result.NextCursor) || response.Result.Truncated || response.Result.HasMore || len(response.Result.Rows) >= suiSQLRowLimit {
		return nil, errSuiSQLPaged
	}
	return response.Result.Rows, nil
}

// suiSQLResult is one sample's rows, decoded but not yet checked against each
// other or against the certificate.
type suiSQLResult struct {
	snapshots []suiSQLSnapshot
	objects   []suiSQLObject
	metadata  []suiSQLMetadata
	values    []suiSQLValue
}

func decodeSuiSQLRows(rows []suiSQLRow) (suiSQLResult, error) {
	var result suiSQLResult
	for _, row := range rows {
		switch row.RowType {
		case "snapshot":
			var snapshot suiSQLSnapshot
			if err := json.Unmarshal([]byte(row.Payload), &snapshot); err != nil {
				return result, fmt.Errorf("invalid Sui SQL snapshot")
			}
			result.snapshots = append(result.snapshots, snapshot)
		case "object":
			var obj suiSQLObject
			if err := json.Unmarshal([]byte(row.Payload), &obj); err != nil {
				return result, fmt.Errorf("invalid Sui SQL object")
			}
			result.objects = append(result.objects, obj)
		case "metadata":
			var meta suiSQLMetadata
			if err := json.Unmarshal([]byte(row.Payload), &meta); err != nil {
				return result, fmt.Errorf("invalid Sui SQL token metadata")
			}
			result.metadata = append(result.metadata, meta)
		case "value":
			var value suiSQLValue
			if err := json.Unmarshal([]byte(row.Payload), &value); err != nil {
				return result, fmt.Errorf("invalid Sui SQL protocol value")
			}
			result.values = append(result.values, value)
		default:
			return result, fmt.Errorf("unknown Sui SQL row type")
		}
	}
	return result, nil
}

// sqlData accepts one sample only when its rows agree with its certificate. A
// single read and each sample of a range read go through the same checks.
func (r *suiHistoryIndex) sqlData(owner SuiAddress, rows suiSQLResult, selection suiSQLSelection) (*suiSQLData, error) {
	data := &suiSQLData{owner: owner, objects: map[string]suiSQLObject{}, metadata: map[string]SuiCoinMetadata{}}
	for _, obj := range rows.objects {
		if _, exists := data.objects[obj.ObjectID]; exists {
			return nil, fmt.Errorf("duplicate Sui SQL object")
		}
		data.objects[obj.ObjectID] = obj
	}
	for _, meta := range rows.metadata {
		if meta.Status != "found" {
			continue
		}
		decimals, e := strconv.Atoi(meta.Decimals)
		if e != nil {
			return nil, fmt.Errorf("invalid Sui SQL token precision")
		}
		coin, e := suiCoinMetadataFrom(meta.CoinType, &decimals, &meta.Symbol, meta.Name)
		if e != nil {
			return nil, e
		}
		if _, exists := data.metadata[meta.CoinType]; exists {
			return nil, fmt.Errorf("duplicate Sui SQL token metadata")
		}
		data.metadata[meta.CoinType] = coin
	}
	values := rows.values
	if len(rows.snapshots) != 1 {
		return nil, fmt.Errorf("Sui index has no completed sample for the requested hour")
	}
	data.snapshot = rows.snapshots[0]
	snap := data.snapshot
	var err error
	expectedObjects, eObjects := historyUint(snap.ObjectCount)
	observedObjects, aObjects := historyUint(snap.ObservedObjectCount)
	expectedValues, eValues := historyUint(snap.ValueCount)
	observedValues, aValues := historyUint(snap.ObservedValueCount)
	kinds, kindErr := suiSQLProtocolKinds(r.protocolID)
	// The certificate's counts guard against reading a sample before every row
	// the processor wrote has become visible, so fewer visible rows than
	// certified is incomplete. More visible rows than certified is not: the
	// processor counts its rows with a store list at the closing sample, and
	// that list can miss rows a concurrent commit is flushing, while every row
	// it wrote is deterministic and visible here. Suilend's state is entirely
	// object rows, so a certified value row would be a contract the reader does
	// not implement rather than something to ignore. NAVI does implement them:
	// its e-mode values are certified and counted the same way as object rows.
	if eObjects != nil || aObjects != nil || eValues != nil || aValues != nil || kindErr != nil ||
		observedObjects < expectedObjects || observedValues < expectedValues ||
		(len(kinds.values) == 0 && snap.ValueCount != "0") {
		return nil, fmt.Errorf("Sui SQL snapshot materialization is incomplete")
	}

	data.pin, err = indexedSuiCheckpoint(snap.Checkpoint, snap.TimestampMs, snap.Digest, r.start)
	if err != nil {
		return nil, err
	}
	materialized, e1 := historyUint(snap.MaterializedAtCheckpoint)
	next, e2 := historyUint(snap.NextCheckpoint)
	nextTime, e3 := historyUint(snap.NextTimestampMs)
	previous, e4 := historyUint(snap.PreviousCheckpoint)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || previous < r.start-1 || previous >= data.pin.Sequence || snap.SchemaVersion != suiPortfolioSchemaVersion || snap.StartCheckpoint != strconv.FormatUint(r.start, 10) || snap.ID != fmt.Sprintf("%020d", data.pin.Sequence) || materialized <= data.pin.Sequence || next != materialized || nextTime <= uint64(data.pin.Timestamp.UnixMilli()) || nextTime > 253402300799999 {
		return nil, fmt.Errorf("invalid Sui SQL completion certificate")
	}
	if selection.checkpoint != nil && (*selection.checkpoint < data.pin.Sequence || *selection.checkpoint >= next) {
		return nil, fmt.Errorf("Sui index has not completed the requested checkpoint")
	}
	if selection.at != nil && (selection.at.Before(data.pin.Timestamp) || selection.at.UnixMilli() >= int64(nextTime)) {
		return nil, fmt.Errorf("Sui index has not completed the requested hour")
	}
	seenValues := map[string]bool{}
	for _, value := range values {
		cp, e1 := historyUint(value.Checkpoint)
		at, e2 := historyUint(value.MaterializedAtCheckpoint)
		_, e3 := historyUint(value.Market)
		account, e4 := ParseSuiAddress(value.Account)
		// The stored ID carries the source checkpoint; the identity the
		// calculator keys on is the account and market the row is about.
		identity := value.Kind + ":" + value.Market + ":" + value.Account
		if len(kinds.values) == 0 || value.Kind != "emode" || e1 != nil || e2 != nil || e3 != nil || e4 != nil ||
			account.Hex() != value.Account || cp < r.start || cp > data.pin.Sequence || at < cp || at > materialized ||
			value.ID != fmt.Sprintf("%s:%020d:%s", value.Kind, cp, identity) || seenValues[identity] {
			return nil, fmt.Errorf("invalid Sui indexed protocol value")
		}
		seenValues[identity] = true
		value.suiProtocolValue.ID = identity
		data.values = append(data.values, value.suiProtocolValue)
	}
	for _, obj := range mapSuiSQLObjects(data.objects) {
		cp, e1 := historyUint(obj.Checkpoint)
		at, e2 := historyUint(obj.MaterializedAtCheckpoint)
		if e1 != nil || e2 != nil || cp > data.pin.Sequence || at < cp || at > materialized || obj.ObjectID == "" || obj.Kind == "" {
			return nil, fmt.Errorf("Sui SQL object exceeds completed snapshot")
		}
		if obj.State == "live" {
			if _, e := obj.object(); e != nil {
				return nil, e
			}
		} else if obj.State != "deleted" && obj.State != "wrapped" {
			return nil, fmt.Errorf("invalid Sui indexed lifecycle")
		}
	}
	return data, nil
}
func mapSuiSQLObjects(m map[string]suiSQLObject) []suiSQLObject {
	out := make([]suiSQLObject, 0, len(m))
	for _, o := range m {
		out = append(out, o)
	}
	return out
}
func sortedSuiSQLObjects(m map[string]suiSQLObject) []suiSQLObject {
	out := mapSuiSQLObjects(m)
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out
}
func (o suiSQLObject) object() (SuiObject, error) {
	version, err := historyUint(o.Version)
	if err != nil || version == 0 || o.Content == "" {
		return SuiObject{}, fmt.Errorf("Sui indexed object content is incomplete")
	}
	return SuiObject{ID: o.ObjectID, Version: version, Digest: o.Digest, ObjectType: o.ObjectType, Content: o.Content, Owner: o.Owner, OwnerKind: o.OwnerKind, PreviousTransaction: o.TransactionDigest}, nil
}
func (d *suiSQLData) Objects(_ context.Context, ids []string) (map[string]SuiObject, error) {
	result := map[string]SuiObject{}
	for _, id := range ids {
		row, ok := d.objects[id]
		if !ok || row.State != "live" {
			continue
		}
		obj, err := row.object()
		if err != nil {
			return nil, err
		}
		result[id] = obj
	}
	return result, nil
}
func (d *suiSQLData) OwnedObjects(_ context.Context, owner SuiAddress, typ string) ([]SuiObject, error) {
	result := []SuiObject{}
	for _, row := range d.objects {
		if row.State != "live" || row.Owner != owner.Hex() || (row.OwnerKind != "ADDRESS" && row.OwnerKind != "CONSENSUS_ADDRESS") || !suiObjectTypeMatches(row.ObjectType, suiType(typ)) {
			continue
		}
		obj, err := row.object()
		if err != nil {
			return nil, err
		}
		result = append(result, obj)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (d *suiSQLData) DynamicFields(_ context.Context, parent string) ([]SuiObject, error) {
	result := []SuiObject{}
	for _, row := range sortedSuiSQLObjects(d.objects) {
		if row.State == "live" && row.ParentID == parent {
			obj, e := row.object()
			if e != nil {
				return nil, e
			}
			result = append(result, obj)
		}
	}
	return result, nil
}
func (d *suiSQLData) LatestCheckpoint(context.Context) (SuiCheckpoint, error) { return d.pin, nil }
func (d *suiSQLData) Holdings(context.Context, SuiAddress, *SuiCheckpoint) (SuiHoldings, error) {
	return SuiHoldings{}, fmt.Errorf("protocol index does not provide wallet coin balances")
}
func (d *suiSQLData) Close() {}
func (d *suiSQLData) CoinMetadata(_ context.Context, coins []string) (map[string]SuiCoinMetadata, map[string]error, error) {
	result := map[string]SuiCoinMetadata{}
	for _, coin := range coins {
		meta, ok := d.metadata[coin]
		if !ok {
			return nil, nil, fmt.Errorf("Sui indexed token metadata is incomplete")
		}
		// Validate on use: a full daily inventory can contain an unrelated coin
		// with unusable metadata without invalidating every other account.
		decimals := int(meta.Decimals)
		if _, err := suiCoinMetadataFrom(coin, &decimals, &meta.Symbol, meta.Name); err != nil {
			return nil, nil, err
		}
		result[coin] = meta
	}
	return result, nil, nil
}

// naviState groups one snapshot's rows into the state both NAVI products read.
// The row's own key and parent are kept: the calculator re-checks that every
// principal sits in a reserve of the observed topology and is named by one of
// the wallet's accounts, so a candidate the SQL over-selected is rejected here
// rather than silently attributed.
func (d *suiSQLData) naviState() (suiProtocolState, error) {
	state := suiProtocolState{Emodes: d.values}
	// Object ID order, not map order: the calculators emit components in the
	// order they meet principals, and a stored sample must not depend on a hash.
	for _, row := range sortedSuiSQLObjects(d.objects) {
		if row.State != "live" {
			continue
		}
		object, err := row.object()
		if err != nil {
			return state, err
		}
		key := row.Key
		switch row.Kind {
		case "account", "receipt":
			if row.Owner != d.owner.Hex() || (row.OwnerKind != "ADDRESS" && row.OwnerKind != "CONSENSUS_ADDRESS") {
				return state, fmt.Errorf("indexed NAVI root is not owned by the account")
			}
			// A receipt is keyed by the vault it names, the way the node path
			// keys it from the receipt's own vault_address field.
			if row.Kind == "receipt" {
				key = row.RelatedID
			}
			state.Owned = append(state.Owned, protocolObject(object, row.Kind, key))
		case "storage", "market", "reserve":
			state.Topology = append(state.Topology, protocolObject(object, row.Kind, key))
		case "principal":
			// A principal is object-owned by its reserve table, so the account
			// it belongs to is the field's name, which the processor stores as
			// the row's key.
			principal := protocolObject(object, row.Kind, key)
			principal.Owner = key
			state.Principals = append(state.Principals, principal)
		case "vault":
			state.Vaults = append(state.Vaults, protocolObject(object, row.Kind, key))
		case "receiptState":
			state.ReceiptStates = append(state.ReceiptStates, protocolObject(object, row.Kind, key))
		default:
			return state, fmt.Errorf("unknown indexed NAVI object kind")
		}
	}
	return state, nil
}

// sqlString only sees canonical addresses or compiler-owned literals. Keep SQL
// escaping explicit so a future caller cannot turn an identifier into syntax.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, "\\", "\\\\"), "'", "\\'") + "'"
}
func sqlPayload(fields ...string) string {
	pairs := []string{}
	for _, field := range fields {
		pairs = append(pairs, sqlString(field), "toString(\""+field+"\")")
	}
	return "toJSONString(map(" + strings.Join(pairs, ",") + "))"
}
