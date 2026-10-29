package portfolio

import (
	"fmt"
	"time"
)

const (
	// indexMaxLag is how far a live index read may trail its pin before the scan says so. Nothing
	// tops an index read up from the chain, so a position the account opened after the indexed
	// block is absent from it; past this lag the read still returns, with an indexLagNote.
	indexMaxLag = time.Hour
	// indexBackfillMaxLag bounds the age of the checkpoint that certifies a read at a fixed pin. A
	// processor writes its checkpoint weekly while it backfills, so a historical block can lie up
	// to a week past the last row before it.
	indexBackfillMaxLag = 7*24*time.Hour + 30*time.Minute
)

// indexLagNote is the coverage note for a live index read that trails its pin by more than
// indexMaxLag. The adapter still read every quantity it reports at the pin, but the index chose
// what to read as of its checkpoint, so a position opened after it is missing.
type indexLagNote struct {
	checkpoint uint64
	lag        time.Duration
}

func (n *indexLagNote) Error() string {
	return fmt.Sprintf(
		"indexer trails the pinned block by %s: positions opened after block %d may be missing",
		n.lag.Truncate(time.Minute), n.checkpoint,
	)
}

// indexQueryBlock reads the processor status of block's chain through read and returns it with
// the block an index read for block queries at: the pin, or the processor's last block when it
// trails the pin. A live read takes the processor wherever it is, and its checkpoint then says how
// far behind it is (checkIndexCheckpoint). A read at a fixed pin must find the processor past the
// pin, since nothing makes up the blocks it has not processed and a historical scan is kept rather
// than retried, so a status that trails the pin is read again before the read fails.
func indexQueryBlock(
	block BlockRef,
	read func(forceRefresh bool) (map[ChainID]sentioChainStatus, error),
) (map[ChainID]sentioChainStatus, uint64, error) {
	statuses, err := read(false)
	if err != nil {
		return nil, 0, err
	}
	status, exists := statuses[block.ChainID]
	if exists && block.Fixed && status.ProcessedBlock < block.Number {
		if statuses, err = read(true); err != nil {
			return nil, 0, err
		}
		status, exists = statuses[block.ChainID]
	}
	if !exists {
		return nil, 0, fmt.Errorf("processor status omitted chain %d", block.ChainID)
	}
	if status.State != "PROCESSING_LATEST" {
		return nil, 0, fmt.Errorf(
			"indexer backfill is incomplete for chain %d: state=%s processed=%d estimatedLatest=%d",
			block.ChainID, status.State, status.ProcessedBlock, status.EstimatedLatest,
		)
	}
	if block.Fixed && status.ProcessedBlock < block.Number {
		return nil, 0, fmt.Errorf(
			"indexer has processed chain %d to block %d, short of pinned block %d",
			block.ChainID, status.ProcessedBlock, block.Number,
		)
	}
	return statuses, min(block.Number, status.ProcessedBlock), nil
}

// checkIndexCheckpoint validates the checkpoint that certifies an index read against the pin. A
// checkpoint ahead of the pin fails at any pin. At a fixed pin the processor has passed the pin,
// so the read is exact and its checkpoint only has to be as recent as the backfill cadence
// allows. A live read may trail the pin by any amount; past indexMaxLag it returns a note saying
// so rather than failing.
func checkIndexCheckpoint(block BlockRef, checkpointBlock, checkpointMS uint64) (*indexLagNote, error) {
	if checkpointBlock > block.Number {
		return nil, fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", checkpointBlock, block.Number)
	}
	checkpointSeconds := checkpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return nil, fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp <= checkpointSeconds {
		return nil, nil
	}
	lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
	if block.Fixed {
		if lag > indexBackfillMaxLag {
			return nil, fmt.Errorf("indexer checkpoint is stale by %s", lag)
		}
		return nil, nil
	}
	if lag > indexMaxLag {
		return &indexLagNote{checkpoint: checkpointBlock, lag: lag}, nil
	}
	return nil, nil
}
