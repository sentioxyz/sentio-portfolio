package portfolio

// naviHistoryStart is NAVI's first checkpoint, where the processor starts and
// therefore the earliest sample any certificate can claim.
const naviHistoryStart uint64 = 7877880

// newNaviIndex reads completed NAVI samples from its processor index: the wallet's account
// capabilities and vault receipts, the lending principals attributed to those accounts, the
// receipt states under their vaults, the protocol-wide reserve topology, and the e-mode values its
// events carry.
//
// Unlike Suilend, NAVI keeps its node path for latest reads, which answer from head object state
// and are fresher than the newest completed sample. The index answers only what a node cannot, a
// past checkpoint or hour, and is never a fallback for a failed node read. Both paths run the same
// naviLending and suiVaults math over the same suiProtocolState; only where that state is read
// differs.
func newNaviIndex(config SentioIndexerConfig) (*suiHistoryIndex, error) {
	return newSuiHistoryIndex(config, "navi", "NAVI", naviHistoryStart)
}
