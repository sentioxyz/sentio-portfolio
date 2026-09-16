package portfolio

import "context"

// NaviHistoryReader reads completed hourly NAVI state from a version-pinned
// processor index. It never accepts or queries a Sui node reader.
type NaviHistoryReader struct{ *suiHistoryIndex }

func NewNaviHistoryReader(config SentioIndexerConfig) (*NaviHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "navi", "NAVI", naviHistoryStart)
	if err != nil {
		return nil, err
	}
	return &NaviHistoryReader{i}, nil
}

func (r *NaviHistoryReader) WithEngine(engine *Engine) *NaviHistoryReader {
	return &NaviHistoryReader{r.suiHistoryIndex.WithEngine(engine)}
}

const voloHistoryStart uint64 = 172857371

// VoloHistoryReader preserves the quote paired with each vault's stored NAV.
// Object contents, ownership, settlement provenance and metadata come from the
// dedicated Volo index, including for the latest completed hourly sample.
type VoloHistoryReader struct{ *suiHistoryIndex }

func NewVoloHistoryReader(config SentioIndexerConfig) (*VoloHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "volo-vaults", "Volo Vaults", voloHistoryStart)
	if err != nil {
		return nil, err
	}
	return &VoloHistoryReader{i}, nil
}

func (r *VoloHistoryReader) WithEngine(engine *Engine) *VoloHistoryReader {
	return &VoloHistoryReader{r.suiHistoryIndex.WithEngine(engine)}
}

const suilendHistoryStart uint64 = 28510257

// SuilendHistoryReader reads completed hourly lending state from its processor
// index, including ownership, market reserves, obligations and coin metadata.
type SuilendHistoryReader struct{ *suiHistoryIndex }

func NewSuilendHistoryReader(config SentioIndexerConfig) (*SuilendHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "suilend", "Suilend", suilendHistoryStart)
	if err != nil {
		return nil, err
	}
	return &SuilendHistoryReader{i}, nil
}

func (r *SuilendHistoryReader) WithEngine(engine *Engine) *SuilendHistoryReader {
	return &SuilendHistoryReader{r.suiHistoryIndex.WithEngine(engine)}
}

// ReadLatest returns the newest completed hourly sample certified by a later
// index watermark. Its checkpoint describes that sample, not the current head.
// Hosts may enforce a wall-clock freshness bound before serving the result.
func (r *suiHistoryIndex) ReadLatest(ctx context.Context, owner SuiAddress) (SuiProtocolPositions, error) {
	return r.readSQLPortfolio(ctx, owner, suiSQLSelection{})
}

// CetusHistoryReader reads hourly CLMM positions and accrual inputs from one SQL query.
type CetusHistoryReader struct{ *suiHistoryIndex }

func NewCetusHistoryReader(config SentioIndexerConfig) (*CetusHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "cetus", "Cetus", 1579561)
	if err != nil {
		return nil, err
	}
	return &CetusHistoryReader{i}, nil
}
func (r *CetusHistoryReader) WithEngine(e *Engine) *CetusHistoryReader {
	return &CetusHistoryReader{r.suiHistoryIndex.WithEngine(e)}
}

// BluefinHistoryReader reads hourly CLMM positions and accrual inputs from one SQL query.
type BluefinHistoryReader struct{ *suiHistoryIndex }

func NewBluefinHistoryReader(config SentioIndexerConfig) (*BluefinHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "bluefin", "Bluefin", 71783891)
	if err != nil {
		return nil, err
	}
	return &BluefinHistoryReader{i}, nil
}
func (r *BluefinHistoryReader) WithEngine(e *Engine) *BluefinHistoryReader {
	return &BluefinHistoryReader{r.suiHistoryIndex.WithEngine(e)}
}
