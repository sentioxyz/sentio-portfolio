package portfolio

import (
	"context"
	"fmt"
	"sort"
)

// SuiProtocolReader reads latest lending and vault state through the same reader as
// wallet holdings. Object lineage discovers and caches protocol root IDs only.
type SuiProtocolReader struct {
	markets, oracle suiRootCache
}

func NewSuiProtocolReader() *SuiProtocolReader {
	return &SuiProtocolReader{markets: suiRootCache{gate: make(chan struct{}, 1)}, oracle: suiRootCache{gate: make(chan struct{}, 1)}}
}

func (r *SuiProtocolReader) ProtocolIDs() []string { return []string{"navi", "volo-vaults", "suilend"} }

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
	ProtocolID     string
	ProtocolName   string
	Checkpoint     SuiCheckpoint
	HeadBeforeRead uint64
	Groups         []SuiProtocolGroup
	Errors         []error
}

type suiProtocolObject struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Owner      string `json:"owner"`
	Parent     string `json:"parent"`
	Key        string `json:"key"`
	ObjectType string `json:"objectType"`
	Content    string `json:"content"`
	Version    string `json:"version"`
}

type suiProtocolValue struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Account string `json:"account"`
	Market  string `json:"market"`
	Content string `json:"content"`
}

type suiProtocolState struct {
	Owned, Topology, Principals, Vaults, ReceiptStates, OracleObjects []suiProtocolObject
	Emodes, AssetValues                                               []suiProtocolValue
}

// ReadLatest reports head observations explicitly; they need not be ordered
// when requests reach different backends. It cannot read history.
func (r *SuiProtocolReader) ReadLatest(ctx context.Context, protocolID string, owner SuiAddress, reader SuiReader) (SuiProtocolPositions, error) {
	if protocolID == "suilend" {
		return readSuilendLatest(ctx, owner, reader)
	}
	name := map[string]string{"navi": "NAVI", "volo-vaults": "Volo Vaults"}[protocolID]
	result := SuiProtocolPositions{ProtocolID: protocolID, ProtocolName: name}
	if name == "" {
		return result, fmt.Errorf("unsupported Sui protocol")
	}
	objects, ok := reader.(SuiObjectReader)
	if !ok {
		return result, fmt.Errorf("Sui object reads are unavailable")
	}
	before, err := reader.LatestCheckpoint(ctx)
	if err != nil {
		return result, err
	}
	state, lendingErr, vaultErr := r.loadLatest(ctx, protocolID, owner, objects)
	after, err := reader.LatestCheckpoint(ctx)
	if err != nil {
		return result, err
	}
	result.Checkpoint, result.HeadBeforeRead = after, before.Sequence
	if protocolID == "navi" {
		if lendingErr == nil {
			var groups []SuiProtocolGroup
			groups, lendingErr = naviLending(ctx, owner, after, reader, state)
			if lendingErr == nil {
				result.Groups = append(result.Groups, groups...)
			}
		}
		if lendingErr != nil {
			result.Errors = append(result.Errors, fmt.Errorf("lending: %w", lendingErr))
		}
	}
	if vaultErr == nil {
		var groups []SuiProtocolGroup
		groups, vaultErr = suiVaults(ctx, protocolID, reader, state)
		if vaultErr == nil {
			result.Groups = append(result.Groups, groups...)
		}
	}
	if vaultErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("vaults: %w", vaultErr))
	}
	for i := range result.Groups {
		group := &result.Groups[i]
		if group.Metadata == nil {
			group.Metadata = map[string]any{}
		}
		group.Metadata["stateMode"] = "latest"
		group.Metadata["headBeforeRead"] = fmt.Sprint(before.Sequence)
		group.Metadata["headAfterRead"] = fmt.Sprint(after.Sequence)
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}
