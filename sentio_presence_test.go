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
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("8", s.chains, presenceTestBlock))
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
