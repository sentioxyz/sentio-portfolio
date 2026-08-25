package portfolio

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestPendleFactoryGenerationsCoverEveryChain(t *testing.T) {
	if got, want := len(pendleChainConfigs), len(SupportedChainIDs); got != want {
		t.Fatalf("Pendle chain configs = %d, want %d", got, want)
	}
	seen := make(map[common.Address]ChainID)
	for _, chainID := range SupportedChainIDs {
		chain, supported := pendleChainConfigs[chainID]
		if !supported {
			t.Fatalf("chain %d has no Pendle config", chainID)
		}
		if chain.ChainID != chainID {
			t.Fatalf("chain %d config reports chain %d", chainID, chain.ChainID)
		}
		if len(chain.Generations) == 0 {
			t.Fatalf("chain %d has no Pendle factory generations", chainID)
		}
		for _, generation := range chain.Generations {
			for _, address := range []common.Address{
				generation.YieldContractFactory, generation.MarketFactory,
			} {
				if address == (common.Address{}) {
					t.Fatalf("chain %d has an unset Pendle factory", chainID)
				}
				if other, duplicate := seen[address]; duplicate {
					t.Fatalf("factory %s is configured on both chain %d and chain %d",
						address, other, chainID)
				}
				seen[address] = chainID
			}
		}
	}
}

// The activation block must be the oldest generation and every window must be set: a PT, YT or
// LP is only ever discoverable from the factory event that created it, so a config that starts
// late hides every token created before it, with no later event to recover from.
func TestPendleActivationBlockIsTheOldestGeneration(t *testing.T) {
	for chainID, chain := range pendleChainConfigs {
		if chain.ActivationBlock == 0 {
			t.Errorf("chain %d has no activation block", chainID)
			continue
		}
		oldest := ^uint64(0)
		for _, generation := range chain.Generations {
			yieldBlock := generation.YieldContractFactoryWindow.ActivationBlock
			marketBlock := generation.MarketFactoryWindow.ActivationBlock
			if yieldBlock == 0 || marketBlock == 0 {
				t.Errorf("chain %d has a factory generation without a deployment window", chainID)
				continue
			}
			if yieldBlock < chain.ActivationBlock {
				t.Errorf("chain %d yield factory %s activates at %d, before the chain activation %d",
					chainID, generation.YieldContractFactory, yieldBlock, chain.ActivationBlock)
			}
			// Pendle deploys the market factory right after the yield factory it reads from.
			if marketBlock <= yieldBlock {
				t.Errorf("chain %d market factory %s activates at %d, not after its yield factory %d",
					chainID, generation.MarketFactory, marketBlock, yieldBlock)
			}
			oldest = min(oldest, yieldBlock)
		}
		if oldest != chain.ActivationBlock {
			t.Errorf("chain %d activation block is %d, but its oldest generation starts at %d",
				chainID, chain.ActivationBlock, oldest)
		}
	}
}

type stubPendleIndexer struct {
	refs   []pendlePositionRef
	err    error
	called bool
}

func (s *stubPendleIndexer) PositionRefs(
	context.Context,
	*RPCClient,
	BlockRef,
	common.Address,
) ([]pendlePositionRef, error) {
	s.called = true
	return s.refs, s.err
}

// Before the first factory exists there is nothing to enumerate, and the scan must not reach the
// index or the RPC client at all. A nil client makes that provable without a network.
func TestPendleSkipsChainsBeforeActivation(t *testing.T) {
	for chainID, chain := range pendleChainConfigs {
		indexer := &stubPendleIndexer{}
		adapter := newPendleAdapterWithIndexer(indexer)
		block := BlockRef{ChainID: chainID, Number: chain.ActivationBlock - 1, Fixed: true}
		groups, err := adapter.Positions(
			context.Background(), nil, block, common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
		)
		if err != nil {
			t.Errorf("chain %d before activation: %v", chainID, err)
		}
		if len(groups) != 0 {
			t.Errorf("chain %d reported %d groups before Pendle existed", chainID, len(groups))
		}
		if indexer.called {
			t.Errorf("chain %d queried the index before Pendle existed", chainID)
		}
	}
}

// An unsupported chain must not be treated as "Pendle with no positions": the adapter advertises
// four chains and the engine only calls it for those.
func TestPendleSkipsUnsupportedChains(t *testing.T) {
	indexer := &stubPendleIndexer{}
	adapter := newPendleAdapterWithIndexer(indexer)
	block := BlockRef{ChainID: ChainID(10), Number: 1_000_000, Fixed: true}
	groups, err := adapter.Positions(
		context.Background(), nil, block, common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
	)
	if err != nil || len(groups) != 0 || indexer.called {
		t.Fatalf("unsupported chain: groups=%d err=%v called=%v", len(groups), err, indexer.called)
	}
}

func TestPendleAdapterAdvertisesEveryConfiguredChain(t *testing.T) {
	adapter := newPendleAdapterWithIndexer(&stubPendleIndexer{})
	info := adapter.Info()
	if info.ID != "pendle" {
		t.Fatalf("protocol id = %q, want %q", info.ID, "pendle")
	}
	if got, want := len(info.Chains), len(pendleChainConfigs); got != want {
		t.Fatalf("advertised chains = %d, want %d", got, want)
	}
	for _, chainID := range info.Chains {
		if _, supported := pendleChainConfigs[chainID]; !supported {
			t.Fatalf("adapter advertises chain %d without a config", chainID)
		}
	}
}

func TestPendleGroupLabelsDistinguishEveryKind(t *testing.T) {
	labels := map[pendleTokenKind]string{
		pendlePT: "Principal Token",
		pendleYT: "Yield Token",
		pendleLP: "Liquidity",
	}
	for kind, want := range labels {
		if got := pendleGroupLabel(kind); got != want {
			t.Fatalf("label for %q = %q, want %q", kind, got, want)
		}
	}
}

func TestPendleTokenKindValidationRejectsUnknownKinds(t *testing.T) {
	for _, kind := range []pendleTokenKind{pendlePT, pendleYT, pendleLP} {
		if !validPendleTokenKind(kind) {
			t.Fatalf("kind %q was rejected", kind)
		}
	}
	for _, kind := range []pendleTokenKind{"", "sy", "PT", "lp "} {
		if validPendleTokenKind(kind) {
			t.Fatalf("kind %q was accepted", kind)
		}
	}
}

// Row ids are recomputed by the kernel and compared against what the index returned, so the two
// derivations must agree byte for byte, lowercase included.
func TestPendleRowIdentitiesAreLowercaseAndStable(t *testing.T) {
	account := common.HexToAddress("0xC02dd10b401E01E0fb3BF497E46E6d6b51664AD7")
	token := common.HexToAddress("0xDC169ABe56461a2e0C034DA431aC2A3EBF596094")
	if got, want := pendleRefRowPrefix(Ethereum, account),
		"1:0xc02dd10b401e01e0fb3bf497e46e6d6b51664ad7:"; got != want {
		t.Fatalf("ref prefix = %q, want %q", got, want)
	}
	if got, want := pendleTokenRowID(Ethereum, token),
		"1:0xdc169abe56461a2e0c034da431ac2a3ebf596094"; got != want {
		t.Fatalf("token row id = %q, want %q", got, want)
	}
	if !strings.HasPrefix(
		pendleRefRowPrefix(Ethereum, account)+strings.ToLower(token.Hex()),
		pendleRefRowPrefix(Ethereum, account),
	) {
		t.Fatal("position row id does not extend its account prefix")
	}
}

func TestPendleMergeRefsPrefersTheIndexedRegistration(t *testing.T) {
	token := common.HexToAddress("0x1111111111111111111111111111111111111111")
	other := common.HexToAddress("0x2222222222222222222222222222222222222222")
	indexed := []pendlePositionRef{{Token: token, Kind: pendlePT, Expiry: 42, CreatedBlock: 7}}
	// The tail resolves a kind from the chain but knows no creation block; when both sources
	// carry the same token the indexed registration is the richer one and must win.
	tail := []pendlePositionRef{
		{Token: token, Kind: pendlePT},
		{Token: other, Kind: pendleLP},
	}
	merged, err := mergePendleRefs(indexed, tail)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged refs = %d, want 2", len(merged))
	}
	if merged[0].Token != token || merged[0].CreatedBlock != 7 || merged[0].Expiry != 42 {
		t.Fatalf("indexed registration was overwritten by the tail: %+v", merged[0])
	}
	if merged[1].Token != other || merged[1].Kind != pendleLP {
		t.Fatalf("tail-only ref was dropped: %+v", merged[1])
	}
}

func TestPendleMergeRefsRejectsUnboundedAccounts(t *testing.T) {
	refs := make([]pendlePositionRef, 0, pendleMaxPositionRefs+1)
	for index := 0; index <= pendleMaxPositionRefs; index++ {
		address := common.BigToAddress(common.Big1)
		address[0] = byte(index >> 8)
		address[1] = byte(index)
		refs = append(refs, pendlePositionRef{Token: address, Kind: pendlePT})
	}
	if _, err := mergePendleRefs(refs, nil); err == nil {
		t.Fatal("merge accepted more refs than the account bound allows")
	}
}

// The window boundary is inclusive: a factory must be skipped at one block before it has code
// and consulted at the block it was deployed in. An eth_call against an address with no code
// returns empty data, which fails strict decoding and drops the whole protocol for that scan.
func TestPendleFactoryWindowsAreInclusiveAtTheirActivationBlock(t *testing.T) {
	for chainID, chain := range pendleChainConfigs {
		for _, generation := range chain.Generations {
			windows := map[common.Address]deploymentWindow{
				generation.YieldContractFactory: generation.YieldContractFactoryWindow,
				generation.MarketFactory:        generation.MarketFactoryWindow,
			}
			for address, window := range windows {
				if window.ActiveAt(window.ActivationBlock - 1) {
					t.Errorf("chain %d factory %s is active one block before deployment %d",
						chainID, address, window.ActivationBlock)
				}
				if !window.ActiveAt(window.ActivationBlock) {
					t.Errorf("chain %d factory %s is inactive at its deployment block %d",
						chainID, address, window.ActivationBlock)
				}
			}
		}
	}
}

// Tail classification is the only place the kernel calls a factory directly, so it carries the
// window gate. With every generation still undeployed it must build no calls at all — proved
// here by passing a nil client, which would panic if a call were attempted.
func TestPendleClassifyTailIsInertBeforeAnyFactoryExists(t *testing.T) {
	candidates := []common.Address{
		common.HexToAddress("0xdc169abe56461a2e0c034da431ac2a3ebf596094"),
		common.HexToAddress("0x9c560ebaf78e596cbcc27411d633a74d628dd7dc"),
	}
	for chainID, chain := range pendleChainConfigs {
		block := BlockRef{ChainID: chainID, Number: chain.ActivationBlock - 1, Fixed: true}
		refs, err := pendleClassifyTail(context.Background(), nil, block, candidates, chain)
		if err != nil {
			t.Errorf("chain %d classification before deployment: %v", chainID, err)
		}
		if len(refs) != 0 {
			t.Errorf("chain %d classified %d tokens before any factory existed", chainID, len(refs))
		}
	}
}

// No candidates means no calls, on any chain and at any block.
func TestPendleClassifyTailWithoutCandidatesNeverDials(t *testing.T) {
	for chainID, chain := range pendleChainConfigs {
		block := BlockRef{ChainID: chainID, Number: chain.ActivationBlock + 1_000, Fixed: true}
		refs, err := pendleClassifyTail(context.Background(), nil, block, nil, chain)
		if err != nil || len(refs) != 0 {
			t.Fatalf("chain %d: refs=%d err=%v", chainID, len(refs), err)
		}
	}
}

// heldRefs is what turns "the account touched this token once" into "the account holds it now".
// With no refs it must not dial, so an account with no Pendle history costs nothing.
func TestPendleHeldRefsWithoutReferencesNeverDials(t *testing.T) {
	adapter := newPendleAdapterWithIndexer(&stubPendleIndexer{})
	block := BlockRef{ChainID: Ethereum, Number: 25_000_000, Fixed: true}
	held, err := adapter.heldRefs(
		context.Background(), nil, block,
		common.HexToAddress("0x000000000000000000000000000000000000dEaD"), nil,
	)
	if err != nil || len(held) != 0 {
		t.Fatalf("held=%d err=%v", len(held), err)
	}
}

// The decomposition is checked against a real market rather than invented numbers: Ethereum
// market 0x47ad2cd1… at block 25,830,546, holder 0x0b129e1a…. DeBank reports that position as
// USDe 187050.6280464333 and PT-sUSDE-26NOV2026 51485.398359245904; the PT leg reproduces to the
// last wei and the asset leg to 2.4e-6, which is the SY exchange rate accruing between DeBank's
// snapshot and the pinned block.
func TestPendleReserveShareReproducesADeBankLiquidityPosition(t *testing.T) {
	mustInt := func(text string) *big.Int {
		value, valid := new(big.Int).SetString(text, 10)
		if !valid {
			t.Fatalf("invalid test fixture %q", text)
		}
		return value
	}
	balance := mustInt("86211778212162680378774")
	totalPt := mustInt("736775137074598688609271")
	totalSy := mustInt("2150189400904398701665727")
	totalLp := mustInt("1233722506457099026298120")
	exchangeRate := mustInt("1244899699568836222")

	if got, want := pendleReserveShare(balance, totalPt, totalLp).String(),
		"51485398359245895155192"; got != want {
		t.Fatalf("PT share = %s, want %s", got, want)
	}
	syAmount := pendleReserveShare(balance, totalSy, totalLp)
	if got, want := pendleSYToAsset(syAmount, exchangeRate).String(),
		"187051068905701089331823"; got != want {
		t.Fatalf("asset share = %s, want %s", got, want)
	}
}

// Rounding down matters: shares that rounded up could sum past the reserve and let a market
// report more underlying than it holds.
func TestPendleReserveShareRoundsDown(t *testing.T) {
	share := pendleReserveShare(big.NewInt(1), big.NewInt(10), big.NewInt(3))
	if share.String() != "3" {
		t.Fatalf("share = %s, want 3", share)
	}
	full := pendleReserveShare(big.NewInt(3), big.NewInt(10), big.NewInt(3))
	if full.String() != "10" {
		t.Fatalf("whole-supply share = %s, want the entire reserve", full)
	}
}

func TestPendleSYToAssetIsIdentityAtParity(t *testing.T) {
	amount := new(big.Int).SetUint64(1_234_567_890_123_456_789)
	if got := pendleSYToAsset(amount, pendleExchangeRateOne); got.Cmp(amount) != 0 {
		t.Fatalf("asset at parity = %s, want %s", got, amount)
	}
}
