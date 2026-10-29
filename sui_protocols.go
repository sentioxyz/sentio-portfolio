package portfolio

import (
	"context"
	"fmt"
	"sort"
)

// suiNodeProtocols reads latest lending, vault and liquidity state from a node's head object
// state. Object lineage discovers and caches protocol root IDs only. Suilend has no node path and
// is not read here.
type suiNodeProtocols struct {
	markets, oracle suiRootCache
}

func newSuiNodeProtocols() *suiNodeProtocols {
	return &suiNodeProtocols{markets: suiRootCache{gate: make(chan struct{}, 1)}, oracle: suiRootCache{gate: make(chan struct{}, 1)}}
}

// SuiComponent is one amount of one coin a group holds or owes. PriceUSD and ValueUSD are set when
// the coin was priced; a debt's ValueUSD is positive and is subtracted from its group.
type SuiComponent struct {
	Kind                 string
	Coin                 SuiCoinMetadata
	AmountRaw            string
	AmountDenominatorRaw string
	PriceUSD             *float64
	ValueUSD             *float64
	Metadata             map[string]any
}

// SuiGroup is one position: a wallet coin, a lending account in one market, a vault receipt or a
// liquidity NFT. ValueUSD nets its priced components and is nil when none was priced.
type SuiGroup struct {
	ID         string
	MarketID   string
	Label      string
	Components []SuiComponent
	ValueUSD   *float64
	Metadata   map[string]any
}

// suiPositions is what one read of one protocol returned, before valuation: the checkpoint it
// answers for (the head seen after a node read, or the sample of an index read) and the errors of
// the parts it could not read.
type suiPositions struct {
	ProtocolID     string
	ProtocolName   string
	Checkpoint     SuiCheckpoint
	HeadBeforeRead uint64
	Groups         []SuiGroup
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
	VaultPrices                                                       map[string]suiProtocolObject
}

// ReadLatest reports head observations explicitly; they need not be ordered
// when requests reach different backends. It cannot read history.
func (r *suiNodeProtocols) ReadLatest(ctx context.Context, protocolID string, owner SuiAddress, reader SuiReader) (suiPositions, error) {
	if protocolID == "cetus" || protocolID == "bluefin" {
		return readSuiCLMMLatest(ctx, protocolID, owner, reader)
	}
	name := map[string]string{"navi": "NAVI", "volo-vaults": "Volo Vaults"}[protocolID]
	result := suiPositions{ProtocolID: protocolID, ProtocolName: name}
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
			var groups []SuiGroup
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
		var groups []SuiGroup
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
