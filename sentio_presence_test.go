package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	presenceTestAccount = common.HexToAddress("0xC02dd10b401E01E0fb3BF497E46E6d6b51664AD7")
	presenceTestVault   = common.HexToAddress("0xBEEF0e0834849aCc03f0089F01F4f1eeB06873C9")
)

const (
	presenceTestBlock     = uint64(1_000)
	presenceTestTimestamp = uint64(1_700_000_000)
)

// presenceTestServer serves a Morpho indexer whose account holds one vault position on Base and
// nothing elsewhere. It answers status reads, the presence probe and per-chain pages, counting
// the last two so a test can see which of them a scan needed.
type presenceTestServer struct {
	t              *testing.T
	chains         []ChainID
	probes         atomic.Int32
	pages          atomic.Int32
	pagedChains    []ChainID
	probeErrors    bool
	staleChains    map[ChainID]bool
	lastProbeQuery atomic.Pointer[string]
	// processed overrides the processed block the status reports; zero means presenceTestBlock.
	processed atomic.Uint64
}

func (s *presenceTestServer) processedBlock() uint64 {
	if processed := s.processed.Load(); processed != 0 {
		return processed
	}
	return presenceTestBlock
}

func (s *presenceTestServer) checkpoint(chainID ChainID) map[string]any {
	timestamp := presenceTestTimestamp * 1_000
	if s.staleChains[chainID] {
		timestamp = (presenceTestTimestamp - 3_600) * 1_000
	}
	return map[string]any{
		"blockNumber": strconv.FormatUint(presenceTestBlock, 10),
		"timestampMs": strconv.FormatUint(timestamp, 10),
	}
}

func (s *presenceTestServer) vaultRow(chainID ChainID) map[string]any {
	return map[string]any{
		"id":      morphoRowPrefix(chainID, presenceTestAccount) + strings.ToLower(presenceTestVault.Hex()),
		"chainId": int(chainID),
		"account": strings.ToLower(presenceTestAccount.Hex()),
		"vault":   strings.ToLower(presenceTestVault.Hex()),
		"version": "v1",
	}
}

func (s *presenceTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("content-type", "application/json")
	if request.Method == http.MethodGet {
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("8", s.chains, s.processedBlock()))
		return
	}
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		s.t.Errorf("decode GraphQL request: %v", err)
		return
	}
	owner := strings.ToLower(presenceTestAccount.Hex())
	if strings.Contains(body.Query, "query Presence") {
		s.probes.Add(1)
		query := body.Query
		s.lastProbeQuery.Store(&query)
		holds := strings.Contains(query, owner)
		if s.probeErrors {
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"errors": []map[string]any{{"message": "presence unavailable"}},
			})
			return
		}
		data := make(map[string]any)
		for _, chainID := range s.chains {
			data[presenceAlias("cp", chainID)] = []map[string]any{s.checkpoint(chainID)}
			data[presenceAlias("f0_", chainID)] = []map[string]any{}
			vaults := []map[string]any{}
			if chainID == Base && holds {
				vaults = append(vaults, map[string]any{"id": s.vaultRow(chainID)["id"]})
			}
			data[presenceAlias("f1_", chainID)] = vaults
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
		return
	}
	s.pages.Add(1)
	chainID := ChainID(body.Variables["chainId"].(float64))
	s.pagedChains = append(s.pagedChains, chainID)
	vaults := []map[string]any{}
	if chainID == Base && body.Variables["account"] == owner {
		vaults = append(vaults, s.vaultRow(chainID))
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
		"indexerCheckpoints":       []map[string]any{s.checkpoint(chainID)},
		"morphoMarketPositionRefs": []map[string]any{},
		"morphoVaultPositionRefs":  vaults,
	}})
}

func newPresenceTestMorpho(t *testing.T, server *presenceTestServer) (*morphoIndexer, *httptest.Server) {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return &morphoIndexer{
		api: &sentioAPIClient{
			apiKey:     "test",
			httpClient: httpServer.Client(),
			statuses:   make(map[string]sentioStatusCache),
		},
		config: SentioIndexerConfig{
			GraphQLURL:       httpServer.URL + "/graphql",
			StatusURL:        httpServer.URL + "/status",
			ProcessorVersion: "8",
		},
		requiredChains: deploymentChains(morphoDeployments),
	}, httpServer
}

func presenceTestPinned(chains ...ChainID) map[ChainID]BlockRef {
	pinned := make(map[ChainID]BlockRef, len(chains))
	for _, chainID := range chains {
		pinned[chainID] = BlockRef{ChainID: chainID, Number: presenceTestBlock, Timestamp: presenceTestTimestamp}
	}
	return pinned
}

func presenceScanContext(pinned map[ChainID]BlockRef) context.Context {
	return withPinnedBlocks(withScan(context.Background(), nil, newIndexerLane(1)), pinned)
}

func TestPresenceProbeSkipsChainsProvenEmpty(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base, Arbitrum}
	server := &presenceTestServer{t: t, chains: chains}
	indexer, _ := newPresenceTestMorpho(t, server)
	pinned := presenceTestPinned(chains...)
	ctx := presenceScanContext(pinned)

	for _, chainID := range chains {
		refs, err := indexer.indexedRefs(ctx, pinned[chainID], presenceTestAccount, false)
		if err != nil {
			t.Fatalf("chain %d: %v", chainID, err)
		}
		if refs.IndexerBlock != presenceTestBlock {
			t.Fatalf("chain %d indexer block = %d, want %d", chainID, refs.IndexerBlock, presenceTestBlock)
		}
		wantVaults := 0
		if chainID == Base {
			wantVaults = 1
		}
		if len(refs.Vaults) != wantVaults || len(refs.MarketIDs) != 0 {
			t.Fatalf("chain %d refs = %+v, want %d vaults and no markets", chainID, refs, wantVaults)
		}
	}
	if server.probes.Load() != 1 {
		t.Fatalf("probes = %d, want exactly one for the scan", server.probes.Load())
	}
	if server.pages.Load() != 1 || len(server.pagedChains) != 1 || server.pagedChains[0] != Base {
		t.Fatalf("per-chain pages = %v, want only Base", server.pagedChains)
	}
	query := *server.lastProbeQuery.Load()
	for _, want := range []string{
		`cp1: indexerCheckpoints(first: 2, block: { number: 1000 }, where: { id: "1" })`,
		`f0_8453: morphoMarketPositionRefs(first: 1, orderBy: id, orderDirection: asc, block: { number: 1000 }, where: { chainId: 8453, account: "` + strings.ToLower(presenceTestAccount.Hex()) + `" }) { id }`,
		`f1_42161: morphoVaultPositionRefs(`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("probe query lacks %q:\n%s", want, query)
		}
	}
	if strings.Index(query, "cp1:") > strings.Index(query, "cp56:") || strings.Index(query, "cp56:") > strings.Index(query, "cp8453:") {
		t.Fatalf("probe query does not list chains in ascending order:\n%s", query)
	}
}

func TestPresenceProbeIsMemoizedPerAccount(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base}
	server := &presenceTestServer{t: t, chains: chains}
	indexer, _ := newPresenceTestMorpho(t, server)
	pinned := presenceTestPinned(chains...)
	ctx := presenceScanContext(pinned)
	other := common.HexToAddress("0x1111111111111111111111111111111111111111")
	for _, chainID := range chains {
		if _, err := indexer.indexedRefs(ctx, pinned[chainID], presenceTestAccount, false); err != nil {
			t.Fatal(err)
		}
		if _, err := indexer.indexedRefs(ctx, pinned[chainID], other, false); err != nil {
			t.Fatal(err)
		}
	}
	if server.probes.Load() != 2 {
		t.Fatalf("probes = %d, want one per account", server.probes.Load())
	}
}

func TestPresenceProbeFallsBackWhenItCannotProveAnything(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base}
	pinned := presenceTestPinned(chains...)
	for _, test := range []struct {
		name       string
		server     *presenceTestServer
		ctx        context.Context
		feeMarkets bool
		wantProbes int32
		wantPages  int32
	}{
		{
			name:      "no scan scope",
			server:    &presenceTestServer{t: t, chains: chains},
			ctx:       context.Background(),
			wantPages: 3,
		},
		{
			name:       "probe fails",
			server:     &presenceTestServer{t: t, chains: chains, probeErrors: true},
			ctx:        presenceScanContext(pinned),
			wantProbes: 1, wantPages: 3,
		},
		{
			name:      "too few chains",
			server:    &presenceTestServer{t: t, chains: chains},
			ctx:       presenceScanContext(presenceTestPinned(Ethereum, Base)),
			wantPages: 3,
		},
		{
			name:       "fee recipient",
			server:     &presenceTestServer{t: t, chains: chains},
			ctx:        presenceScanContext(pinned),
			feeMarkets: true,
			wantPages:  3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			indexer, _ := newPresenceTestMorpho(t, test.server)
			for _, chainID := range chains {
				if _, err := indexer.indexedRefs(test.ctx, pinned[chainID], presenceTestAccount, test.feeMarkets); err != nil {
					t.Fatalf("chain %d: %v", chainID, err)
				}
			}
			if test.server.probes.Load() != test.wantProbes || test.server.pages.Load() < test.wantPages {
				t.Fatalf(
					"probes = %d pages = %d, want %d probes and at least %d pages",
					test.server.probes.Load(), test.server.pages.Load(), test.wantProbes, test.wantPages,
				)
			}
		})
	}
}

func TestPresenceProbeDoesNotSkipAStaleChain(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base}
	server := &presenceTestServer{t: t, chains: chains, staleChains: map[ChainID]bool{BSC: true}}
	indexer, _ := newPresenceTestMorpho(t, server)
	pinned := presenceTestPinned(chains...)
	ctx := presenceScanContext(pinned)
	if _, err := indexer.indexedRefs(ctx, pinned[Ethereum], presenceTestAccount, false); err != nil {
		t.Fatal(err)
	}
	// The stale chain's page fails the same freshness check the probe applied, exactly as before.
	if _, err := indexer.indexedRefs(ctx, pinned[BSC], presenceTestAccount, false); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale chain error = %v, want the checkpoint staleness error", err)
	}
	if server.probes.Load() != 1 || len(server.pagedChains) != 1 || server.pagedChains[0] != BSC {
		t.Fatalf("probes = %d paged = %v, want the stale chain to reach its own page", server.probes.Load(), server.pagedChains)
	}
}

func TestPresenceTargetsExcludeChainsThePerChainFlowWouldReject(t *testing.T) {
	probe := presenceProbe{requiredChains: []ChainID{Ethereum, BSC, Base, Arbitrum}, maxRPCTail: 10}
	pinned := presenceTestPinned(Ethereum, BSC, Base)
	pinned[Base] = BlockRef{ChainID: Base, Number: presenceTestBlock + 100}
	statuses := map[ChainID]sentioChainStatus{
		Ethereum: {State: "PROCESSING_LATEST", ProcessedBlock: presenceTestBlock + 5},
		BSC:      {State: "CATCHING_UP", ProcessedBlock: presenceTestBlock},
		Base:     {State: "PROCESSING_LATEST", ProcessedBlock: presenceTestBlock},
		Arbitrum: {State: "PROCESSING_LATEST", ProcessedBlock: presenceTestBlock},
	}
	targets := presenceTargets(probe, pinned, statuses)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want only Ethereum", targets)
	}
	if target := targets[Ethereum]; target.queryBlock != presenceTestBlock {
		t.Fatalf("Ethereum query block = %d, want the pinned block %d", target.queryBlock, presenceTestBlock)
	}
}

func TestValidatePresenceCheckpointMatchesPageRules(t *testing.T) {
	probe := presenceProbe{liveMaxLag: 15 * time.Minute, backfillMaxLag: 7 * 24 * time.Hour}
	block := BlockRef{Number: 100, Timestamp: presenceTestTimestamp}
	if err := validatePresenceCheckpoint(probe, block, 100, presenceTestTimestamp*1_000); err != nil {
		t.Fatalf("fresh checkpoint rejected: %v", err)
	}
	if err := validatePresenceCheckpoint(probe, block, 101, presenceTestTimestamp*1_000); err == nil {
		t.Fatalf("checkpoint ahead of the pinned block accepted")
	}
	if err := validatePresenceCheckpoint(probe, block, 100, (presenceTestTimestamp-1_800)*1_000); err == nil {
		t.Fatalf("checkpoint 30 minutes stale accepted on a live scan")
	}
	fixed := BlockRef{Number: 100, Timestamp: presenceTestTimestamp, Fixed: true}
	if err := validatePresenceCheckpoint(probe, fixed, 100, (presenceTestTimestamp-1_800)*1_000); err != nil {
		t.Fatalf("checkpoint 30 minutes stale rejected on a fixed-block scan: %v", err)
	}
}

func TestPrefetchedPresenceIsReusedByTheChainJobs(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base, Arbitrum}
	server := &presenceTestServer{t: t, chains: chains}
	indexer, _ := newPresenceTestMorpho(t, server)
	pinned := presenceTestPinned(chains...)
	ctx := presenceScanContext(pinned)

	indexer.prefetchPresence(ctx, presenceTestAccount)
	// A second prefetch for the same account starts nothing new.
	indexer.prefetchPresence(ctx, presenceTestAccount)
	for _, chainID := range chains {
		refs, err := indexer.indexedRefs(ctx, pinned[chainID], presenceTestAccount, false)
		if err != nil {
			t.Fatalf("chain %d: %v", chainID, err)
		}
		if (chainID == Base) != (len(refs.Vaults) == 1) {
			t.Fatalf("chain %d refs = %+v", chainID, refs)
		}
	}
	if server.probes.Load() != 1 {
		t.Fatalf("probes = %d, want the one prefetched probe", server.probes.Load())
	}
	if len(server.pagedChains) != 1 || server.pagedChains[0] != Base {
		t.Fatalf("paged chains = %v, want only Base", server.pagedChains)
	}
	// Outside a scan there is nothing to prefetch into.
	indexer.prefetchPresence(context.Background(), presenceTestAccount)
	time.Sleep(10 * time.Millisecond)
	if server.probes.Load() != 1 {
		t.Fatalf("prefetch outside a scan ran a probe")
	}
}

// A proof established while the index stood at block N still holds after the index advances: the
// chain job then reports N as its indexed block so the RPC tail starts right after the proof.
func TestPresenceProofFromAnEarlierIndexedBlockStartsTheTailThere(t *testing.T) {
	chains := []ChainID{Ethereum, BSC, Base}
	server := &presenceTestServer{t: t, chains: chains}
	indexer, _ := newPresenceTestMorpho(t, server)
	pinned := make(map[ChainID]BlockRef, len(chains))
	for _, chainID := range chains {
		pinned[chainID] = BlockRef{ChainID: chainID, Number: presenceTestBlock + 10, Timestamp: presenceTestTimestamp}
	}
	ctx := presenceScanContext(pinned)

	// Probe at processed = 1000, then the index advances to 1010 before the chain job runs.
	indexer.prefetchPresence(ctx, presenceTestAccount)
	if _, err := indexer.indexedRefs(ctx, pinned[Base], presenceTestAccount, false); err != nil {
		t.Fatal(err)
	}
	server.processed.Store(presenceTestBlock + 10)
	indexer.api.statuses = make(map[string]sentioStatusCache)

	refs, err := indexer.indexedRefs(ctx, pinned[Ethereum], presenceTestAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	if refs.IndexerBlock != presenceTestBlock || len(refs.Vaults) != 0 {
		t.Fatalf("refs = %+v, want an empty result indexed at the proof block %d", refs, presenceTestBlock)
	}
	if len(server.pagedChains) != 1 || server.pagedChains[0] != Base {
		t.Fatalf("paged chains = %v, want the drifted Ethereum job to reuse the proof", server.pagedChains)
	}
}

// Uniswap has no RPC tail, so a proof from an earlier indexed block cannot stand in for the page.
//
// The scan context carries a lane of one slot, so this also guards the ordering that keeps a
// prefetch from deadlocking: the chain job waits for the probe before it takes the slot the probe
// needs.
func TestUniswapPresenceRequiresAProofAtTheQueryBlock(t *testing.T) {
	chains := deploymentChains(uniswapV4Deployments)
	var probes, pages atomic.Int32
	processed := atomic.Uint64{}
	processed.Store(presenceTestBlock)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("5", chains, processed.Load()))
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(request.Body).Decode(&body)
		if strings.Contains(body.Query, "query Presence") {
			probes.Add(1)
			data := make(map[string]any)
			for _, chainID := range chains {
				data[presenceAlias("cp", chainID)] = []map[string]any{{
					"blockNumber": strconv.FormatUint(processed.Load(), 10),
					"timestampMs": strconv.FormatUint(presenceTestTimestamp*1_000, 10),
				}}
				data[presenceAlias("f0_", chainID)] = []map[string]any{}
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": data})
			return
		}
		pages.Add(1)
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestResponse(processed.Load(), presenceTestTimestamp*1_000, nil))
	}))
	t.Cleanup(server.Close)
	indexer := newUniswapIndexerTestClient(server, "5")
	pinned := make(map[ChainID]BlockRef, len(chains))
	for _, chainID := range chains {
		pinned[chainID] = BlockRef{ChainID: chainID, Number: presenceTestBlock + 10, Timestamp: presenceTestTimestamp}
	}
	ctx := presenceScanContext(pinned)

	indexer.prefetchPresence(ctx, uniswapV4, presenceTestAccount)
	first, err := indexer.indexedNFTs(ctx, uniswapV4, pinned[chains[0]], presenceTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.NFTs) != 0 || first.CheckpointBlock != presenceTestBlock || pages.Load() != 0 {
		t.Fatalf("first chain = %+v pages=%d, want the proof to stand in for the page", first, pages.Load())
	}
	processed.Store(presenceTestBlock + 10)
	indexer.api.statuses = make(map[string]sentioStatusCache)
	second, err := indexer.indexedNFTs(ctx, uniswapV4, pinned[chains[1]], presenceTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 1 || second.CheckpointBlock != presenceTestBlock+10 {
		t.Fatalf("drifted chain = %+v pages=%d, want the page to run at the new indexed block", second, pages.Load())
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want the single prefetched probe", probes.Load())
	}
}

func TestEngineRegistersPresencePrefetchers(t *testing.T) {
	engine := NewEngineWithConfig(nil, nil, EngineConfig{SentioIndexers: map[string]SentioIndexerConfig{
		"morpho-blue": testSentioIndexerConfig("morpho"),
		"pendle":      testSentioIndexerConfig("pendle"),
		"uniswap-v3":  testSentioIndexerConfig("uniswap-v3"),
		"uniswap-v4":  testSentioIndexerConfig("uniswap-v4"),
	}})
	want := map[string]bool{"morpho-blue": true, "pendle": true, "uniswap-v3": true, "uniswap-v4": true}
	for _, adapter := range engine.adapters {
		_, prefetches := adapter.(presencePrefetcher)
		if prefetches != want[adapter.Info().ID] {
			t.Errorf("protocol %q prefetches presence = %v, want %v", adapter.Info().ID, prefetches, want[adapter.Info().ID])
		}
	}
}
