package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const suiHistoryPageSize = 250
const suiHistoryMaxRows = 4096

// SuiProtocolReader reads checkpoint-scoped protocol state. Wallet holdings
// remain a separate head-only read; protocol history never consults that head.
type SuiProtocolReader struct {
	api     *sentioAPIClient
	configs map[string]SentioIndexerConfig
	lane    *indexerLane
}

func NewSuiProtocolReader(config EngineConfig) *SuiProtocolReader {
	return newSuiProtocolReader(config, newIndexerLane(config.IndexerConcurrency))
}

// SuiProtocolReader shares admission with this engine's EVM adapters.
func (e *Engine) SuiProtocolReader(config EngineConfig) *SuiProtocolReader {
	return newSuiProtocolReader(config, e.indexerLane)
}

func newSuiProtocolReader(config EngineConfig, lane *indexerLane) *SuiProtocolReader {
	configs := make(map[string]SentioIndexerConfig)
	for _, id := range []string{"navi", "volo-vaults"} {
		if value, ok := config.SentioIndexers[id]; ok {
			configs[id] = value
		}
	}
	return &SuiProtocolReader{api: newSentioAPIClient(), configs: configs, lane: lane}
}

func (r *SuiProtocolReader) ProtocolIDs() []string {
	ids := []string{}
	for _, id := range []string{"navi", "volo-vaults"} {
		if _, ok := r.configs[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

type SuiProtocolComponent struct {
	Kind                 string
	Coin                 SuiCoinMetadata
	AmountRaw            string
	AmountDenominatorRaw string
	Metadata             map[string]any
}

type SuiProtocolGroup struct {
	ID         string
	MarketID   string
	Label      string
	Components []SuiProtocolComponent
	Metadata   map[string]any
}

type SuiProtocolPositions struct {
	ProtocolID   string
	ProtocolName string
	Checkpoint   SuiCheckpoint
	Groups       []SuiProtocolGroup
	Errors       []error
}

type suiHistoryObject struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Owner      string `json:"owner"`
	Parent     string `json:"parent"`
	Key        string `json:"key"`
	ObjectType string `json:"objectType"`
	Content    string `json:"content"`
	Version    string `json:"version"`
	Checkpoint string `json:"checkpoint"`
}

type suiHistoryValue struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Account    string `json:"account"`
	Market     string `json:"market"`
	Content    string `json:"content"`
	Checkpoint string `json:"checkpoint"`
}

type suiHistoryPayload struct {
	Data struct {
		Objects     []suiHistoryObject `json:"suiHistoryObjects"`
		Values      []suiHistoryValue  `json:"suiHistoryValues"`
		Checkpoints []struct {
			ID          string `json:"id"`
			BlockNumber string `json:"blockNumber"`
			TimestampMS string `json:"timestampMs"`
			Digest      string `json:"digest"`
		} `json:"indexerCheckpoints"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (r *SuiProtocolReader) query(ctx context.Context, protocolID, query string, variables map[string]any) (suiHistoryPayload, error) {
	var payload suiHistoryPayload
	config, ok := r.configs[protocolID]
	if !ok {
		return payload, fmt.Errorf("%s history index is not configured", protocolID)
	}
	if err := config.validate(); err != nil {
		return payload, err
	}
	if err := r.lane.acquire(ctx); err != nil {
		return payload, err
	}
	defer r.lane.release()
	err := r.api.doJSON(ctx, http.MethodPost, config.GraphQLURL, map[string]any{"query": query, "variables": variables}, &payload)
	if err != nil {
		return payload, err
	}
	if len(payload.Errors) != 0 {
		return payload, fmt.Errorf("protocol history query failed: %s", PublicError(fmt.Errorf("%s", payload.Errors[0].Message)))
	}
	return payload, nil
}

// LatestCheckpoint returns a durable coverage watermark, not the latest user
// interaction. Callers verify its digest with SuiReader before serving it.
func (r *SuiProtocolReader) LatestCheckpoint(ctx context.Context, protocolID string) (SuiCheckpoint, error) {
	p, err := r.query(ctx, protocolID, `query { indexerCheckpoints(first: 2, where: { id: "sui_mainnet" }) { id blockNumber timestampMs digest } }`, nil)
	if err != nil {
		return SuiCheckpoint{}, err
	}
	if len(p.Data.Checkpoints) != 1 {
		return SuiCheckpoint{}, fmt.Errorf("%s history has no coverage watermark", protocolID)
	}
	c := p.Data.Checkpoints[0]
	sequence, err := strconv.ParseUint(c.BlockNumber, 10, 64)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("invalid history checkpoint sequence")
	}
	ms, err := strconv.ParseInt(c.TimestampMS, 10, 64)
	if err != nil || ms <= 0 || c.ID != "sui_mainnet" {
		return SuiCheckpoint{}, fmt.Errorf("invalid history checkpoint timestamp")
	}
	return newSuiCheckpoint(sequence, c.Digest, time.UnixMilli(ms))
}

func (r *SuiProtocolReader) objects(ctx context.Context, protocolID string, checkpoint uint64, where string) ([]suiHistoryObject, error) {
	rows := make([]suiHistoryObject, 0)
	after := ""
	for {
		p, err := r.query(ctx, protocolID, `query($block: BigInt!, $after: ID!, $first: Int!) {
  suiHistoryObjects(first: $first, orderBy: id, orderDirection: asc, block: {number: $block},
    where: {id_gt: $after, `+where+`}) { id kind owner parent key objectType content version checkpoint }
}`, map[string]any{"block": strconv.FormatUint(checkpoint, 10), "after": after, "first": suiHistoryPageSize})
		if err != nil {
			return nil, err
		}
		for _, row := range p.Data.Objects {
			id, err := ParseSuiAddress(row.ID)
			if err != nil || id.Hex() != row.ID || row.ID <= after {
				return nil, fmt.Errorf("invalid or unordered historical object row")
			}
			if err := historyRowAt(row.Checkpoint, checkpoint); err != nil {
				return nil, err
			}
			if _, err := strconv.ParseUint(row.Version, 10, 64); err != nil {
				return nil, fmt.Errorf("invalid historical object version")
			}
			after = row.ID
			rows = append(rows, row)
		}
		if len(rows) > suiHistoryMaxRows {
			return nil, fmt.Errorf("historical object enumeration exceeds %d rows", suiHistoryMaxRows)
		}
		if len(p.Data.Objects) < suiHistoryPageSize {
			return rows, nil
		}
	}
}

func (r *SuiProtocolReader) values(ctx context.Context, protocolID string, checkpoint uint64, where string) ([]suiHistoryValue, error) {
	rows := make([]suiHistoryValue, 0)
	after := ""
	for {
		p, err := r.query(ctx, protocolID, `query($block: BigInt!, $after: ID!, $first: Int!) {
  suiHistoryValues(first: $first, orderBy: id, orderDirection: asc, block: {number: $block},
    where: {id_gt: $after, `+where+`}) { id kind account market content checkpoint }
}`, map[string]any{"block": strconv.FormatUint(checkpoint, 10), "after": after, "first": suiHistoryPageSize})
		if err != nil {
			return nil, err
		}
		for _, row := range p.Data.Values {
			if row.ID <= after {
				return nil, fmt.Errorf("unordered historical value row")
			}
			if err := historyRowAt(row.Checkpoint, checkpoint); err != nil {
				return nil, err
			}
			after = row.ID
			rows = append(rows, row)
		}
		if len(rows) > suiHistoryMaxRows {
			return nil, fmt.Errorf("historical value enumeration exceeds %d rows", suiHistoryMaxRows)
		}
		if len(p.Data.Values) < suiHistoryPageSize {
			return rows, nil
		}
	}
}

func historyRowAt(raw string, checkpoint uint64) error {
	actual, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || actual > checkpoint {
		return fmt.Errorf("index returned a row beyond the requested checkpoint")
	}
	return nil
}

func quotedStrings(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ",") + "]"
}
