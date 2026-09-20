package portfolio

import (
	"fmt"
	"strconv"
	"strings"
)

const suiPortfolioSchemaVersion = "2"

type suiSQLKinds struct {
	roots, direct, second, topology, children, oracle, prices []string
	// indexed protocols also publish PortfolioOwnerIndex and PortfolioObjectIndex:
	// narrow rows ordered by owner and by object that point at state rows.
	indexed bool
}

func suiSQLProtocolKinds(protocol string) (suiSQLKinds, error) {
	switch protocol {
	case "suilend":
		return suiSQLKinds{roots: []string{"cap"}, direct: []string{"obligation"}, second: []string{"market"}, indexed: true}, nil
	case "navi":
		return suiSQLKinds{roots: []string{"account", "receipt"}, direct: []string{"vault"}, topology: []string{"storage", "market", "reserve"}, children: []string{"principal", "receiptState"}, indexed: true}, nil
	case "cetus":
		return suiSQLKinds{roots: []string{"position"}, direct: []string{"pool"}, children: []string{"accounting", "tick"}}, nil
	case "bluefin":
		return suiSQLKinds{roots: []string{"position"}, direct: []string{"pool"}, children: []string{"tick"}}, nil
	case "volo-vaults":
		return suiSQLKinds{roots: []string{"receipt"}, direct: []string{"vault"}, children: []string{"receiptState", "navValue", "navTimestamp"}, oracle: []string{"oracle"}, prices: []string{"oraclePrice"}}, nil
	default:
		return suiSQLKinds{}, fmt.Errorf("unsupported Sui processor protocol")
	}
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
		// object, max(id) has exactly the checkpoint/version/terminal ordering.
		selection := `SELECT max(id) AS selectedId FROM "PortfolioObjectState_raw" WHERE ` + suiSQLIDRange(objectKinds, nil, upper)
		if kinds.indexed {
			// The object index orders one object's versions contiguously and its
			// skip index on objectId keeps a lookup to the granules of that object,
			// instead of a scan over every version of the kind.
			selection = `SELECT max(stateId) AS selectedId FROM "PortfolioObjectIndex_raw" WHERE ` + suiSQLKindPrefixRange(objectKinds) + ` AND checkpoint <= ` + upperCheckpoint
		}
		if ids != "" {
			selection += ` AND objectId IN ` + suiSQLSet(ids)
		}
		ctes = append(ctes, `(SELECT groupArray(selectedId) FROM (`+selection+` GROUP BY objectId)) AS `+name+`_ids`)
		// arrayJoin turns the cached array back into a set: only a set reaches the
		// primary key, a constant array is evaluated row by row over the table.
		ctes = append(ctes, name+` AS (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(`+name+`_ids)) AND `+visible+` ORDER BY id DESC,materializedAtCheckpoint DESC LIMIT 1 BY objectId)`)
		stageIDs = append(stageIDs, name+"_ids")
	}
	candidates := `SELECT groupUniqArray(objectId) FROM "PortfolioObjectState_raw" WHERE ` + suiSQLIDRange(kinds.roots, nil, upper) + ` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS') AND owner=` + sqlString(owner.Hex())
	if kinds.indexed {
		// The owner index orders an account's capabilities contiguously, so the
		// candidates are one primary-key prefix range per root kind.
		ranges := make([]string, 0, len(kinds.roots))
		for _, kind := range kinds.roots {
			ranges = append(ranges, `(id > `+sqlString(kind+":"+owner.Hex()+":")+` AND id <= `+sqlString(kind+":"+owner.Hex()+":~")+`)`)
		}
		candidates = `SELECT groupUniqArray(objectId) FROM "PortfolioOwnerIndex_raw" WHERE (` + strings.Join(ranges, ` OR `) + `) AND checkpoint <= ` + upperCheckpoint
	}
	ctes = append(ctes, `(`+candidates+`) AS root_candidates`)
	latest("root_states", "root_candidates", kinds.roots)
	ctes = append(ctes, `roots AS (SELECT * FROM root_states WHERE state='live' AND owner=`+sqlString(owner.Hex())+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS'))`)
	latest("direct_states", `(SELECT relatedId FROM roots WHERE relatedId != '')`, kinds.direct)
	if r.protocolID == "suilend" {
		// A Suilend capability points through its obligation to the market. The
		// other protocols' roots point directly to their pool or vault.
		latest("second_states", `(SELECT relatedId FROM direct_states WHERE state='live' AND relatedId != '')`, kinds.second)
	}
	childCandidates := ""
	valueSelect := `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM "PortfolioValue" WHERE 0`
	quoteSelect := `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM "PortfolioObjectState_raw" WHERE 0`
	switch r.protocolID {
	case "suilend":
	case "navi":
		// Protocol topology is small. Principal ownership is indexed separately
		// because a dynamic-field object itself is object-owned; key and parent
		// checks below remain authoritative for account and reserve membership.
		latest("topology", "", kinds.topology)
		ctes = append(ctes, `accounts AS (SELECT `+sqlString(owner.Hex())+` AS account UNION DISTINCT SELECT JSONExtractString(links,'accountAddress') FROM roots WHERE kind='account')`)
		ctes = append(ctes, `(SELECT groupUniqArray(objectId) FROM "PortfolioOwnerIndex_raw" WHERE `+suiSQLKindPrefixRange([]string{"principal"})+` AND owner IN (SELECT account FROM accounts) AND checkpoint <= `+upperCheckpoint+`) AS principal_candidates`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='principal' AND objectId IN ` + suiSQLSet("principal_candidates") + ` AND key IN (SELECT account FROM accounts) AND parentId IN (SELECT JSONExtractString(links,'supplyTableId') FROM topology WHERE kind='reserve' AND state='live' UNION DISTINCT SELECT JSONExtractString(links,'borrowTableId') FROM topology WHERE kind='reserve' AND state='live')) OR (kind='receiptState' AND key IN (SELECT objectId FROM roots WHERE kind='receipt') AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live' AND kind='vault')))`
		valueSelect = `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM (SELECT * FROM "PortfolioValue" WHERE ` + suiSQLIDRange([]string{"emode"}, nil, upper) + ` AND ` + visible + ` AND account IN (SELECT account FROM accounts) ORDER BY checkpoint DESC LIMIT 1 BY kind,account,market)`
	case "cetus", "bluefin":
		ctes = append(ctes, `position_ticks AS (SELECT JSONExtractString(links,'lowerTick') AS tick FROM roots UNION DISTINCT SELECT JSONExtractString(links,'upperTick') FROM roots)`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='accounting' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'positionTableId') FROM direct_states WHERE state='live')) OR (kind='tick' AND key IN (SELECT tick FROM position_ticks) AND parentId IN (SELECT JSONExtractString(links,'tickTableId') FROM direct_states WHERE state='live')))`
	case "volo-vaults":
		latest("oracle", "", kinds.oracle)
		latest("prices", "", kinds.prices)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='receiptState' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live')) OR (kind='navValue' AND parentId IN (SELECT JSONExtractString(links,'navValueTableId') FROM direct_states WHERE state='live')) OR (kind='navTimestamp' AND parentId IN (SELECT JSONExtractString(links,'navTimestampTableId') FROM direct_states WHERE state='live')))`
		quoteSelect = `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM (SELECT * FROM "PortfolioObjectState_raw" WHERE ` + visible + ` AND id IN (SELECT id FROM "PortfolioObjectState_raw" WHERE ` + suiSQLIDRange(kinds.prices, nil, upper) + ` AND transactionDigest IN (SELECT transactionDigest FROM child_states WHERE kind='navTimestamp' AND state='live')) ORDER BY checkpoint DESC,version DESC,id DESC,materializedAtCheckpoint DESC LIMIT 1 BY id)`
	default:
		return "", fmt.Errorf("unsupported Sui processor protocol")
	}
	if childCandidates != "" {
		childCandidates += ` AND ` + suiSQLIDRange(kinds.children, nil, upper)
		latest("child_states", `(`+childCandidates+`)`, kinds.children)
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
	allKinds := []string{}
	for _, group := range [][]string{kinds.roots, kinds.direct, kinds.second, kinds.topology, kinds.children, kinds.oracle, kinds.prices} {
		allKinds = append(allKinds, group...)
	}
	interval := `materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM candidate_snapshot)`
	lower := suiSQLBound{column: "previousCheckpoint", source: "candidate_snapshot"}
	snapshotOutput := `(SELECT *, (SELECT uniqExact(id) FROM "PortfolioObjectState_raw" WHERE ` + interval + ` AND ` + suiSQLIDRange(allKinds, &lower, upper) + `) AS observedObjectCount, (SELECT uniqExact(id) FROM "PortfolioValue" WHERE ` + interval + ` AND ` + suiSQLIDRange([]string{"emode"}, &lower, upper) + `) AS observedValueCount FROM snapshot)`

	branches := []string{
		`SELECT 'snapshot' AS rowType,` + sqlPayload(snapshotFields...) + ` AS payload FROM ` + snapshotOutput,
		`SELECT 'object' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM objects`,
		`SELECT 'metadata' AS rowType,` + sqlPayload("coinType", "decimals", "symbol", "name", "checkpoint", "materializedAtCheckpoint", "status") + ` AS payload FROM metadata`, valueSelect, quoteSelect,
	}
	return "WITH " + strings.Join(ctes, ",\n") + "\n" + strings.Join(branches, "\nUNION ALL\n"), nil
}
func suiSQLObjectPayload() string {
	return sqlPayload("id", "objectId", "kind", "version", "digest", "state", "ownerKind", "owner", "objectType", "content", "checkpoint", "timestampMs", "transactionDigest", "materializedAtCheckpoint", "parentId", "key", "relatedId", "links")
}
