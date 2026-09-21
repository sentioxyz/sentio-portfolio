package portfolio

import (
	"fmt"
	"strconv"
	"strings"
)

const suiPortfolioSchemaVersion = "2"

// suiSQLKinds names a protocol's dependency levels. Every level is resolved
// through the index tables the processor publishes next to its state rows:
// PortfolioOwnerIndex orders an account's roots, PortfolioObjectIndex orders
// one object's versions. Scanning a kind's whole ID range instead costs seconds
// per stage once the state table reaches millions of rows.
type suiSQLKinds struct{ roots, direct, second, topology, children, values []string }

func (k suiSQLKinds) all() []string {
	all := []string{}
	for _, group := range [][]string{k.roots, k.direct, k.second, k.topology, k.children} {
		all = append(all, group...)
	}
	return all
}

func suiSQLProtocolKinds(protocol string) (suiSQLKinds, error) {
	switch protocol {
	case "suilend":
		// A capability names an obligation, and an obligation names its market.
		return suiSQLKinds{roots: []string{"cap"}, direct: []string{"obligation"}, second: []string{"market"}}, nil
	case "navi":
		// An account capability and a vault receipt are owned outright, and a
		// receipt names its vault. Reserves, markets and storage are one
		// protocol-wide topology every account shares, and principals and
		// receipt states are dynamic fields reached through it.
		return suiSQLKinds{
			roots:    []string{"account", "receipt"},
			direct:   []string{"vault"},
			topology: []string{"storage", "market", "reserve"},
			children: []string{"principal", "receiptState"},
			values:   []string{"emode"},
		}, nil
	}
	return suiSQLKinds{}, fmt.Errorf("unsupported Sui processor protocol")
}

// suiSQLBound names the snapshot column an ID range is bounded by. The whole
// bound string is built inside one scalar subquery: primary-key analysis takes
// a scalar as a constant, while concat over a scalar stays an expression it
// cannot evaluate, and the range degraded to a scan of every kind sorting after
// the prefix (thirteen seconds of the certificate check alone at six million rows).
type suiSQLBound struct{ column, source string }

func (b suiSQLBound) end(kind string) string {
	return `(SELECT concat(` + sqlString(kind+":") + `,leftPad(toString(` + b.column + `),20,'0'),':~') FROM ` + b.source + `)`
}

// Version 2 IDs sort by kind, source checkpoint, object, version and terminal
// suffix. These ranges let the entity primary key prune source-checkpoint
// intervals without depending on secondary indexes or numeric view conversions.
func suiSQLIDRange(kinds []string, lower *suiSQLBound, upper suiSQLBound) string {
	ranges := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		start := `id >= ` + sqlString(kind+":")
		if lower != nil {
			start = `id > ` + lower.end(kind)
		}
		ranges = append(ranges, `(`+start+` AND id <= `+upper.end(kind)+`)`)
	}
	return `(` + strings.Join(ranges, ` OR `) + `)`
}

// suiSQLKindPrefixRange selects every row of the given kinds in an index table,
// whose IDs start with the kind and then the object or owner.
func suiSQLKindPrefixRange(kinds []string) string {
	ranges := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		ranges = append(ranges, `(id >= `+sqlString(kind+":")+` AND id <= `+sqlString(kind+":~")+`)`)
	}
	return `(` + strings.Join(ranges, ` OR `) + `)`
}

// suiSQLSet turns a stage's candidate expression into a set: a subquery is one
// already, an array alias is expanded with arrayJoin. Only a set reaches the
// primary key and the skip indexes; a constant array is evaluated row by row.
func suiSQLSet(ids string) string {
	if strings.HasPrefix(ids, "(") {
		return ids
	}
	return `(SELECT arrayJoin(` + ids + `))`
}

// portfolioSQL resolves candidate identities before selecting latest lifecycle.
// Filtering owner, parent or key before that selection would resurrect an object
// whose final version was transferred, wrapped or deleted. Object-state IDs are
// immutable versions, so the raw entity history supports bounded scans without
// a redundant global latest-entity aggregation. Lifecycle and quote selections
// collapse deterministic replay rows by object identity and version ID.
func (r *suiHistoryIndex) portfolioSQL(owner SuiAddress, selection suiSQLSelection) (string, error) {
	kinds, err := suiSQLProtocolKinds(r.protocolID)
	if err != nil {
		return "", err
	}
	where := ""
	if selection.checkpoint != nil {
		if *selection.checkpoint < r.start {
			return "", fmt.Errorf("Sui history predates protocol publication")
		}
		where = " AND checkpoint <= " + strconv.FormatUint(*selection.checkpoint, 10) + " AND nextCheckpoint > " + strconv.FormatUint(*selection.checkpoint, 10)
	}
	if selection.at != nil {
		if selection.at.UnixMilli() < 0 {
			return "", fmt.Errorf("invalid Sui history timestamp")
		}
		at := strconv.FormatInt(selection.at.UnixMilli(), 10)
		where = " AND timestampMs <= " + at + " AND nextTimestampMs > " + at
	}
	ctes := []string{
		`candidate_snapshot AS (SELECT * FROM "PortfolioSnapshot" WHERE schemaVersion=` + suiPortfolioSchemaVersion + where + ` ORDER BY checkpoint DESC LIMIT 1)`,
		`snapshot AS (SELECT * FROM candidate_snapshot)`,
	}
	// Source checkpoints are encoded in the padded ID prefix, so the ID ranges
	// alone bound them through the primary key. The numeric visibility bound
	// is only evaluated on the few rows a stage has already selected: filtering
	// a whole kind range on the numeric columns costs over a second per stage.
	visible := `materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM snapshot)`
	upper := suiSQLBound{column: "checkpoint", source: "snapshot"}
	upperCheckpoint := `(SELECT checkpoint FROM snapshot)`
	// Each selection stage keeps its chosen IDs in a scalar array alias. A
	// scalar subquery is evaluated once per statement, while a CTE is expanded
	// again at every reference, so a chain of CTEs rescanned the table once per
	// dependency level and per output branch. An empty stage yields an empty array.
	// Every stage's array also feeds the one row fetch behind the object output.
	stageIDs := []string{}
	latest := func(name, ids string, objectKinds []string) {
		// Compare only the immutable IDs before fetching JSON content. For one
		// object, max(stateId) has exactly the checkpoint/version/terminal
		// ordering. The object index orders one object's versions contiguously
		// and its skip index on objectId keeps a lookup to the granules of that
		// object, instead of a scan over every version of the kind.
		selection := `SELECT max(stateId) AS selectedId FROM "PortfolioObjectIndex_raw" WHERE ` + suiSQLKindPrefixRange(objectKinds) + ` AND checkpoint <= ` + upperCheckpoint
		if ids != "" {
			selection += ` AND objectId IN ` + suiSQLSet(ids)
		}
		ctes = append(ctes, `(SELECT groupArray(selectedId) FROM (`+selection+` GROUP BY objectId)) AS `+name+`_ids`)
		// arrayJoin turns the cached array back into a set: only a set reaches the
		// primary key, a constant array is evaluated row by row over the table.
		ctes = append(ctes, name+` AS (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(`+name+`_ids)) AND `+visible+` ORDER BY id DESC,materializedAtCheckpoint DESC LIMIT 1 BY objectId)`)
		stageIDs = append(stageIDs, name+"_ids")
	}
	// The owner index orders an account's capabilities contiguously, so the
	// candidates are one primary-key prefix range per root kind.
	ranges := make([]string, 0, len(kinds.roots))
	for _, kind := range kinds.roots {
		ranges = append(ranges, `(id > `+sqlString(kind+":"+owner.Hex()+":")+` AND id <= `+sqlString(kind+":"+owner.Hex()+":~")+`)`)
	}
	candidates := `SELECT groupUniqArray(objectId) FROM "PortfolioOwnerIndex_raw" WHERE (` + strings.Join(ranges, ` OR `) + `) AND checkpoint <= ` + upperCheckpoint
	ctes = append(ctes, `(`+candidates+`) AS root_candidates`)
	latest("root_states", "root_candidates", kinds.roots)
	ctes = append(ctes, `roots AS (SELECT * FROM root_states WHERE state='live' AND owner=`+sqlString(owner.Hex())+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS'))`)
	latest("direct_states", `(SELECT relatedId FROM roots WHERE relatedId != '')`, kinds.direct)
	if len(kinds.second) > 0 {
		// A Suilend capability points through its obligation to the market.
		latest("second_states", `(SELECT relatedId FROM direct_states WHERE state='live' AND relatedId != '')`, kinds.second)
	}
	valueBranch := ""
	if r.protocolID == "navi" {
		// The topology is protocol-wide and small enough to read whole: reading
		// it is what names the reserve tables a principal must sit under, so it
		// cannot be derived from the account's own rows.
		latest("topology", "", kinds.topology)
		// A wallet's lending positions are attributed to the wallet itself and
		// to every account capability it owns.
		ctes = append(ctes, `accounts AS (SELECT `+sqlString(owner.Hex())+` AS account UNION DISTINCT SELECT JSONExtractString(links,'accountAddress') FROM roots WHERE kind='account')`)
		// A principal is a dynamic field, so it is object-owned by its reserve
		// table rather than by the account: the owner index carries the account
		// the field's name names, which is the only skip-indexed way in. The
		// kind is the largest in the index by an order of magnitude, so scanning
		// its ID range here is the shape that cost Suilend seconds per read.
		ctes = append(ctes, `(SELECT groupUniqArray(objectId) FROM "PortfolioOwnerIndex_raw" WHERE `+suiSQLKindPrefixRange([]string{"principal"})+` AND owner IN (SELECT account FROM accounts) AND checkpoint <= `+upperCheckpoint+`) AS principal_candidates`)
		// The index says which objects an account has ever had a position in.
		// These key and parent checks are what decide whether a row is a live
		// position in a reserve of the observed topology, and they are repeated
		// by the calculator over the rows this finally selects.
		reserveTables := `SELECT JSONExtractString(links,'supplyTableId') FROM topology WHERE kind='reserve' AND state='live'` +
			` UNION DISTINCT SELECT JSONExtractString(links,'borrowTableId') FROM topology WHERE kind='reserve' AND state='live'`
		children := `(SELECT objectId FROM "PortfolioObjectState_raw" WHERE checkpoint <= ` + upperCheckpoint + ` AND (` +
			`(kind='principal' AND objectId IN ` + suiSQLSet("principal_candidates") + ` AND key IN (SELECT account FROM accounts) AND parentId IN (` + reserveTables + `))` +
			` OR (kind='receiptState' AND key IN (SELECT objectId FROM roots WHERE kind='receipt') AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live' AND kind='vault'))))`
		latest("child_states", children, kinds.children)
		// E-mode is a running per-account setting an event carries, not an
		// object, so the latest row at or before P is the one in force. It has
		// no lower bound for the same reason.
		valueBranch = `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") +
			` AS payload FROM (SELECT * FROM "PortfolioValue" WHERE ` + suiSQLIDRange([]string{"emode"}, nil, upper) + ` AND ` + visible +
			` AND account IN (SELECT account FROM accounts) ORDER BY checkpoint DESC LIMIT 1 BY kind,account,market)`
	}
	// The object output is one primary-key fetch over every stage's array. The
	// stage CTEs stay for the protocols' set logic, but a union of them here was
	// expanded again by each output branch, fetching every stage several times
	// over. Identical IDs may be reached through multiple owned capabilities.
	ctes = append(ctes, `objects AS (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(arrayConcat(`+strings.Join(stageIDs, ",")+`))) AND `+visible+` ORDER BY id DESC,materializedAtCheckpoint DESC LIMIT 1 BY objectId)`)
	// Token metadata is a few dozen rows per protocol. Reading all of it costs
	// less than deriving the referenced coin types from another pass over objects.
	ctes = append(ctes, `metadata AS (SELECT * FROM "PortfolioTokenMetadata" WHERE checkpoint <= (SELECT materializedAtCheckpoint FROM snapshot) AND materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM snapshot) ORDER BY checkpoint DESC LIMIT 1 BY coinType)`)
	snapshotFields := []string{"id", "checkpoint", "timestampMs", "digest", "schemaVersion", "startCheckpoint", "materializedAtCheckpoint", "nextCheckpoint", "nextTimestampMs", "previousCheckpoint", "objectCount", "valueCount", "observedObjectCount", "observedValueCount"}
	// Counts are computed only in this output branch. Putting these scans into
	// snapshot's WHERE makes every dependency reuse expand the global scans again.
	allKinds := kinds.all()
	interval := `materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM candidate_snapshot)`
	lower := suiSQLBound{column: "previousCheckpoint", source: "candidate_snapshot"}
	// A protocol without value rows certifies valueCount 0, and reading a table
	// its processor never creates would fail the whole statement, so the count
	// is only observed where the schema has one.
	observedValues := "0"
	if len(kinds.values) > 0 {
		observedValues = `(SELECT uniqExact(id) FROM "PortfolioValue_raw" WHERE ` + interval + ` AND ` + suiSQLIDRange(kinds.values, &lower, upper) + `)`
	}
	snapshotOutput := `(SELECT *, (SELECT uniqExact(id) FROM "PortfolioObjectState_raw" WHERE ` + interval + ` AND ` + suiSQLIDRange(allKinds, &lower, upper) + `) AS observedObjectCount,` + observedValues + ` AS observedValueCount FROM snapshot)`

	branches := []string{
		`SELECT 'snapshot' AS rowType,` + sqlPayload(snapshotFields...) + ` AS payload FROM ` + snapshotOutput,
		`SELECT 'object' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM objects`,
		`SELECT 'metadata' AS rowType,` + sqlPayload("coinType", "decimals", "symbol", "name", "checkpoint", "materializedAtCheckpoint", "status") + ` AS payload FROM metadata`,
	}
	if valueBranch != "" {
		branches = append(branches, valueBranch)
	}
	return "WITH " + strings.Join(ctes, ",\n") + "\n" + strings.Join(branches, "\nUNION ALL\n"), nil
}
func suiSQLObjectPayload() string {
	return sqlPayload("id", "objectId", "kind", "version", "digest", "state", "ownerKind", "owner", "objectType", "content", "checkpoint", "timestampMs", "transactionDigest", "materializedAtCheckpoint", "parentId", "key", "relatedId", "links")
}
