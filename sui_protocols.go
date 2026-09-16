package portfolio

import (
	"context"
	"fmt"
)

// SuiProtocolReader routes lending and vault protocols through configured indexes.
// All protocol quantities come from dedicated processor indexes.
type SuiProtocolReader struct {
	navi    *NaviHistoryReader
	volo    *VoloHistoryReader
	suilend *SuilendHistoryReader
	cetus   *CetusHistoryReader
	bluefin *BluefinHistoryReader
}

func NewSuiProtocolReader() *SuiProtocolReader {
	return &SuiProtocolReader{}
}

func (r *SuiProtocolReader) ProtocolIDs() []string {
	return []string{"navi", "volo-vaults", "suilend", "cetus", "bluefin"}
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
	VaultPrices                                                       map[string]suiProtocolObject
	Suilend                                                           *suilendState
}

// ReadLatest returns a completed indexed hour. The reader parameter is retained
// for source compatibility and is never invoked for protocol positions.
func (r *SuiProtocolReader) ReadLatest(ctx context.Context, protocolID string, owner SuiAddress, reader SuiReader) (SuiProtocolPositions, error) {
	switch protocolID {
	case "cetus":
		if r.cetus != nil {
			return r.cetus.ReadLatest(ctx, owner)
		}
	case "bluefin":
		if r.bluefin != nil {
			return r.bluefin.ReadLatest(ctx, owner)
		}
	case "suilend":
		if r.suilend != nil {
			return r.suilend.ReadLatest(ctx, owner)
		}
	case "navi":
		if r.navi != nil {
			return r.navi.ReadLatest(ctx, owner)
		}
	case "volo-vaults":
		if r.volo != nil {
			return r.volo.ReadLatest(ctx, owner)
		}
	default:
		return SuiProtocolPositions{ProtocolID: protocolID}, fmt.Errorf("unsupported Sui protocol")
	}
	return SuiProtocolPositions{ProtocolID: protocolID}, fmt.Errorf("%s requires a configured processor index", protocolID)
}

// WithNaviHistory and WithVoloHistory return independent configured readers.
// Missing or incomplete indexes never fall back to node history.
func (r *SuiProtocolReader) WithNaviHistory(index *NaviHistoryReader) *SuiProtocolReader {
	copy := *r
	copy.navi = index
	return &copy
}

// WithVoloHistory configures the dedicated Volo index without changing r.
func (r *SuiProtocolReader) WithVoloHistory(index *VoloHistoryReader) *SuiProtocolReader {
	copy := *r
	copy.volo = index
	return &copy
}

// WithSuilendHistory configures Suilend without changing r. No node fallback is used.
func (r *SuiProtocolReader) WithSuilendHistory(index *SuilendHistoryReader) *SuiProtocolReader {
	copy := *r
	copy.suilend = index
	return &copy
}

func (r *SuiProtocolReader) WithCetusHistory(index *CetusHistoryReader) *SuiProtocolReader {
	copy := *r
	copy.cetus = index
	return &copy
}
func (r *SuiProtocolReader) WithBluefinHistory(index *BluefinHistoryReader) *SuiProtocolReader {
	copy := *r
	copy.bluefin = index
	return &copy
}
