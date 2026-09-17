package portfolio

import (
	"fmt"
	"strconv"
	"strings"
)

const suiPortfolioSchemaVersion = "2"

type suiSQLKinds struct {
	roots, direct, second, topology, children, oracle, prices []string
}

func suiSQLProtocolKinds(protocol string) (suiSQLKinds, error) {
	switch protocol {
	case "suilend":
		return suiSQLKinds{roots: []string{"cap"}, direct: []string{"obligation"}, second: []string{"market"}}, nil
	case "navi":
		return suiSQLKinds{roots: []string{"account", "receipt"}, direct: []string{"vault"}, topology: []string{"storage", "market", "reserve"}, children: []string{"principal", "receiptState"}}, nil
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

// Version 2 IDs sort by kind, source checkpoint, object, version and terminal
// suffix. These ranges let the entity primary key prune source-checkpoint
// intervals without depending on secondary indexes or numeric view conversions.
func suiSQLIDRange(kinds []string, lower, upper string) string {
	ranges := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		end := func(cp string) string {
			return `concat(` + sqlString(kind+":") + `,leftPad(toString(` + cp + `),20,'0'),':~')`
		}
		start := `id >= ` + sqlString(kind+":")
		if lower != "" {
			start = `id > ` + end(lower)
		}
		ranges = append(ranges, `(`+start+` AND id <= `+end(upper)+`)`)
	}
	return `(` + strings.Join(ranges, ` OR `) + `)`
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
	upper := `(SELECT checkpoint FROM snapshot)`
	// Each selection stage keeps its chosen IDs in a scalar array alias. A
	// scalar subquery is evaluated once per statement, while a CTE is expanded
	// again at every reference, so a chain of CTEs rescanned the table once per
	// dependency level and per output branch. An empty stage yields an empty array.
	latest := func(name, ids string, objectKinds []string) {
		predicate := suiSQLIDRange(objectKinds, "", upper)
		if ids != "" {
			predicate += ` AND objectId IN ` + ids
		}
		// Compare only the immutable IDs before fetching JSON content. For one
		// object, max(id) has exactly the checkpoint/version/terminal ordering.
		ctes = append(ctes, `(SELECT groupArray(selectedId) FROM (SELECT max(id) AS selectedId FROM "PortfolioObjectState_raw" WHERE `+predicate+` GROUP BY objectId)) AS `+name+`_ids`)
		// arrayJoin turns the cached array back into a set: only a set reaches the
		// primary key, a constant array is evaluated row by row over the table.
		ctes = append(ctes, name+` AS (SELECT * FROM "PortfolioObjectState_raw" WHERE id IN (SELECT arrayJoin(`+name+`_ids)) AND `+visible+` ORDER BY id DESC,materializedAtCheckpoint DESC LIMIT 1 BY objectId)`)
	}
	ctes = append(ctes, `(SELECT groupUniqArray(objectId) FROM "PortfolioObjectState_raw" WHERE `+suiSQLIDRange(kinds.roots, "", upper)+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS') AND owner=`+sqlString(owner.Hex())+`) AS root_candidates`)
	latest("root_states", "root_candidates", kinds.roots)
	ctes = append(ctes, `roots AS (SELECT * FROM root_states WHERE state='live' AND owner=`+sqlString(owner.Hex())+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS'))`)
	latest("direct_states", `(SELECT relatedId FROM roots WHERE relatedId != '')`, kinds.direct)
	selected := `SELECT * FROM root_states UNION ALL SELECT * FROM direct_states`
	if r.protocolID == "suilend" {
		// A Suilend capability points through its obligation to the market. The
		// other protocols' roots point directly to their pool or vault.
		latest("second_states", `(SELECT relatedId FROM direct_states WHERE state='live' AND relatedId != '')`, kinds.second)
		selected += ` UNION ALL SELECT * FROM second_states`
	}
	childCandidates := ""
	valueSelect := `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM "PortfolioValue" WHERE 0`
	quoteSelect := `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM "PortfolioObjectState_raw" WHERE 0`
	switch r.protocolID {
	case "suilend":
	case "navi":
		// Protocol topology is small; only address-matched principal rows are read.
		latest("topology", "", kinds.topology)
		ctes = append(ctes, `accounts AS (SELECT `+sqlString(owner.Hex())+` AS account UNION DISTINCT SELECT JSONExtractString(links,'accountAddress') FROM roots WHERE kind='account')`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='principal' AND key IN (SELECT account FROM accounts) AND parentId IN (SELECT JSONExtractString(links,'supplyTableId') FROM topology WHERE kind='reserve' AND state='live' UNION DISTINCT SELECT JSONExtractString(links,'borrowTableId') FROM topology WHERE kind='reserve' AND state='live')) OR (kind='receiptState' AND key IN (SELECT objectId FROM roots WHERE kind='receipt') AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live' AND kind='vault')))`
		selected += ` UNION ALL SELECT * FROM topology`
		valueSelect = `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM (SELECT * FROM "PortfolioValue" WHERE ` + suiSQLIDRange([]string{"emode"}, "", upper) + ` AND ` + visible + ` AND account IN (SELECT account FROM accounts) ORDER BY checkpoint DESC LIMIT 1 BY kind,account,market)`
	case "cetus", "bluefin":
		ctes = append(ctes, `position_ticks AS (SELECT JSONExtractString(links,'lowerTick') AS tick FROM roots UNION DISTINCT SELECT JSONExtractString(links,'upperTick') FROM roots)`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='accounting' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'positionTableId') FROM direct_states WHERE state='live')) OR (kind='tick' AND key IN (SELECT tick FROM position_ticks) AND parentId IN (SELECT JSONExtractString(links,'tickTableId') FROM direct_states WHERE state='live')))`
	case "volo-vaults":
		latest("oracle", "", kinds.oracle)
		latest("prices", "", kinds.prices)
		selected += ` UNION ALL SELECT * FROM oracle UNION ALL SELECT * FROM prices`
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ((kind='receiptState' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live')) OR (kind='navValue' AND parentId IN (SELECT JSONExtractString(links,'navValueTableId') FROM direct_states WHERE state='live')) OR (kind='navTimestamp' AND parentId IN (SELECT JSONExtractString(links,'navTimestampTableId') FROM direct_states WHERE state='live')))`
		quoteSelect = `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM (SELECT * FROM "PortfolioObjectState_raw" WHERE ` + visible + ` AND id IN (SELECT id FROM "PortfolioObjectState_raw" WHERE ` + suiSQLIDRange(kinds.prices, "", upper) + ` AND transactionDigest IN (SELECT transactionDigest FROM child_states WHERE kind='navTimestamp' AND state='live')) ORDER BY checkpoint DESC,version DESC,id DESC,materializedAtCheckpoint DESC LIMIT 1 BY id)`
	default:
		return "", fmt.Errorf("unsupported Sui processor protocol")
	}
	if childCandidates != "" {
		childCandidates += ` AND ` + suiSQLIDRange(kinds.children, "", upper)
		latest("child_states", `(`+childCandidates+`)`, kinds.children)
		selected += ` UNION ALL SELECT * FROM child_states`
	}
	ctes = append(ctes, `selected_states AS (`+selected+`)`)
	// Identical IDs may be reached through multiple owned capabilities.
	ctes = append(ctes, `objects AS (SELECT * FROM selected_states ORDER BY checkpoint DESC,version DESC,id DESC LIMIT 1 BY objectId)`)
	ctes = append(ctes, `coin_types AS (SELECT arrayJoin(JSONExtract(links,'coinTypes','Array(String)')) AS coinType FROM objects WHERE state='live')`)
	ctes = append(ctes, `metadata AS (SELECT * FROM "PortfolioTokenMetadata" WHERE checkpoint <= (SELECT materializedAtCheckpoint FROM snapshot) AND materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM snapshot) AND coinType IN (SELECT coinType FROM coin_types) ORDER BY checkpoint DESC LIMIT 1 BY coinType)`)
	snapshotFields := []string{"id", "checkpoint", "timestampMs", "digest", "schemaVersion", "startCheckpoint", "materializedAtCheckpoint", "nextCheckpoint", "nextTimestampMs", "previousCheckpoint", "objectCount", "valueCount", "observedObjectCount", "observedValueCount"}
	// Counts are computed only in this output branch. Putting these scans into
	// snapshot's WHERE makes every dependency reuse expand the global scans again.
	allKinds := []string{}
	for _, group := range [][]string{kinds.roots, kinds.direct, kinds.second, kinds.topology, kinds.children, kinds.oracle, kinds.prices} {
		allKinds = append(allKinds, group...)
	}
	interval := `materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM candidate_snapshot)`
	lower := `(SELECT previousCheckpoint FROM candidate_snapshot)`
	snapshotOutput := `(SELECT *, (SELECT uniqExact(id) FROM "PortfolioObjectState_raw" WHERE ` + interval + ` AND ` + suiSQLIDRange(allKinds, lower, upper) + `) AS observedObjectCount, (SELECT uniqExact(id) FROM "PortfolioValue" WHERE ` + interval + ` AND ` + suiSQLIDRange([]string{"emode"}, lower, upper) + `) AS observedValueCount FROM snapshot)`

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
