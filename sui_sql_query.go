package portfolio

import (
	"fmt"
	"strconv"
	"strings"
)

// portfolioSQL resolves candidate identities before selecting latest lifecycle.
// Filtering owner, parent or key before that selection would resurrect an object
// whose final version was transferred, wrapped or deleted. Object-state IDs are
// immutable versions, so the raw entity history supports bounded scans without
// a redundant global latest-entity aggregation. Lifecycle and quote selections
// collapse deterministic replay rows by object identity and version ID.
func (r *suiHistoryIndex) portfolioSQL(owner SuiAddress, selection suiSQLSelection) (string, error) {
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
		`candidate_snapshot AS (SELECT * FROM "PortfolioSnapshot" WHERE schemaVersion=1` + where + ` ORDER BY checkpoint DESC LIMIT 1)`,
		`snapshot AS (SELECT * FROM candidate_snapshot)`,
	}
	sourceBound := `checkpoint <= (SELECT checkpoint FROM snapshot) AND materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM snapshot)`
	latest := func(name, ids string) {
		ctes = append(ctes, name+` AS (SELECT * FROM "PortfolioObjectState_raw" WHERE `+sourceBound+` AND objectId IN (`+ids+`) ORDER BY checkpoint DESC,version DESC,id DESC LIMIT 1 BY objectId)`)
	}
	candidates := `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ` + sourceBound + ` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS') AND owner=` + sqlString(owner.Hex())
	latest("root_states", candidates)
	ctes = append(ctes, `roots AS (SELECT * FROM root_states WHERE state='live' AND owner=`+sqlString(owner.Hex())+` AND ownerKind IN ('ADDRESS','CONSENSUS_ADDRESS'))`)
	latest("direct_states", `SELECT relatedId FROM roots WHERE relatedId != ''`)
	selected := `SELECT * FROM root_states UNION ALL SELECT * FROM direct_states`
	if r.protocolID == "suilend" {
		// A Suilend capability points through its obligation to the market. The
		// other protocols' roots point directly to their pool or vault.
		latest("second_states", `SELECT relatedId FROM direct_states WHERE state='live' AND relatedId != ''`)
		selected += ` UNION ALL SELECT * FROM second_states`
	}
	childCandidates := ""
	valueSelect := `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM "PortfolioValue" WHERE 0`
	quoteSelect := `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM "PortfolioObjectState_raw" WHERE 0`
	switch r.protocolID {
	case "suilend":
	case "navi":
		// Protocol topology is small; only address-matched principal rows are read.
		latest("topology", `SELECT objectId FROM "PortfolioObjectState_raw" WHERE kind IN ('storage','market','reserve') AND `+sourceBound)
		ctes = append(ctes, `accounts AS (SELECT `+sqlString(owner.Hex())+` AS account UNION DISTINCT SELECT JSONExtractString(links,'accountAddress') FROM roots WHERE kind='account')`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ` + sourceBound + ` AND ((kind='principal' AND key IN (SELECT account FROM accounts) AND parentId IN (SELECT JSONExtractString(links,'supplyTableId') FROM topology WHERE kind='reserve' AND state='live' UNION DISTINCT SELECT JSONExtractString(links,'borrowTableId') FROM topology WHERE kind='reserve' AND state='live')) OR (kind='receiptState' AND key IN (SELECT objectId FROM roots WHERE kind='receipt') AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live' AND kind='vault')))`
		selected += ` UNION ALL SELECT * FROM topology`
		valueSelect = `SELECT 'value' AS rowType,` + sqlPayload("id", "kind", "account", "market", "content", "checkpoint", "materializedAtCheckpoint") + ` AS payload FROM (SELECT * FROM "PortfolioValue" WHERE ` + sourceBound + ` AND account IN (SELECT account FROM accounts) ORDER BY checkpoint DESC LIMIT 1 BY kind,account,market)`
	case "cetus", "bluefin":
		ctes = append(ctes, `position_ticks AS (SELECT JSONExtractString(links,'lowerTick') AS tick FROM roots UNION DISTINCT SELECT JSONExtractString(links,'upperTick') FROM roots)`)
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ` + sourceBound + ` AND ((kind='accounting' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'positionTableId') FROM direct_states WHERE state='live')) OR (kind='tick' AND key IN (SELECT tick FROM position_ticks) AND parentId IN (SELECT JSONExtractString(links,'tickTableId') FROM direct_states WHERE state='live')))`
	case "volo-vaults":
		latest("oracle", `SELECT objectId FROM "PortfolioObjectState_raw" WHERE kind='oracle' AND `+sourceBound)
		latest("prices", `SELECT objectId FROM "PortfolioObjectState_raw" WHERE kind='oraclePrice' AND `+sourceBound)
		selected += ` UNION ALL SELECT * FROM oracle UNION ALL SELECT * FROM prices`
		childCandidates = `SELECT objectId FROM "PortfolioObjectState_raw" WHERE ` + sourceBound + ` AND ((kind='receiptState' AND key IN (SELECT objectId FROM roots) AND parentId IN (SELECT JSONExtractString(links,'usersTableId') FROM direct_states WHERE state='live')) OR (kind='navValue' AND parentId IN (SELECT JSONExtractString(links,'navValueTableId') FROM direct_states WHERE state='live')) OR (kind='navTimestamp' AND parentId IN (SELECT JSONExtractString(links,'navTimestampTableId') FROM direct_states WHERE state='live')))`
		quoteSelect = `SELECT 'quote' AS rowType,` + suiSQLObjectPayload() + ` AS payload FROM (SELECT * FROM "PortfolioObjectState_raw" WHERE kind='oraclePrice' AND ` + sourceBound + ` AND transactionDigest IN (SELECT transactionDigest FROM child_states WHERE kind='navTimestamp' AND state='live') ORDER BY checkpoint DESC,version DESC,id DESC LIMIT 1 BY id)`
	default:
		return "", fmt.Errorf("unsupported Sui processor protocol")
	}
	if childCandidates != "" {
		latest("child_states", childCandidates)
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
	snapshotOutput := `(SELECT *, (SELECT uniqExact(id) FROM "PortfolioObjectState_raw" WHERE checkpoint > (SELECT previousCheckpoint FROM candidate_snapshot) AND checkpoint <= (SELECT checkpoint FROM candidate_snapshot) AND materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM candidate_snapshot)) AS observedObjectCount, (SELECT uniqExact(id) FROM "PortfolioValue" WHERE checkpoint > (SELECT previousCheckpoint FROM candidate_snapshot) AND checkpoint <= (SELECT checkpoint FROM candidate_snapshot) AND materializedAtCheckpoint <= (SELECT materializedAtCheckpoint FROM candidate_snapshot)) AS observedValueCount FROM snapshot)`

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
