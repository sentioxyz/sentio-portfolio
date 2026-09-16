package portfolio

import (
	"context"
	"encoding/binary"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const naviHistoryStart uint64 = 7877880

type suiHistoryIndex struct {
	protocolID, name string
	start            uint64
	api              *sentioAPIClient
	config           SentioIndexerConfig
	engine           *Engine
}

func newSuiHistoryIndex(config SentioIndexerConfig, protocolID, name string, start uint64) (*suiHistoryIndex, error) {
	if _, err := strconv.ParseUint(config.ProcessorVersion, 10, 64); err != nil || config.ProcessorVersion == "0" {
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
	return &suiHistoryIndex{api: newSentioAPIClient(), config: config, protocolID: protocolID, name: name, start: start}, nil
}

func (r *suiHistoryIndex) WithEngine(engine *Engine) *suiHistoryIndex {
	copy := *r
	copy.engine = engine
	return &copy
}

func (r *suiHistoryIndex) request(ctx context.Context, method, endpoint string, body, out any) error {
	if r.engine != nil {
		scope := scopeFrom(ctx)
		scope.lane, scope.observer, scope.protocolID = r.engine.indexerLane, r.engine.observer, r.protocolID
		ctx = context.WithValue(ctx, scanScopeKey{}, scope)
	}
	if err := lockSentioLane(ctx); err != nil {
		return err
	}
	defer unlockSentioLane(ctx)
	return r.api.doJSONOnce(ctx, method, endpoint, body, out)
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

func naviVersionID(id string, version uint64, terminal bool) string {
	suffix := "0"
	if terminal {
		suffix = "1"
	}
	return fmt.Sprintf("%s:%020d:%s", id, version, suffix)
}

func naviHistoryType(kind string) string {
	switch kind {
	case "storage":
		return suiType(naviStorageType)
	case "market":
		return suiFieldType(naviMarketKeyType, "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::MarketInfo")
	case "reserve":
		return suiFieldType("u8", "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::ReserveData")
	case "principal":
		return suiFieldType("address", "u256")
	case "account":
		return suiType(naviAccountType)
	case "vault":
		return suiType(naviVaultPackage + "::navi_vault::Vault")
	case "receipt":
		return suiType(naviVaultPackage + "::navi_vault::Receipt")
	case "receiptState":
		return suiFieldType("address", naviVaultPackage+"::navi_vault::UserState")
	}
	return ""
}

func (r *suiHistoryIndex) result(pin SuiCheckpoint) SuiProtocolPositions {
	return SuiProtocolPositions{ProtocolID: r.protocolID, ProtocolName: r.name, Checkpoint: pin, HeadBeforeRead: pin.Sequence}
}

func suiU8FieldID(parent string, key uint8) (string, error) {
	p, err := ParseSuiAddress(parent)
	if err != nil {
		return "", err
	}
	data := append([]byte{0xf0}, p[:]...)
	data = binary.LittleEndian.AppendUint64(data, 1)
	// BCS u8 key, followed by TypeTag::U8 (variant 1).
	data = append(data, key, 1)
	hash := blake2b.Sum256(data)
	return SuiAddress(hash).Hex(), nil
}
