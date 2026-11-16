package portfolio

// cetusHistoryStart is the publication checkpoint of Cetus's original CLMM type package, where
// its processor starts and therefore the earliest sample any certificate can claim.
const cetusHistoryStart uint64 = 1579561

// cetusLinkedFields are the derived object IDs the processor writes into a position's links: its
// PositionInfo record in the pool's position table and its two boundary ticks in the pool's tick
// table. All three are dynamic fields owned by a table rather than by the wallet, and their IDs
// follow from the pool's tables and the position's range, so the position names them directly.
var cetusLinkedFields = []string{"positionInfoId", "lowerTickId", "upperTickId"}

// newCetusIndex reads completed Cetus samples from its processor index: the wallet's directly held
// position NFTs, the pool each one names, and the PositionInfo record and boundary ticks each one
// links to.
//
// Like NAVI, Cetus keeps its node path for latest reads, which answer from head object state and
// are fresher than the newest completed sample; the index answers only a past checkpoint or hour.
// Both paths run loadSuiCLMM and suiCLMMGroups over a SuiObjectReader, so only where the objects
// come from differs. A sample's timestamp is the time its rewards accrue to.
func newCetusIndex(config SentioIndexerConfig) (*suiHistoryIndex, error) {
	return newSuiHistoryIndex(config, "cetus", "Cetus", cetusHistoryStart)
}
