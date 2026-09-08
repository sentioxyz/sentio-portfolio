package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// presenceMinChains is how many chains an indexer must be scanning before the probe pays for
// itself: it costs one request and saves one per chain without rows, so below three chains it can
// only break even.
const presenceMinChains = 3

// presenceField is one entity the probe asks for on every chain: the entity's GraphQL field and
// the where clause, as a literal object body, that selects the account's rows on one chain.
type presenceField struct {
	entity string
	where  func(chainID ChainID) string
}

// presenceProbe describes one indexer's account-keyed entities.
//
// Most index-backed adapters keep one processor per protocol and query it once per chain the
// protocol is deployed on, so an account with positions on one chain still costs a scan a request
// on every other chain — each of which returns nothing. The probe asks every chain the same
// question in a single request, aliasing one time-travel query per chain at that chain's own
// indexed block, and the per-chain flow then skips the chains the probe proved empty.
//
// The proof is exact rather than heuristic: a chain is skipped only when every field returned no
// row at the very block the per-chain query would have used, and its checkpoint passed the same
// validation, so the skipped query could only ever have returned the same nothing. Everything
// after the indexed block — the RPC tail up to the pinned block — runs unchanged.
type presenceProbe struct {
	// name keys the memo and labels the operation; one per indexer.
	name           string
	config         SentioIndexerConfig
	requiredChains []ChainID
	fields         []presenceField
	maxRPCTail     uint64
	liveMaxLag     time.Duration
	backfillMaxLag time.Duration
}

// presenceProof is what the probe established for one chain: no rows for the account as of
// QueryBlock, whose checkpoint the indexer reported as Checkpoint.
type presenceProof struct {
	QueryBlock uint64
	Checkpoint uint64
}

// presenceResult is the probe's verdict: a proof for every chain shown to hold nothing.
type presenceResult struct {
	proofs map[ChainID]presenceProof
}

// presencePrefetcher is implemented by adapters whose indexer can answer, in one request, which
// chains hold anything for an account. The engine calls it as the protocol phase starts, so the
// probe runs while the workers are still on adapters that need no indexer at all.
type presencePrefetcher interface {
	prefetchPresence(ctx context.Context, account common.Address)
}

func presenceKey(probe presenceProbe, account string) string {
	return probe.name + "@" + probe.config.GraphQLURL + "@" + account
}

// chainProvenEmpty reports whether the probe proved that the per-chain query for block would
// return no rows, and hands back the proof: the block the emptiness holds at and the checkpoint
// that query would have seen. It reports false whenever there is nothing to prove it with — no
// scan scope, too few chains, a failed probe — so the caller always has the full per-chain flow
// to fall back on. account keys the memo: a scan asks about its root address and each attributed
// account separately.
//
// A proof established at an earlier query block still holds, because emptiness at a block is a
// fact about that block. The caller must then treat proof.QueryBlock as its indexed block, so the
// RPC tail covers everything after it. A proof from a later block than the caller would query is
// not one it can use.
func (c *sentioAPIClient) chainProvenEmpty(
	ctx context.Context,
	probe presenceProbe,
	block BlockRef,
	account string,
	statuses map[ChainID]sentioChainStatus,
) (presenceProof, bool) {
	scope := scopeFrom(ctx)
	if scope.memo == nil || len(scope.pinned) == 0 {
		return presenceProof{}, false
	}
	result, _ := scope.memo.once(ctx, presenceKey(probe, account), func() any {
		return c.presence(ctx, probe, scope.pinned, statuses, account)
	}).(presenceResult)
	proof, proven := result.proofs[block.ChainID]
	if !proven || proof.QueryBlock > min(block.Number, statuses[block.ChainID].ProcessedBlock) {
		return presenceProof{}, false
	}
	return proof, true
}

// awaitPresence waits for a prefetched probe to finish. Callers invoke it before taking a lane
// slot: the probe needs a slot of its own, so a chain job that held one while waiting on the probe
// could starve it — and with a lane of one, deadlock the scan.
func (c *sentioAPIClient) awaitPresence(ctx context.Context, probe presenceProbe, account string) {
	scope := scopeFrom(ctx)
	if scope.memo == nil {
		return
	}
	scope.memo.await(ctx, presenceKey(probe, account))
}

// prefetchPresence starts the probe for account in the background, taking a prefetch slot and
// then a lane slot, so that by the time a worker reaches the indexer's first chain the verdict is
// ready or nearly so. It is a no-op outside a scan and when the probe was already started.
func (c *sentioAPIClient) prefetchPresence(ctx context.Context, probe presenceProbe, account string) {
	scope := scopeFrom(ctx)
	if scope.memo == nil || len(scope.pinned) == 0 {
		return
	}
	scope.memo.start(presenceKey(probe, account), func() any {
		if scope.prefetchSlots != nil {
			select {
			case scope.prefetchSlots <- struct{}{}:
				defer func() { <-scope.prefetchSlots }()
			case <-ctx.Done():
				return presenceResult{}
			}
		}
		if err := lockSentioLane(ctx); err != nil {
			return presenceResult{}
		}
		defer unlockSentioLane(ctx)
		statuses, err := c.chainStatuses(ctx, probe.config, nil, false)
		if err != nil {
			return presenceResult{}
		}
		return c.presence(ctx, probe, scope.pinned, statuses, account)
	})
}

// presence decides which chains are worth probing and runs the probe. It expects the caller to
// hold a lane slot.
func (c *sentioAPIClient) presence(
	ctx context.Context,
	probe presenceProbe,
	pinned map[ChainID]BlockRef,
	statuses map[ChainID]sentioChainStatus,
	account string,
) presenceResult {
	targets := presenceTargets(probe, pinned, statuses)
	if len(targets) < presenceMinChains {
		return presenceResult{}
	}
	return c.runPresenceProbe(ctx, probe, targets, account)
}

// presenceTarget is one chain the probe asks about, at the block the per-chain query would use.
type presenceTarget struct {
	block      BlockRef
	queryBlock uint64
}

// presenceTargets selects the chains the probe can speak for: pinned in this scan, configured for
// the indexer, and within the indexer's RPC tail bound. A chain outside that bound fails in its
// own per-chain flow, and the probe must not pre-empt that error.
func presenceTargets(
	probe presenceProbe,
	pinned map[ChainID]BlockRef,
	statuses map[ChainID]sentioChainStatus,
) map[ChainID]presenceTarget {
	targets := make(map[ChainID]presenceTarget)
	for _, chainID := range probe.requiredChains {
		block, isPinned := pinned[chainID]
		status, hasStatus := statuses[chainID]
		if !isPinned || !hasStatus || status.err != nil || status.State != "PROCESSING_LATEST" {
			continue
		}
		if block.Number > status.ProcessedBlock && block.Number-status.ProcessedBlock > probe.maxRPCTail {
			continue
		}
		targets[chainID] = presenceTarget{block: block, queryBlock: min(block.Number, status.ProcessedBlock)}
	}
	return targets
}

func presenceAlias(prefix string, chainID ChainID) string {
	return prefix + strconv.FormatUint(uint64(chainID), 10)
}

// presenceQuery builds one request with, per chain, its checkpoint and one row of each field at
// that chain's indexed block. Values are inlined rather than passed as variables: every value is
// a chain ID, a block number or a lowercase address the kernel itself produced.
func presenceQuery(probe presenceProbe, targets map[ChainID]presenceTarget) string {
	chainIDs := make([]ChainID, 0, len(targets))
	for chainID := range targets {
		chainIDs = append(chainIDs, chainID)
	}
	sort.Slice(chainIDs, func(left, right int) bool { return chainIDs[left] < chainIDs[right] })
	var query strings.Builder
	query.WriteString("query Presence {\n")
	for _, chainID := range chainIDs {
		block := strconv.FormatUint(targets[chainID].queryBlock, 10)
		fmt.Fprintf(
			&query,
			"  %s: indexerCheckpoints(first: 2, block: { number: %s }, where: { id: \"%d\" }) { blockNumber timestampMs }\n",
			presenceAlias("cp", chainID), block, chainID,
		)
		for index, field := range probe.fields {
			fmt.Fprintf(
				&query,
				"  %s: %s(first: 1, orderBy: id, orderDirection: asc, block: { number: %s }, where: { %s }) { id }\n",
				presenceAlias(fmt.Sprintf("f%d_", index), chainID), field.entity, block, field.where(chainID),
			)
		}
	}
	query.WriteString("}")
	return query.String()
}

type presenceResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type presenceCheckpoint struct {
	BlockNumber string `json:"blockNumber"`
	TimestampMS string `json:"timestampMs"`
}

// runPresenceProbe asks every target chain at once. A request or GraphQL failure proves nothing,
// so it returns an empty verdict and the per-chain flow reports whatever the failure was.
func (c *sentioAPIClient) runPresenceProbe(
	ctx context.Context,
	probe presenceProbe,
	targets map[ChainID]presenceTarget,
	account string,
) presenceResult {
	var payload presenceResponse
	err := c.doJSON(ctx, http.MethodPost, probe.config.GraphQLURL, map[string]any{
		"query": presenceQuery(probe, targets),
	}, &payload)
	if err != nil || len(payload.Errors) > 0 {
		return presenceResult{}
	}
	proofs := make(map[ChainID]presenceProof, len(targets))
	for chainID, target := range targets {
		if checkpoint, proven := presenceChainEmpty(probe, payload.Data, chainID, target); proven {
			proofs[chainID] = presenceProof{QueryBlock: target.queryBlock, Checkpoint: checkpoint}
		}
	}
	return presenceResult{proofs: proofs}
}

// presenceChainEmpty decides one chain: a valid checkpoint and no row in any field. It returns the
// checkpoint block so a caller that reports it can report the same value the page would have.
func presenceChainEmpty(
	probe presenceProbe,
	data map[string]json.RawMessage,
	chainID ChainID,
	target presenceTarget,
) (uint64, bool) {
	var checkpoints []presenceCheckpoint
	raw, exists := data[presenceAlias("cp", chainID)]
	if !exists || json.Unmarshal(raw, &checkpoints) != nil || len(checkpoints) != 1 {
		return 0, false
	}
	checkpointBlock, err := strconv.ParseUint(checkpoints[0].BlockNumber, 10, 64)
	if err != nil {
		return 0, false
	}
	checkpointMS, err := strconv.ParseUint(checkpoints[0].TimestampMS, 10, 64)
	if err != nil {
		return 0, false
	}
	if checkpointBlock > target.queryBlock ||
		validatePresenceCheckpoint(probe, target.block, checkpointBlock, checkpointMS) != nil {
		return 0, false
	}
	for index := range probe.fields {
		var rows []json.RawMessage
		raw, exists := data[presenceAlias(fmt.Sprintf("f%d_", index), chainID)]
		if !exists || json.Unmarshal(raw, &rows) != nil || len(rows) != 0 {
			return 0, false
		}
	}
	return checkpointBlock, true
}

// validatePresenceCheckpoint applies the freshness rule every indexer applies to its own pages,
// so a chain the probe skips is one the per-chain query would have accepted too.
func validatePresenceCheckpoint(
	probe presenceProbe,
	block BlockRef,
	checkpointBlock uint64,
	checkpointMS uint64,
) error {
	if checkpointBlock > block.Number {
		return fmt.Errorf("indexer checkpoint %d is ahead of pinned block %d", checkpointBlock, block.Number)
	}
	checkpointSeconds := checkpointMS / 1_000
	if checkpointSeconds > block.Timestamp+60 {
		return fmt.Errorf("indexer checkpoint timestamp is ahead of pinned block")
	}
	if block.Timestamp <= checkpointSeconds {
		return nil
	}
	lag := time.Duration(block.Timestamp-checkpointSeconds) * time.Second
	maximumLag := probe.liveMaxLag
	if block.Fixed {
		maximumLag = probe.backfillMaxLag
	}
	if lag > maximumLag {
		return fmt.Errorf("indexer checkpoint is stale by %s", lag)
	}
	return nil
}
