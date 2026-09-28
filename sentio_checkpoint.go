package portfolio

import (
	"context"
	"fmt"
	"strconv"
)

// indexerCheckpointRow is an IndexerCheckpoint row as a time-travel query returns it.
type indexerCheckpointRow struct {
	BlockNumber string `json:"blockNumber"`
	TimestampMS string `json:"timestampMs"`
}

// indexStart is the block an index's processor starts at on one chain, and the client that reads
// that chain. It stands in for the checkpoint the processor has not written yet.
//
// A processor writes its checkpoint on a time interval, a week long while it backfills, and the
// driver fires the interval on weekly boundaries rather than at the start block. The first row
// therefore lands up to seven days after the index starts, and a query at a block before it finds
// no checkpoint, although the chain status proved the processor has passed that block. The start
// block then serves as the checkpoint and is validated like a written one, so it is accepted only
// within the backfill lag of the start: later, a missing checkpoint fails as a stale one.
//
// The block must be the one the processor's checkpoint binding starts at on the chain.
type indexStart struct {
	client *RPCClient
	block  uint64
}

// checkpoint reads the checkpoint rows of one time-travel query and returns the checkpoint's block
// and time, or the index start's when the processor had written none by the query block.
func (s indexStart) checkpoint(ctx context.Context, rows []indexerCheckpointRow) (uint64, uint64, error) {
	switch len(rows) {
	case 0:
		start, err := s.client.BlockByNumber(ctx, s.block)
		if err != nil {
			return 0, 0, fmt.Errorf("GraphQL returned 0 checkpoints; read index start %d: %w", s.block, err)
		}
		return s.block, start.Timestamp * 1_000, nil
	case 1:
	default:
		return 0, 0, fmt.Errorf("GraphQL returned %d checkpoints", len(rows))
	}
	block, err := strconv.ParseUint(rows[0].BlockNumber, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid checkpoint block: %w", err)
	}
	timestampMS, err := strconv.ParseUint(rows[0].TimestampMS, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid checkpoint timestamp: %w", err)
	}
	return block, timestampMS, nil
}
