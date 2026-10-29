package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
// row at the block the per-chain query would have used, and its checkpoint passed the same
// validation without a lag note, so the skipped query could only ever have returned the same
// nothing.
type presenceProbe struct {
	// name keys the memo and labels the operation; one per indexer.
	name           string
	config         SentioIndexerConfig
	requiredChains []ChainID
	fields         []presenceField
}

// presenceProof is what the probe established for one chain: no rows for the account as of
// QueryBlock, whose checkpoint the indexer reported as Checkpoint, written at CheckpointMS.
type presenceProof struct {
	QueryBlock   uint64
	Checkpoint   uint64
	CheckpointMS uint64
}

// presenceResult is the probe's verdict: a proof for every chain shown to hold nothing.
type presenceResult struct {
	proofs map[ChainID]presenceProof
}

// presencePrefetcher is implemented by adapters whose indexer can answer, in one request, which
// chains hold anything for an account. The engine calls it as the protocol phase starts, so the
// probe runs while the workers are still on adapters that need no indexer at all, and cancels ctx
// once no deployment of the protocol in the scan is left to use the verdict.
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
// account separately, each only on the chains the scan reads it on.
//
// A proof established at an earlier query block than the caller's still holds: emptiness at a
// block is a fact about that block, and the proof's checkpoint trails the pin by no more than
// indexMaxLag, as a page the caller accepted without a note would. The caller then reports
// proof.QueryBlock as its indexed block. At a fixed pin the proof must be at the pin itself.
//
// The call may wait for a prefetched probe or send the probe itself, which takes a lane slot for
// its request. The caller must hold none while it waits: the other chain jobs of the scan wait
// for the probe too, and with a lane of one a waiter holding the slot would deadlock the scan.
// Index reads hold a slot only for each request's round trip, so none ever does.
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
		return c.presence(withPresence(ctx), probe, scope.pinnedFor(account), statuses, account)
	}).(presenceResult)
	proof, proven := result.proofs[block.ChainID]
	if !proven || proof.QueryBlock > block.Number || (block.Fixed && proof.QueryBlock != block.Number) {
		return presenceProof{}, false
	}
	return proof, true
}

// prefetchPresence starts the probe for account in the background, taking a prefetch slot, so
// that by the time a worker reaches the indexer's first chain the verdict is ready or nearly so.
// It is a no-op outside a scan, when the probe was already started, and for an account the scan
// reads on too few of the probe's chains to probe at all — one attributed through Instadapp, read
// on Ethereum alone.
func (c *sentioAPIClient) prefetchPresence(ctx context.Context, probe presenceProbe, account string) {
	scope := scopeFrom(ctx)
	if scope.memo == nil || len(scope.pinned) == 0 {
		return
	}
	pinned := scope.pinnedFor(account)
	probed := 0
	for _, chainID := range probe.requiredChains {
		if _, isPinned := pinned[chainID]; isPinned {
			probed++
		}
	}
	if probed < presenceMinChains {
		return
	}
	ctx = withPresence(ctx)
	scope.memo.start(presenceKey(probe, account), func() any {
		if scope.prefetchSlots != nil {
			select {
			case scope.prefetchSlots <- struct{}{}:
				defer func() { <-scope.prefetchSlots }()
			case <-ctx.Done():
				return presenceResult{}
			}
		}
		statuses, err := c.chainStatuses(ctx, probe.config, nil, false)
		if err != nil {
			return presenceResult{}
		}
		return c.presence(ctx, probe, pinned, statuses, account)
	})
}

// presence decides which chains are worth probing and, when enough of them are, sends the probe,
// which takes a lane slot only for its request, so a probe with too few chains to ask about takes
// none at all.
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
// the indexer, and with a status the per-chain flow accepts (indexQueryBlock). A chain whose
// status it rejects fails in its own per-chain flow, and the probe must not pre-empt that error.
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
		if block.Fixed && status.ProcessedBlock < block.Number {
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
		query.WriteString("  " + indexCheckpointField(presenceAlias("cp", chainID), chainID, targets[chainID].queryBlock) + "\n")
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

// runPresenceProbe asks every target chain at once. A request or GraphQL failure proves nothing,
// so it returns an empty verdict and the per-chain flow reports whatever the failure was.
func (c *sentioAPIClient) runPresenceProbe(
	ctx context.Context,
	probe presenceProbe,
	targets map[ChainID]presenceTarget,
	account string,
) presenceResult {
	var data map[string]json.RawMessage
	if err := c.graphql(ctx, probe.config.GraphQLURL, presenceQuery(probe, targets), &data); err != nil {
		return presenceResult{}
	}
	proofs := make(map[ChainID]presenceProof, len(targets))
	for chainID, target := range targets {
		if checkpoint, checkpointMS, proven := presenceChainEmpty(probe, data, chainID, target); proven {
			proofs[chainID] = presenceProof{QueryBlock: target.queryBlock, Checkpoint: checkpoint, CheckpointMS: checkpointMS}
		}
	}
	return presenceResult{proofs: proofs}
}

// presenceChainEmpty decides one chain: a valid checkpoint and no row in any field. It returns the
// checkpoint so a later read at the proof's block can be held to the same one.
func presenceChainEmpty(
	probe presenceProbe,
	data map[string]json.RawMessage,
	chainID ChainID,
	target presenceTarget,
) (uint64, uint64, bool) {
	// The probe never uses the start as a stand-in: a chain without a written checkpoint is not
	// proven empty and runs its own pages.
	raw, exists := data[presenceAlias("cp", chainID)]
	if !exists {
		return 0, 0, false
	}
	checkpoint, err := parseIndexCheckpoint(raw)
	if err != nil || !checkpoint.written || checkpoint.block > target.queryBlock {
		return 0, 0, false
	}
	// A chain whose page would carry a lag note runs that page, so the scan reports the lag.
	if note, err := checkIndexCheckpoint(target.block, checkpoint.block, checkpoint.ms); err != nil || note != nil {
		return 0, 0, false
	}
	for index := range probe.fields {
		var rows []json.RawMessage
		raw, exists := data[presenceAlias(fmt.Sprintf("f%d_", index), chainID)]
		if !exists || json.Unmarshal(raw, &rows) != nil || len(rows) != 0 {
			return 0, 0, false
		}
	}
	return checkpoint.block, checkpoint.ms, true
}
