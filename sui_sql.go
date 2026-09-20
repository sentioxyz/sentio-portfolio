package portfolio

import (
	"context"
	"encoding/json"
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
type suiSQLValue struct {
	suiProtocolValue
	Checkpoint, MaterializedAtCheckpoint string
}
type suiSQLData struct {
	pin      SuiCheckpoint
	snapshot suiSQLSnapshot
	owner    SuiAddress
	objects  map[string]suiSQLObject
	quotes   []suiSQLObject
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
	if r.config.SuiPortfolioSchemaVersion == 3 {
		return r.readDailyPortfolio(ctx, owner, selection)
	}
	data, err := r.readSQL(ctx, owner, selection)
	if err != nil {
		return r.result(SuiCheckpoint{}), err
	}
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

// calculatePortfolio is shared by SQL readers and processor-side daily snapshots.
func (r *suiHistoryIndex) calculatePortfolio(ctx context.Context, owner SuiAddress, data *suiSQLData) (SuiProtocolPositions, error) {
	result := r.result(data.pin)
	var err error
	switch r.protocolID {
	case "suilend":
		state, e := loadSuilend(ctx, owner, data)
		if e != nil {
			return result, e
		}
		result.Groups, err = suilendLending(ctx, owner, data.pin, data, state)
	case "cetus", "bluefin":
		pkg := cetusCLMMPackage
		if r.protocolID == "bluefin" {
			pkg = bluefinCLMMPackage
		}
		positions, pools, e := loadSuiCLMM(ctx, owner, data, pkg)
		if e != nil {
			return result, e
		}
		result.Groups, err = suiCLMMGroups(ctx, data, r.protocolID, positions, pools, data.pin)
	case "navi":
		state, e := data.naviState()
		if e != nil {
			return result, e
		}
		result.Groups, err = naviLending(ctx, owner, data.pin, data, state)
		if err == nil {
			var groups []SuiProtocolGroup
			groups, err = suiVaults(ctx, r.protocolID, data, state)
			result.Groups = append(result.Groups, groups...)
		}
	case "volo-vaults":
		state, e := r.loadVolo(context.WithValue(ctx, suiSQLDataKey{}, data), owner, data.pin)
		if e != nil {
			return result, e
		}
		result.Groups, err = suiVaults(ctx, r.protocolID, data, state)
	default:
		return result, fmt.Errorf("unsupported Sui processor protocol")
	}
	if err != nil {
		return result, err
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}

type suiSQLDataKey struct{}

func (r *suiHistoryIndex) readSQL(ctx context.Context, owner SuiAddress, selection suiSQLSelection) (*suiSQLData, error) {
	query, err := r.portfolioSQL(owner, selection)
	if err != nil {
		return nil, err
	}
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
		return nil, err
	}
	nonempty := func(v json.RawMessage) bool {
		return len(v) > 0 && string(v) != "null" && string(v) != "\"\"" && string(v) != "{}"
	}
	if nonempty(response.Error) || len(response.Errors) > 0 || response.Result == nil || nonempty(response.Cursor) || nonempty(response.Result.Cursor) || nonempty(response.Result.NextCursor) || response.Result.Truncated || response.Result.HasMore || len(response.Result.Rows) >= suiSQLRowLimit {
		return nil, fmt.Errorf("Sui SQL query failed or returned incomplete data")
	}
	data := &suiSQLData{owner: owner, objects: map[string]suiSQLObject{}, metadata: map[string]SuiCoinMetadata{}}
	snapshots := 0
	var values []suiSQLValue
	for _, row := range response.Result.Rows {
		switch row.RowType {
		case "snapshot":
			snapshots++
			if err := json.Unmarshal([]byte(row.Payload), &data.snapshot); err != nil {
				return nil, fmt.Errorf("invalid Sui SQL snapshot")
			}
		case "object", "quote":
			var obj suiSQLObject
			if err := json.Unmarshal([]byte(row.Payload), &obj); err != nil {
				return nil, fmt.Errorf("invalid Sui SQL object")
			}
			if row.RowType == "quote" {
				data.quotes = append(data.quotes, obj)
			} else {
				if _, exists := data.objects[obj.ObjectID]; exists {
					return nil, fmt.Errorf("duplicate Sui SQL object")
				}
				data.objects[obj.ObjectID] = obj
			}
		case "metadata":
			var meta suiSQLMetadata
			if err := json.Unmarshal([]byte(row.Payload), &meta); err != nil {
				return nil, fmt.Errorf("invalid Sui SQL token metadata")
			}
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
		case "value":
			var value suiSQLValue
			if err := json.Unmarshal([]byte(row.Payload), &value); err != nil {
				return nil, fmt.Errorf("invalid Sui SQL value")
			}
			values = append(values, value)
		default:
			return nil, fmt.Errorf("unknown Sui SQL row type")
		}
	}
	if snapshots != 1 {
		return nil, fmt.Errorf("Sui index has no completed sample for the requested hour")
	}
	snap := data.snapshot
	expectedObjects, eObjects := historyUint(snap.ObjectCount)
	observedObjects, aObjects := historyUint(snap.ObservedObjectCount)
	expectedValues, eValues := historyUint(snap.ValueCount)
	observedValues, aValues := historyUint(snap.ObservedValueCount)
	// The certificate's counts guard against reading a sample before every row
	// the processor wrote has become visible, so fewer visible rows than
	// certified is incomplete. More visible rows than certified is not: the
	// processor counts its rows with a store list at the closing sample, and
	// that list can miss rows a concurrent commit is flushing, while every row
	// it wrote is deterministic and visible here.
	if eObjects != nil || aObjects != nil || eValues != nil || aValues != nil || observedObjects < expectedObjects || observedValues < expectedValues {
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
		identity := "emode:" + value.Market + ":" + value.Account
		if r.protocolID != "navi" || value.Kind != "emode" || e1 != nil || e2 != nil || e3 != nil || e4 != nil || account.Hex() != value.Account || cp < r.start || cp > data.pin.Sequence || at < cp || at > materialized || value.ID != fmt.Sprintf("emode:%020d:%s", cp, identity) || seenValues[identity] {
			return nil, fmt.Errorf("invalid Sui indexed e-mode value")
		}
		seenValues[identity] = true
		// Storage IDs include source time; the calculator uses the stable
		// account/market identity after the historical row has been validated.
		value.ID = identity
		data.values = append(data.values, value.suiProtocolValue)
	}
	for _, obj := range append(mapSuiSQLObjects(data.objects), data.quotes...) {
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
	for _, row := range d.objects {
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
func (d *suiSQLData) naviState() (suiProtocolState, error) {
	s := suiProtocolState{Emodes: d.values}
	for _, row := range d.objects {
		if row.State != "live" {
			continue
		}
		obj, err := row.object()
		if err != nil {
			return s, err
		}
		key := row.Key
		switch row.Kind {
		case "account", "receipt":
			if row.Owner == d.owner.Hex() && (row.OwnerKind == "ADDRESS" || row.OwnerKind == "CONSENSUS_ADDRESS") {
				if row.Kind == "receipt" {
					key = row.RelatedID
				}
				s.Owned = append(s.Owned, protocolObject(obj, row.Kind, key))
			}
		case "storage", "market", "reserve":
			s.Topology = append(s.Topology, protocolObject(obj, row.Kind, key))
		case "principal":
			p := protocolObject(obj, row.Kind, key)
			p.Owner = key
			s.Principals = append(s.Principals, p)
		case "vault":
			s.Vaults = append(s.Vaults, protocolObject(obj, row.Kind, key))
		case "receiptState":
			s.ReceiptStates = append(s.ReceiptStates, protocolObject(obj, row.Kind, key))
		}
	}
	return s, nil
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

func (r *suiHistoryIndex) objects(ctx context.Context, ids []string, pin SuiCheckpoint, required bool) (map[string]SuiObject, map[string]suiSQLObject, error) {
	data, ok := ctx.Value(suiSQLDataKey{}).(*suiSQLData)
	if !ok {
		return nil, nil, fmt.Errorf("Sui SQL state is missing")
	}
	refs := map[string]suiSQLObject{}
	for _, id := range ids {
		if row, exists := data.objects[id]; exists {
			refs[id] = row
		} else if required {
			return nil, nil, fmt.Errorf("Sui indexed dependency is missing")
		}
	}
	objects, err := data.Objects(ctx, ids)
	return objects, refs, err
}
