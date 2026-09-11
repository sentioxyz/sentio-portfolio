package portfolio

import (
	"context"
	"fmt"
	"sort"
)

// SuiProtocolReader reads latest on-chain NAVI and Volo state. A shared-object
// directory discovers protocol roots; it supplies no balances or NAV values.
type SuiProtocolReader struct{ directory SuiObjectDirectory }

func NewSuiProtocolReader(directory SuiObjectDirectory) *SuiProtocolReader {
	return &SuiProtocolReader{directory: directory}
}

func (r *SuiProtocolReader) ProtocolIDs() []string { return []string{"navi", "volo-vaults"} }

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

// ReadLatest reports the observation window explicitly; it cannot read history.
func (r *SuiProtocolReader) ReadLatest(ctx context.Context, protocolID string, owner SuiAddress, reader SuiReader) (SuiProtocolPositions, error) {
	name := map[string]string{"navi": "NAVI", "volo-vaults": "Volo Vaults"}[protocolID]
	result := SuiProtocolPositions{ProtocolID: protocolID, ProtocolName: name}
	if name == "" {
		return result, fmt.Errorf("unsupported Sui protocol")
	}
	objects, ok := reader.(SuiObjectReader)
	if !ok {
		return result, fmt.Errorf("Sui object reads are unavailable")
	}
	if r.directory == nil {
		return result, fmt.Errorf("Sui object directory is not configured")
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
	if after.Sequence < before.Sequence || after.Timestamp.Before(before.Timestamp) {
		return result, fmt.Errorf("Sui head moved backwards during protocol read")
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
		groups, vaultErr = suiVaults(ctx, protocolID, after, reader, state)
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
