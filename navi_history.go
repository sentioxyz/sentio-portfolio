package portfolio

// naviHistoryStart is NAVI's first checkpoint, where the processor starts and
// therefore the earliest sample any certificate can claim.
const naviHistoryStart uint64 = 7877880

// NaviHistoryReader reads completed NAVI samples from its processor index: the
// wallet's account capabilities and vault receipts, the lending principals
// attributed to those accounts, the receipt states under their vaults, the
// protocol-wide reserve topology, and the e-mode values its events carry.
//
// Unlike Suilend, NAVI keeps its node path for latest reads: SuiProtocolReader
// still answers ReadLatest from head object state, which is fresher than the
// newest completed sample. This reader answers only what a node cannot, a past
// checkpoint or hour. Both paths run the same naviLending and suiVaults math
// over the same suiProtocolState; only where that state is read differs.
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
