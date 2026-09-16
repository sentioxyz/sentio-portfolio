package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

const suiDailyIntervalMs = uint64(24 * time.Hour / time.Millisecond)

type suiDailySnapshot struct {
	ID, Checkpoint, TimestampMs, Digest, SchemaVersion, StartTimestampMs  string
	AccountCount, ErrorCount, ObservedCount, VariantCount, ObservedErrors string
	NextCheckpoint                                                        string
}

// A complete-day certificate and event visibility are checked in one statement.
// Replayed identical events deduplicate; conflicting versions fail closed.
func (r *suiHistoryIndex) dailyPortfolioSQL(owner SuiAddress, selection suiSQLSelection) (string, error) {
	where := ""
	if selection.at != nil {
		if selection.at.UnixMilli() < 0 {
			return "", fmt.Errorf("invalid Sui history timestamp")
		}
		where = " AND timestampMs <= " + strconv.FormatInt(selection.at.UnixMilli(), 10)
	}
	if selection.checkpoint != nil {
		where = " AND checkpoint <= " + strconv.FormatUint(*selection.checkpoint, 10)
	}
	return `WITH completed AS (SELECT * FROM "DailyPortfolioSnapshot" WHERE schemaVersion=3 AND id=leftPad(toString(checkpoint),20,'0')),
snapshot AS (SELECT * FROM completed WHERE 1` + where + ` ORDER BY checkpoint DESC LIMIT 1),
events AS (SELECT DISTINCT account,portfolio FROM "Portfolio" WHERE timestampMs=(SELECT timestampMs FROM snapshot)),
certificate AS (SELECT *, (SELECT uniqExact(account) FROM events) AS observedCount,
(SELECT count() FROM events) AS variantCount,
(SELECT countIf(JSONLength(portfolio,'errors') > 0) FROM events) AS observedErrors,
(SELECT minOrNull(checkpoint) FROM completed WHERE checkpoint > (SELECT checkpoint FROM snapshot)) AS nextCheckpoint FROM snapshot)
SELECT 'snapshot' AS rowType,` + sqlPayload("id", "checkpoint", "timestampMs", "digest", "schemaVersion", "startTimestampMs", "accountCount", "errorCount", "observedCount", "variantCount", "observedErrors", "nextCheckpoint") + ` AS payload FROM certificate
UNION ALL SELECT 'event' AS rowType, portfolio AS payload FROM events WHERE account=` + sqlString(owner.Hex()), nil
}

func (r *suiHistoryIndex) readDailyPortfolio(ctx context.Context, owner SuiAddress, selection suiSQLSelection) (SuiProtocolPositions, error) {
	result := r.result(SuiCheckpoint{})
	query, err := r.dailyPortfolioSQL(owner, selection)
	if err != nil {
		return result, err
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
	if err := r.request(ctx, http.MethodPost, r.config.SQLURL, map[string]any{"version": version, "sqlQuery": map[string]any{"sql": query, "size": suiSQLRowLimit}, "sync_v1": true}, &response); err != nil {
		return result, err
	}
	nonempty := func(raw json.RawMessage) bool {
		return len(raw) > 0 && string(raw) != "null" && string(raw) != "\"\"" && string(raw) != "{}"
	}
	if response.Result == nil || nonempty(response.Error) || len(response.Errors) > 0 || nonempty(response.Cursor) || nonempty(response.Result.Cursor) || nonempty(response.Result.NextCursor) || response.Result.Truncated || response.Result.HasMore || len(response.Result.Rows) > 2 {
		return result, fmt.Errorf("daily Sui query failed or is incomplete")
	}
	var snapshot suiDailySnapshot
	var event *SuiPortfolioEvent
	snapshots := 0
	for _, row := range response.Result.Rows {
		switch row.RowType {
		case "snapshot":
			snapshots++
			if json.Unmarshal([]byte(row.Payload), &snapshot) != nil {
				return result, fmt.Errorf("invalid daily Sui certificate")
			}
		case "event":
			if event != nil {
				return result, fmt.Errorf("conflicting daily Sui events")
			}
			event = &SuiPortfolioEvent{}
			if json.Unmarshal([]byte(row.Payload), event) != nil {
				return result, fmt.Errorf("invalid daily Sui event")
			}
		default:
			return result, fmt.Errorf("unexpected daily Sui row")
		}
	}
	if snapshots != 1 {
		return result, fmt.Errorf("completed daily Sui snapshot is unavailable")
	}
	pin, err := indexedSuiCheckpoint(snapshot.Checkpoint, snapshot.TimestampMs, snapshot.Digest, 0)
	if err != nil || snapshot.ID != fmt.Sprintf("%020d", pin.Sequence) || snapshot.SchemaVersion != "3" {
		return result, fmt.Errorf("invalid daily Sui checkpoint")
	}
	start, e := historyUint(snapshot.StartTimestampMs)
	count, e2 := historyUint(snapshot.AccountCount)
	errors, e3 := historyUint(snapshot.ErrorCount)
	if e != nil || e2 != nil || e3 != nil || start == 0 || start > uint64(pin.Timestamp.UnixMilli()) || errors > count || snapshot.ObservedCount != snapshot.AccountCount || snapshot.VariantCount != snapshot.AccountCount || snapshot.ObservedErrors != snapshot.ErrorCount {
		return result, fmt.Errorf("daily Sui events are incomplete or conflicting")
	}
	if selection.at != nil {
		ms := selection.at.UnixMilli()
		if ms < pin.Timestamp.UnixMilli() || uint64(ms) < start || uint64(ms-pin.Timestamp.UnixMilli()) >= suiDailyIntervalMs {
			return result, fmt.Errorf("requested time is outside the daily Sui sample")
		}
	}
	if selection.checkpoint != nil {
		next, _ := historyUint(snapshot.NextCheckpoint)
		if *selection.checkpoint < pin.Sequence || (*selection.checkpoint != pin.Sequence && (next <= pin.Sequence || *selection.checkpoint >= next)) {
			return result, fmt.Errorf("requested checkpoint is outside the completed daily Sui interval")
		}
	}
	result = r.result(pin)
	result.Groups = []SuiProtocolGroup{}
	if event == nil {
		return result, nil
	} // Certified absent at this sample.
	if event.SchemaVersion != 3 || event.ProtocolID != r.protocolID || event.Account != owner.Hex() || event.Checkpoint != snapshot.Checkpoint || event.TimestampMs != snapshot.TimestampMs || event.Digest != snapshot.Digest || event.Positions == nil || event.Errors == nil {
		return result, fmt.Errorf("daily Sui event identity mismatch")
	}
	if len(event.Errors) > 0 {
		return result, fmt.Errorf("daily Sui account calculation failed")
	}
	groups := map[string]bool{}
	for _, position := range event.Positions {
		if position.ID == "" || groups[position.ID] || position.Components == nil {
			return r.result(pin), fmt.Errorf("invalid daily Sui position")
		}
		groups[position.ID] = true
		g := SuiProtocolGroup{ID: position.ID, MarketID: position.MarketID, Label: position.Label, Metadata: position.Metadata}
		if g.Metadata == nil {
			g.Metadata = map[string]any{}
		}
		g.Metadata["sampleIntervalSeconds"] = 86400
		g.Metadata["stateMode"] = "indexed"
		if selection.at != nil || selection.checkpoint != nil {
			g.Metadata["stateMode"] = "historical"
		}
		if selection.at != nil {
			g.Metadata["requestedTimestamp"] = selection.at.UTC().Format(time.RFC3339Nano)
		}
		if selection.checkpoint != nil {
			g.Metadata["requestedCheckpoint"] = strconv.FormatUint(*selection.checkpoint, 10)
		}
		for _, component := range position.Components {
			coin, err := NormalizeMoveType(component.Asset.Address)
			if err != nil || coin != component.Asset.Address || component.Asset.SentioChainID != "sui_mainnet" || (component.Kind != "asset" && component.Kind != "debt" && component.Kind != "reward") {
				return r.result(pin), fmt.Errorf("invalid daily Sui component identity")
			}
			amount, ok := new(big.Int).SetString(component.AmountRaw, 10)
			denominator, ok2 := new(big.Int).SetString(component.AmountDenominatorRaw, 10)
			if !ok || !ok2 || amount.Sign() < 0 || denominator.Sign() <= 0 || amount.String() != component.AmountRaw || denominator.String() != component.AmountDenominatorRaw {
				return r.result(pin), fmt.Errorf("invalid daily Sui quantity")
			}
			decimals := int(component.Decimals)
			meta, err := suiCoinMetadataFrom(coin, &decimals, &component.Symbol, component.Name)
			if err != nil {
				return r.result(pin), err
			}
			g.Components = append(g.Components, SuiProtocolComponent{Kind: component.Kind, Coin: meta, AmountRaw: component.AmountRaw, AmountDenominatorRaw: component.AmountDenominatorRaw, Metadata: component.Metadata})
		}
		result.Groups = append(result.Groups, g)
	}
	return result, nil
}
