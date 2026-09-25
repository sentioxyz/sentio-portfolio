package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// suiHistoryIndex is the read side of a version-pinned Sui processor index: one
// SQL statement per address, answered from completed samples only. It never
// reaches a Sui node, so a protocol served this way has no node fallback.
type suiHistoryIndex struct {
	protocolID, name string
	start            uint64
	api              *sentioAPIClient
	config           SentioIndexerConfig
	sql              sentioSQLEndpoints
	engine           *Engine
}

func newSuiHistoryIndex(config SentioIndexerConfig, protocolID, name string, start uint64) (*suiHistoryIndex, error) {
	version, err := strconv.ParseUint(config.ProcessorVersion, 10, 64)
	if err != nil || version == 0 || strconv.FormatUint(version, 10) != config.ProcessorVersion {
		return nil, fmt.Errorf("Sui history requires an explicit processor version")
	}
	endpoint, err := url.Parse(config.SQLURL)
	if config.SQLURL == "" {
		endpoint, err = url.Parse(config.GraphQLURL)
		if err == nil {
			if strings.Contains(endpoint.Path, "/graphql/") {
				endpoint.Path = strings.Replace(endpoint.Path, "/graphql/", "/analytics/", 1) + "/sql/execute"
			} else {
				endpoint.Path = strings.TrimSuffix(endpoint.Path, "/graphql") + "/sql/execute"
			}
			endpoint.RawQuery = ""
		}
	}
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, fmt.Errorf("invalid Sui SQL endpoint")
	}
	if version := endpoint.Query().Get("version"); version != "" && version != config.ProcessorVersion {
		return nil, fmt.Errorf("Sui SQL endpoint version mismatch")
	}
	config.SQLURL = endpoint.String()
	sql, err := newSentioSQLEndpoints(config.SQLURL)
	if err != nil {
		return nil, fmt.Errorf("invalid Sui SQL endpoint")
	}
	return &suiHistoryIndex{api: newSentioAPIClient(), config: config, sql: sql, protocolID: protocolID, name: name, start: start}, nil
}

func (r *suiHistoryIndex) WithEngine(engine *Engine) *suiHistoryIndex {
	copy := *r
	copy.engine = engine
	return &copy
}

// statement runs one SQL statement against the pinned version, holding a slot of the engine's
// indexer lane from submission until it finishes.
func (r *suiHistoryIndex) statement(ctx context.Context, query string, rowLimit int) (json.RawMessage, error) {
	if r.engine != nil {
		scope := scopeFrom(ctx)
		scope.lane, scope.observer, scope.protocolID = r.engine.indexerLane, r.engine.observer, r.protocolID
		ctx = context.WithValue(ctx, scanScopeKey{}, scope)
	}
	if err := lockSentioLane(ctx); err != nil {
		return nil, err
	}
	defer unlockSentioLane(ctx)
	version, _ := strconv.ParseUint(r.config.ProcessorVersion, 10, 64)
	return r.api.executeSQL(ctx, r.sql, version, query, rowLimit)
}

func indexedSuiCheckpoint(sequence, timestamp, digest string, start uint64) (SuiCheckpoint, error) {
	n, err := historyUint(sequence)
	if err != nil || n < start {
		return SuiCheckpoint{}, fmt.Errorf("invalid Sui history checkpoint")
	}
	ms, err := historyUint(timestamp)
	if err != nil || ms == 0 || ms > 253402300799999 {
		return SuiCheckpoint{}, fmt.Errorf("invalid Sui history timestamp")
	}
	return newSuiCheckpoint(n, digest, time.UnixMilli(int64(ms)))
}

// ReadLatest returns the newest completed hourly sample certified by a later
// index watermark. Its checkpoint describes that sample, not the current head.
// Hosts may enforce a wall-clock freshness bound before serving the result.
func (r *suiHistoryIndex) ReadLatest(ctx context.Context, owner SuiAddress) (SuiProtocolPositions, error) {
	return r.readSQLPortfolio(ctx, owner, suiSQLSelection{})
}

func (r *suiHistoryIndex) ReadAtTime(ctx context.Context, owner SuiAddress, at time.Time) (SuiProtocolPositions, error) {
	return r.readSQLPortfolio(ctx, owner, suiSQLSelection{at: &at})
}

func (r *suiHistoryIndex) ReadAtCheckpoint(ctx context.Context, owner SuiAddress, sequence uint64) (SuiProtocolPositions, error) {
	return r.readSQLPortfolio(ctx, owner, suiSQLSelection{checkpoint: &sequence})
}

func historyUint(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return 0, fmt.Errorf("invalid historical unsigned integer")
	}
	return n, nil
}

func (r *suiHistoryIndex) result(pin SuiCheckpoint) SuiProtocolPositions {
	return SuiProtocolPositions{ProtocolID: r.protocolID, ProtocolName: r.name, Checkpoint: pin, HeadBeforeRead: pin.Sequence}
}
