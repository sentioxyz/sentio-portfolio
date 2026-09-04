package portfolio

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestPendleFactoryGenerationsCoverEveryChain(t *testing.T) {
	if got, want := len(pendleChainConfigs), 7; got != want {
		t.Fatalf("Pendle chain configs = %d, want %d", got, want)
	}
	for _, chainID := range deploymentChains(pendleChainConfigs) {
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
		seen := make(map[common.Address]struct{})
		for _, generation := range chain.Generations {
			for _, address := range []common.Address{
				generation.YieldContractFactory, generation.MarketFactory,
			} {
				if address == (common.Address{}) {
					t.Fatalf("chain %d has an unset Pendle factory", chainID)
				}
				if _, duplicate := seen[address]; duplicate {
					t.Fatalf("factory %s is configured twice on chain %d", address, chainID)
				}
				seen[address] = struct{}{}
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
	refs          []pendlePositionRef
	markets       map[common.Address][]common.Address
	err           error
	called        bool
	marketsCalled bool
	marketsFor    []common.Address
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

func (s *stubPendleIndexer) MarketsForPT(
	_ context.Context,
	_ BlockRef,
	pts []common.Address,
) (map[common.Address][]common.Address, error) {
	s.marketsCalled = true
	s.marketsFor = pts
	return s.markets, s.err
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
// only configured chains and the engine only calls it for those.
func TestPendleSkipsUnsupportedChains(t *testing.T) {
	unsupported := ChainID(999_999)
	indexer := &stubPendleIndexer{}
	adapter := newPendleAdapterWithIndexer(indexer)
	for _, chainID := range adapter.Info().Chains {
		if chainID == unsupported {
			t.Fatalf("sentinel chain %d unexpectedly became supported", unsupported)
		}
	}
	block := BlockRef{ChainID: unsupported, Number: 1_000_000, Fixed: true}
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

// The discount is pinned to live Ethereum state rather than to a round number, because the whole
// claim being tested is that Pendle's own two published numbers reproduce an external valuation.
//
// PT-sUSDS-26NOV2026 (0xdc169abe…) through its market 0x9c560eba… at head 25882782:
// lastLnImpliedRate 47125524316414759 and expiry 1795651200 against a block timestamp of
// 1788269927 — 85.43 days out — give 0.989030473175155466 of the accounting asset, USDS.
//
// DeBank valued the same PT at 1628138 USD against a holding of 1648554.788087306450847013,
// implying 0.98761534. The 0.14% gap is the two snapshots not being the same instant (a PT
// converges to par as expiry approaches), DeBank quoting a time-averaged rate rather than the
// spot one, and USDS not being exactly a dollar. Reproducing that band is the point; matching it
// to the last digit would mean this test had reimplemented DeBank rather than Pendle.
func TestPendlePTToAssetRatioReproducesTheLiveMarketDiscount(t *testing.T) {
	rate, ok := new(big.Int).SetString("47125524316414759", 10)
	if !ok {
		t.Fatal("invalid implied rate fixture")
	}
	ratio, err := pendlePTToAssetRatio(rate, 1_795_651_200, 1_788_269_927)
	if err != nil {
		t.Fatal(err)
	}
	if ratio.String() != "989030473175155466" {
		t.Fatalf("PT ratio = %s, want 989030473175155466", ratio)
	}
	if ratio.Cmp(priceBasisRatioOne) >= 0 {
		t.Fatal("an unexpired PT must be worth less than its redemption asset")
	}
	ours, _ := new(big.Rat).SetFrac(ratio, priceBasisRatioOne).Float64()
	debank := 1628138.0 / 1648554.788087306450847013
	if relative := math.Abs(ours-debank) / debank; relative > 0.005 {
		t.Fatalf("ours = %.9f, DeBank = %.9f, relative difference %.6f", ours, debank, relative)
	}
}

// From expiry onward a PT redeems for its asset one for one, so the ratio is parity and the
// market's last implied rate is not consulted at all — a nil rate must still price.
func TestPendlePTToAssetRatioIsParityFromExpiryOnward(t *testing.T) {
	for _, timestamp := range []uint64{1_795_651_200, 1_795_651_201, 2_000_000_000} {
		ratio, err := pendlePTToAssetRatio(nil, 1_795_651_200, timestamp)
		if err != nil {
			t.Fatalf("timestamp %d: %v", timestamp, err)
		}
		if ratio.Cmp(priceBasisRatioOne) != 0 {
			t.Fatalf("ratio at %d = %s, want parity", timestamp, ratio)
		}
	}
}

// Every input the discount needs is either present or the PT stays unpriced. A guess here would
// be a wrong USD number that nothing in the response marks as derived from nothing.
func TestPendlePTToAssetRatioRefusesToGuess(t *testing.T) {
	rate := big.NewInt(47_125_524_316_414_759)
	if _, err := pendlePTToAssetRatio(rate, 0, 1_788_269_927); err == nil {
		t.Fatal("a PT without an expiry was priced")
	}
	if _, err := pendlePTToAssetRatio(nil, 1_795_651_200, 1_788_269_927); err == nil {
		t.Fatal("an unexpired PT was priced without a market rate")
	}
	if _, err := pendlePTToAssetRatio(
		big.NewInt(-1), 1_795_651_200, 1_788_269_927,
	); err == nil {
		t.Fatal("a negative implied rate was accepted")
	}
}

// One PT plus one YT redeems for one unit of the asset before expiry, so the YT is exactly the
// remainder. At parity it is zero, which this adapter reports as "unknown" rather than "nothing":
// an expired YT can still hold unredeemed accrued interest that nothing here reads.
func TestPendleYTToAssetRatioIsWhatThePTDiscountLeaves(t *testing.T) {
	ptRatio, ok := new(big.Int).SetString("989030473175155466", 10)
	if !ok {
		t.Fatal("invalid PT ratio fixture")
	}
	ytRatio, usable := pendleYTToAssetRatio(ptRatio)
	if !usable {
		t.Fatal("an unexpired YT must be priceable")
	}
	if ytRatio.String() != "10969526824844534" {
		t.Fatalf("YT ratio = %s, want 10969526824844534", ytRatio)
	}
	if sum := new(big.Int).Add(ptRatio, ytRatio); sum.Cmp(priceBasisRatioOne) != 0 {
		t.Fatalf("PT + YT = %s, want exactly one unit of the asset", sum)
	}
	if _, usable := pendleYTToAssetRatio(priceBasisRatioOne); usable {
		t.Fatal("an expired YT must stay unpriced rather than report zero")
	}
	if _, usable := pendleYTToAssetRatio(nil); usable {
		t.Fatal("a YT without a PT ratio must stay unpriced")
	}
}

// pendleStubMarket is one market's readState answer. lnImpliedRate and expiry are what discount
// the PT; the reserves only decide which market wins when a PT has several.
type pendleStubMarket struct {
	totalPt       *big.Int
	totalSy       *big.Int
	totalLp       *big.Int
	expiry        uint64
	lnImpliedRate *big.Int
	revert        bool
}

type pendleStubRPC struct {
	t              *testing.T
	chainID        ChainID
	balances       map[common.Address]*big.Int
	markets        map[common.Address]pendleStubMarket
	marketTokens   map[common.Address][3]common.Address
	exchangeRates  map[common.Address]*big.Int
	yieldTokens    map[common.Address]common.Address
	previewRates   map[common.Address]*big.Int
	ytInterests    map[common.Address]*big.Int
	ytRewards      map[common.Address][]*big.Int
	ytRewardTokens map[common.Address][]common.Address
	rewardReverts  map[common.Address]bool
	rewardCalls    map[common.Address]int
	pyIndices      map[common.Address]*big.Int
	siblingYT      map[common.Address]common.Address
	assets         map[common.Address]common.Address
	symbols        map[common.Address]string
	decimals       map[common.Address]uint8
	stateCalls     map[common.Address]int
}

func (s *pendleStubRPC) dispatch(to common.Address, data []byte) (string, map[string]any) {
	fail := map[string]any{"code": -32000, "message": "execution reverted"}
	if len(data) < 4 {
		return "", fail
	}
	selector := string(data[:4])
	switch selector {
	case string(erc20ABI.Methods["symbol"].ID):
		if to == (common.Address{}) {
			return "", fail
		}
		symbol, known := s.symbols[to]
		if !known {
			return "", fail
		}
		out, _ := erc20ABI.Methods["symbol"].Outputs.Pack(symbol)
		return "0x" + common.Bytes2Hex(out), nil
	case string(erc20ABI.Methods["decimals"].ID):
		if to == (common.Address{}) {
			return "", fail
		}
		decimals, known := s.decimals[to]
		if !known {
			return "", fail
		}
		out, _ := erc20ABI.Methods["decimals"].Outputs.Pack(decimals)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendlePrincipalTokenABI.Methods["balanceOf"].ID):
		balance, known := s.balances[to]
		if !known {
			return "", fail
		}
		out, _ := pendlePrincipalTokenABI.Methods["balanceOf"].Outputs.Pack(balance)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleMarketABI.Methods["readState"].ID):
		market, known := s.markets[to]
		s.stateCalls[to]++
		if !known || market.revert {
			return "", fail
		}
		out, _ := pendleMarketABI.Methods["readState"].Outputs.Pack(
			market.totalPt, market.totalSy, market.totalLp, common.Address{},
			big.NewInt(0), new(big.Int).SetUint64(market.expiry), big.NewInt(0),
			big.NewInt(0), market.lnImpliedRate,
		)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendlePrincipalTokenABI.Methods["YT"].ID):
		yieldToken, known := s.siblingYT[to]
		if !known {
			return "", fail
		}
		out, _ := pendlePrincipalTokenABI.Methods["YT"].Outputs.Pack(yieldToken)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleYieldTokenABI.Methods["pyIndexStored"].ID):
		stored, known := s.pyIndices[to]
		if !known {
			return "", fail
		}
		out, _ := pendleYieldTokenABI.Methods["pyIndexStored"].Outputs.Pack(stored)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleMarketABI.Methods["readTokens"].ID):
		tokens, known := s.marketTokens[to]
		if !known {
			return "", fail
		}
		out, _ := pendleMarketABI.Methods["readTokens"].Outputs.Pack(
			tokens[0], tokens[1], tokens[2],
		)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleMarketABI.Methods["getRewardTokens"].ID):
		if s.rewardReverts[to] {
			return "", fail
		}
		rewardTokens := s.ytRewardTokens[to]
		out, _ := pendleMarketABI.Methods["getRewardTokens"].Outputs.Pack(rewardTokens)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleYieldTokenABI.Methods["redeemDueInterestAndRewards"].ID):
		interest := s.ytInterests[to]
		if interest == nil {
			interest = new(big.Int)
		}
		rewards := s.ytRewards[to]
		if rewards == nil {
			rewards = []*big.Int{}
		}
		out, _ := pendleYieldTokenABI.Methods["redeemDueInterestAndRewards"].Outputs.Pack(
			interest, rewards,
		)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleMarketABI.Methods["redeemRewards"].ID):
		s.rewardCalls[to]++
		rewards := s.ytRewards[to]
		if rewards == nil {
			rewards = []*big.Int{}
		}
		out, _ := pendleMarketABI.Methods["redeemRewards"].Outputs.Pack(rewards)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["exchangeRate"].ID):
		rate, known := s.exchangeRates[to]
		if !known {
			return "", fail
		}
		out, _ := pendleStandardizedYieldABI.Methods["exchangeRate"].Outputs.Pack(rate)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["yieldToken"].ID):
		yieldToken, known := s.yieldTokens[to]
		if !known {
			return "", fail
		}
		out, _ := pendleStandardizedYieldABI.Methods["yieldToken"].Outputs.Pack(yieldToken)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["previewRedeem"].ID):
		values, err := pendleStandardizedYieldABI.Methods["previewRedeem"].Inputs.Unpack(data[4:])
		if err != nil || len(values) != 2 {
			return "", fail
		}
		tokenOut, tokenOK := values[0].(common.Address)
		shares, sharesOK := values[1].(*big.Int)
		yieldToken, tokenKnown := s.yieldTokens[to]
		rate, rateKnown := s.previewRates[to]
		if !tokenOK || !sharesOK || !tokenKnown || !rateKnown || tokenOut != yieldToken {
			return "", fail
		}
		amount := new(big.Int).Div(new(big.Int).Mul(shares, rate), pendleExchangeRateOne)
		out, _ := pendleStandardizedYieldABI.Methods["previewRedeem"].Outputs.Pack(amount)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["assetInfo"].ID):
		asset, known := s.assets[to]
		if !known {
			return "", fail
		}
		decimals, known := s.decimals[asset]
		if !known {
			return "", fail
		}
		out, _ := pendleStandardizedYieldABI.Methods["assetInfo"].Outputs.Pack(
			uint8(0), asset, decimals,
		)
		return "0x" + common.Bytes2Hex(out), nil
	}
	return "", fail
}

func (s *pendleStubRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		s.t.Errorf("read RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	answer := func(call rpcTestRequest) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		switch call.Method {
		case "eth_chainId":
			response["result"] = "0x" + strconv.FormatUint(uint64(s.chainID), 16)
			return response
		case "eth_call":
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
			return response
		}
		var input struct {
			To   common.Address `json:"to"`
			Data string         `json:"data"`
		}
		if unmarshalErr := json.Unmarshal(call.Params[0], &input); unmarshalErr != nil {
			s.t.Errorf("decode eth_call input: %v", unmarshalErr)
			response["error"] = map[string]any{"code": -32602, "message": "bad input"}
			return response
		}
		result, failure := s.dispatch(input.To, common.FromHex(input.Data))
		if failure != nil {
			response["error"] = failure
		} else {
			response["result"] = result
		}
		return response
	}
	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if unmarshalErr := json.Unmarshal(raw, &call); unmarshalErr != nil {
			s.t.Errorf("decode singleton RPC call: %v", unmarshalErr)
			return
		}
		_ = json.NewEncoder(writer).Encode(answer(call))
		return
	}
	var calls []rpcTestRequest
	if unmarshalErr := json.Unmarshal(raw, &calls); unmarshalErr != nil {
		s.t.Errorf("decode RPC batch: %v", unmarshalErr)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

// pendlePricingFixture is the live PT-sUSDS-26NOV2026 position, reduced to the contracts the
// direct-holding path touches.
type pendlePricingFixture struct {
	principal common.Address
	yieldT    common.Address
	sy        common.Address
	asset     common.Address
	market    common.Address
	block     BlockRef
	account   common.Address
	stub      *pendleStubRPC
}

func newPendlePricingFixture(t *testing.T) pendlePricingFixture {
	t.Helper()
	fixture := pendlePricingFixture{
		principal: common.HexToAddress("0xdc169abe56461a2e0c034da431ac2a3ebf596094"),
		yieldT:    common.HexToAddress("0x00000000000000000000000000000000000000a1"),
		sy:        common.HexToAddress("0xbe3d4ec488a0a042bb86f9176c24f8cd54018ba7"),
		asset:     common.HexToAddress("0xdC035D45d973E3EC169d2276DDab16f1e407384F"),
		market:    common.HexToAddress("0x9c560ebaf78e596cbcc27411d633a74d628dd7dc"),
		account:   common.HexToAddress("0xc02dd10b401e01e0fb3bf497e46e6d6b51664ad7"),
		block: BlockRef{
			ChainID: Ethereum, Number: 25_882_782, Timestamp: 1_788_269_927, Fixed: true,
		},
	}
	balance, ok := new(big.Int).SetString("1648554788087306450847013", 10)
	if !ok {
		t.Fatal("invalid balance fixture")
	}
	fixture.stub = &pendleStubRPC{
		t: t, chainID: fixture.block.ChainID,
		balances: map[common.Address]*big.Int{
			fixture.principal: balance,
			fixture.yieldT:    balance,
		},
		markets: map[common.Address]pendleStubMarket{
			fixture.market: {
				totalPt:       big.NewInt(743_742),
				totalSy:       big.NewInt(2_468_053),
				totalLp:       big.NewInt(1_115_654),
				expiry:        1_795_651_200,
				lnImpliedRate: big.NewInt(47_125_524_316_414_759),
			},
		},
		marketTokens: map[common.Address][3]common.Address{
			fixture.market: {fixture.sy, fixture.principal, fixture.yieldT},
		},
		exchangeRates:  map[common.Address]*big.Int{fixture.sy: pendleExchangeRateOne},
		yieldTokens:    map[common.Address]common.Address{fixture.sy: fixture.asset},
		previewRates:   map[common.Address]*big.Int{fixture.sy: pendleExchangeRateOne},
		ytInterests:    map[common.Address]*big.Int{fixture.yieldT: new(big.Int)},
		ytRewards:      map[common.Address][]*big.Int{fixture.yieldT: {}},
		ytRewardTokens: map[common.Address][]common.Address{fixture.yieldT: {}},
		rewardReverts:  map[common.Address]bool{},
		rewardCalls:    map[common.Address]int{},
		// A solvent SY: the exchange rate has kept up with the index the pair ratcheted to, so
		// the solvency factor is one and the discount is the implied rate alone.
		pyIndices:  map[common.Address]*big.Int{fixture.yieldT: pendleExchangeRateOne},
		siblingYT:  map[common.Address]common.Address{fixture.principal: fixture.yieldT},
		assets:     map[common.Address]common.Address{fixture.sy: fixture.asset},
		symbols:    map[common.Address]string{},
		decimals:   map[common.Address]uint8{},
		stateCalls: map[common.Address]int{},
	}
	for address, symbol := range map[common.Address]string{
		fixture.principal: "PT-sUSDS-26NOV2026",
		fixture.yieldT:    "YT-sUSDS-26NOV2026",
		fixture.asset:     "USDS",
		fixture.sy:        "SY-sUSDS",
		fixture.market:    "PENDLE-LPT",
	} {
		fixture.stub.symbols[address] = symbol
		fixture.stub.decimals[address] = 18
	}
	return fixture
}

func (f pendlePricingFixture) run(t *testing.T, indexer *stubPendleIndexer) []Group {
	t.Helper()
	server := httptest.NewServer(f.stub)
	t.Cleanup(server.Close)
	client, err := DialRPC(t.Context(), f.block.ChainID, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	groups, err := newPendleAdapterWithIndexer(indexer).Positions(
		t.Context(), client, f.block, f.account,
	)
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func (f pendlePricingFixture) onlyComponent(t *testing.T, groups []Group) Component {
	t.Helper()
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one group with one component", groups)
	}
	return groups[0].Components[0]
}

// The holding stays reported as the PT the account actually owns, at the balance the chain
// reports, and only its valuation is redirected. Converting the amount and reporting USDS
// instead would name an asset the account does not hold and would key a DeBank comparison on a
// different token than DeBank uses.
func TestPendleDirectPTPricesThroughItsAccountingAsset(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	component := fixture.onlyComponent(t, fixture.run(t, indexer))

	if component.Token.Address != fixture.principal {
		t.Fatalf("component token = %s, want the PT itself", component.Token.Address)
	}
	if component.AmountRaw != "1648554788087306450847013" {
		t.Fatalf("component amount = %s", component.AmountRaw)
	}
	if component.PriceBasis == nil {
		t.Fatal("the PT was left unpriced")
	}
	if component.PriceBasis.Token.Address != fixture.asset {
		t.Fatalf("basis token = %s, want USDS", component.PriceBasis.Token.Address)
	}
	if component.PriceBasis.Token.Symbol != "USDS" {
		t.Fatalf("basis symbol = %q, want USDS", component.PriceBasis.Token.Symbol)
	}
	if component.PriceBasis.RatioRaw != "989030473175155466" {
		t.Fatalf("basis ratio = %s", component.PriceBasis.RatioRaw)
	}
}

// A YT is priced as the remainder of its sibling PT's discount, so it reaches the same accounting
// asset without a YT quote existing anywhere.
func TestPendleDirectYTPricesAsTheComplementOfItsPT(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	component := fixture.onlyComponent(t, fixture.run(t, indexer))

	if component.Token.Address != fixture.yieldT {
		t.Fatalf("component token = %s, want the YT itself", component.Token.Address)
	}
	if component.PriceBasis == nil {
		t.Fatal("the YT was left unpriced")
	}
	if component.PriceBasis.RatioRaw != "10969526824844534" {
		t.Fatalf("basis ratio = %s", component.PriceBasis.RatioRaw)
	}
}

// Plasma account 0x4271...8bbb retained 15 raw units of YT-syrupUSDT-29JAN2026
// after redeeming 1,209,607,250,000 of its prior 1,209,607,250,015 raw balance. At
// block 31,602,716 the six-decimal token was already expired and had no claimable interest or
// rewards, but balanceOf still returned 15. A portfolio inventory must preserve that direct
// holding instead of applying a display-value or dust threshold borrowed from an external oracle.
func TestPendleExpiredDirectYTDustBalanceIsNotFiltered(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	dustYT := common.HexToAddress("0x44920a786043f663693d2ea1c7de8551d90b3213")
	dustPT := common.HexToAddress("0x8dfb9a39dfab16bffe77f15544b5bf03e377e419")
	dustSY := common.HexToAddress("0xd8b49fba7054a6ee2c4bd6813cdd6064430db85c")
	dustAsset := common.HexToAddress("0xb8ce59fc3717ada4c02eadf9682a9e934f625ebb")
	dustYieldToken := common.HexToAddress("0xc4374775489cb9c56003bf2c9b12495fc64f0771")
	account := common.HexToAddress("0x42715ba91deda3c692b9f540cee2fbb4dae78bbb")
	const expiry = uint64(1_769_644_800)
	fixture.block = BlockRef{
		ChainID: Plasma, Number: 31_602_716, Timestamp: 1_788_543_283, Fixed: true,
	}
	fixture.account = account
	fixture.stub.chainID = Plasma
	fixture.stub.balances[dustYT] = big.NewInt(15)
	fixture.stub.symbols[dustYT] = "YT-syrupUSDT-29JAN2026"
	fixture.stub.decimals[dustYT] = 6
	fixture.stub.decimals[dustAsset] = 6
	fixture.stub.exchangeRates[dustSY] = big.NewInt(1_141_613_233_240_166_292)
	fixture.stub.yieldTokens[dustSY] = dustYieldToken
	fixture.stub.assets[dustSY] = dustAsset
	fixture.stub.pyIndices[dustYT] = big.NewInt(1_135_641_111_437_893_889)
	fixture.stub.ytInterests[dustYT] = new(big.Int)
	fixture.stub.ytRewardTokens[dustYT] = []common.Address{}
	fixture.stub.ytRewards[dustYT] = []*big.Int{}
	indexer := &stubPendleIndexer{refs: []pendlePositionRef{{
		Token: dustYT, Kind: pendleYT, PT: dustPT, SY: dustSY,
		Expiry: expiry, CreatedBlock: 2_203_309,
	}}}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one direct dust holding and no claim components", groups)
	}
	if groups[0].ID != "yt:"+strings.ToLower(dustYT.Hex()) ||
		groups[0].MarketID != strings.ToLower(dustPT.Hex()) {
		t.Fatalf("group identity = %#v, want YT %s / PT %s", groups[0], dustYT, dustPT)
	}
	component := groups[0].Components[0]
	if component.Kind != "asset" || component.Token.Address != dustYT ||
		component.Token.Symbol != "YT-syrupUSDT-29JAN2026" ||
		component.Token.Decimals != 6 || component.AmountRaw != "15" {
		t.Fatalf("dust component = %#v, want 15 raw units of the six-decimal YT", component)
	}
	if component.Source.Contract != dustYT || component.Source.Method != "balanceOf" {
		t.Fatalf("dust source = %#v, want pinned balanceOf", component.Source)
	}
	if component.PriceBasis != nil {
		t.Fatalf("expired YT basis = %#v, want the zero-priced token left unpriced", component.PriceBasis)
	}
	if got := groups[0].Metadata["expiry"]; got != "1769644800" {
		t.Fatalf("expiry metadata = %v, want 1769644800", got)
	}
}

// A YT balance is only the future-yield claim. Interest already accrued but not redeemed is a
// separate, currently claimable SY amount; simulating the canonical claim and previewing that SY
// into its yield token keeps it from disappearing from the portfolio.
func TestPendleDirectYTIncludesClaimableInterestInTheYieldToken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	yieldToken := common.HexToAddress("0x00000000000000000000000000000000000000b2")
	interest, ok := new(big.Int).SetString("7124537190334757000", 10)
	if !ok {
		t.Fatal("invalid interest fixture")
	}
	fixture.stub.yieldTokens[fixture.sy] = yieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.symbols[yieldToken] = "sUSDe"
	fixture.stub.decimals[yieldToken] = 18
	fixture.stub.ytInterests[fixture.yieldT] = interest
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 2 {
		t.Fatalf("groups = %#v, want YT balance plus claimable interest", groups)
	}
	claim := groups[0].Components[1]
	if claim.Kind != "reward" || claim.Token.Address != yieldToken {
		t.Fatalf("interest component = %#v, want redeemable yield-token reward", claim)
	}
	if claim.AmountRaw != interest.String() {
		t.Fatalf("interest amount = %s, want %s", claim.AmountRaw, interest)
	}
}

// Transferring the final YT checkpoints interest to the former holder before their token balance
// reaches zero. The index still has the YT reference, so the claim remains discoverable and must
// be reported without inventing a zero-balance YT asset component.
func TestPendleDirectYTKeepsClaimAfterLastTokenWasTransferred(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	yieldToken := common.HexToAddress("0x00000000000000000000000000000000000000b4")
	interest := big.NewInt(9_500_000_000_000_000)
	fixture.stub.balances[fixture.yieldT] = new(big.Int)
	fixture.stub.yieldTokens[fixture.sy] = yieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.symbols[yieldToken] = "sUSDe"
	fixture.stub.decimals[yieldToken] = 18
	fixture.stub.ytInterests[fixture.yieldT] = interest
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want only the post-transfer claim", groups)
	}
	claim := groups[0].Components[0]
	if claim.Kind != "reward" || claim.Token.Address != yieldToken || claim.AmountRaw != interest.String() {
		t.Fatalf("claim component = %#v, want %s %s", claim, interest, yieldToken)
	}
}

// A stale zero-balance YT can return a claim token whose ERC20 metadata is no longer readable.
// That historical claim is skipped best effort and cannot erase an unrelated healthy PT holding.
func TestPendleBrokenZeroBalanceYTClaimMetadataDoesNotBreakHealthyToken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	staleYT := common.HexToAddress("0x00000000000000000000000000000000000000b5")
	brokenInterestToken := common.HexToAddress("0x00000000000000000000000000000000000000b6")
	fixture.stub.balances[staleYT] = new(big.Int)
	fixture.stub.ytInterests[staleYT] = big.NewInt(1)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{
				Token: staleYT, Kind: pendleYT, PT: fixture.principal,
				SY: brokenInterestToken, Expiry: 1_795_651_200, CreatedBlock: 1,
			},
			{
				Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
				SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
			},
		},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || groups[0].ID != "pt:"+strings.ToLower(fixture.principal.Hex()) {
		t.Fatalf("groups = %#v, want only the healthy PT holding", groups)
	}
}

// previewRedeem is an optional improvement to the claim's unit. If its yield token later stops
// answering ERC20 metadata, retain the canonical raw SY claim returned by the YT contract.
func TestPendleYTClaimFallsBackToRawSYWhenPreviewTokenMetadataIsBroken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	brokenYieldToken := common.HexToAddress("0x00000000000000000000000000000000000000b7")
	rawInterest := big.NewInt(17_000_000_000_000_000)
	fixture.stub.balances[fixture.yieldT] = new(big.Int)
	fixture.stub.yieldTokens[fixture.sy] = brokenYieldToken
	fixture.stub.previewRates[fixture.sy] = new(big.Int).Mul(pendleExchangeRateOne, big.NewInt(2))
	fixture.stub.ytInterests[fixture.yieldT] = rawInterest
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one raw-SY claim fallback", groups)
	}
	claim := groups[0].Components[0]
	if claim.Token.Address != fixture.sy || claim.AmountRaw != rawInterest.String() {
		t.Fatalf("claim = %#v, want raw SY %s", claim, rawInterest)
	}
}

// A permissionless SY controls the reward arrays exposed through its YT. A malformed pair cannot
// use a dust YT transfer to fail the whole account; the valid token holding remains visible while
// only its unalignable reward list is discarded.
func TestPendleMismatchedYTRewardArraysDoNotBreakHealthyToken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	malformedYT := common.HexToAddress("0x00000000000000000000000000000000000000b8")
	rewardToken := common.HexToAddress("0x00000000000000000000000000000000000000b9")
	fixture.stub.balances[malformedYT] = big.NewInt(1)
	fixture.stub.ytRewardTokens[malformedYT] = []common.Address{rewardToken}
	fixture.stub.ytRewards[malformedYT] = []*big.Int{}
	fixture.stub.symbols[malformedYT] = "YT-MALFORMED"
	fixture.stub.decimals[malformedYT] = 18
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{
				Token: malformedYT, Kind: pendleYT, PT: fixture.principal,
				SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 1,
			},
			{
				Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
				SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
			},
		},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want both valid token holdings", groups)
	}
	foundHealthyPT := false
	for _, group := range groups {
		if group.ID == "pt:"+strings.ToLower(fixture.principal.Hex()) {
			foundHealthyPT = true
		}
	}
	if !foundHealthyPT {
		t.Fatalf("groups = %#v, healthy PT was erased by malformed YT rewards", groups)
	}
}

// A permissionless SY can name an accounting asset with broken ERC20 metadata. That makes only
// the dust YT's optional price basis unusable; canonical held-token metadata and healthy groups
// remain strict and intact.
func TestPendleBrokenBasisMetadataDoesNotBreakHealthyDirectHolding(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	malformedYT := common.HexToAddress("0x00000000000000000000000000000000000000bc")
	maliciousSY := common.HexToAddress("0x00000000000000000000000000000000000000bd")
	brokenAsset := common.HexToAddress("0x00000000000000000000000000000000000000be")
	fixture.stub.balances[malformedYT] = big.NewInt(1)
	fixture.stub.symbols[malformedYT] = "YT-BROKEN-BASIS"
	fixture.stub.decimals[malformedYT] = 18
	fixture.stub.exchangeRates[maliciousSY] = pendleExchangeRateOne
	fixture.stub.assets[maliciousSY] = brokenAsset
	fixture.stub.decimals[brokenAsset] = 18
	fixture.stub.pyIndices[malformedYT] = pendleExchangeRateOne
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{
				Token: malformedYT, Kind: pendleYT, PT: fixture.principal,
				SY: maliciousSY, Expiry: 1_795_651_200, CreatedBlock: 1,
			},
			{
				Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
				SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
			},
		},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want both direct holdings", groups)
	}
	for _, group := range groups {
		if group.ID == "yt:"+strings.ToLower(malformedYT.Hex()) && group.Components[0].PriceBasis != nil {
			t.Fatalf("malformed YT basis = %#v, want unpriced holding", group.Components[0].PriceBasis)
		}
	}
}

// Native incentives returned by the YT claim simulation are separate portfolio assets. The
// token and amount arrays are positional, and zero rewards must not create empty components or
// trigger metadata reads for assets the account cannot currently claim.
func TestPendleDirectYTIncludesOnlyNonzeroNativeRewards(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	zeroRewardToken := common.HexToAddress("0x00000000000000000000000000000000000000c1")
	rewardToken := common.HexToAddress("0x00000000000000000000000000000000000000c2")
	rewardAmount := big.NewInt(42_500_000)
	fixture.stub.ytRewardTokens[fixture.yieldT] = []common.Address{zeroRewardToken, rewardToken}
	fixture.stub.ytRewards[fixture.yieldT] = []*big.Int{new(big.Int), rewardAmount}
	fixture.stub.symbols[rewardToken] = "PENDLE"
	fixture.stub.decimals[rewardToken] = 18
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{
			fixture.principal: {fixture.market},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 2 {
		t.Fatalf("groups = %#v, want YT balance plus one nonzero native reward", groups)
	}
	reward := groups[0].Components[1]
	if reward.Kind != "reward" || reward.Token.Address != rewardToken {
		t.Fatalf("reward component = %#v, want the nonzero native reward token", reward)
	}
	if reward.AmountRaw != rewardAmount.String() {
		t.Fatalf("reward amount = %s, want %s", reward.AmountRaw, rewardAmount)
	}
}

// A market checkpoints gauge rewards before transferring away the final LP token. Preserve that
// reward-only position without emitting zero SY or PT reserve components.
func TestPendleLiquidityKeepsRewardAfterLastLPWasTransferred(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	rewardToken := common.HexToAddress("0x00000000000000000000000000000000000000c3")
	rewardAmount := big.NewInt(81_000_000_000_000_000)
	fixture.stub.balances[fixture.market] = new(big.Int)
	fixture.stub.ytRewardTokens[fixture.market] = []common.Address{rewardToken}
	fixture.stub.ytRewards[fixture.market] = []*big.Int{rewardAmount}
	fixture.stub.symbols[rewardToken] = "PENDLE"
	fixture.stub.decimals[rewardToken] = 18
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want only the post-transfer LP reward", groups)
	}
	reward := groups[0].Components[0]
	if reward.Kind != "reward" || reward.Token.Address != rewardToken || reward.AmountRaw != rewardAmount.String() {
		t.Fatalf("reward component = %#v, want %s %s", reward, rewardAmount, rewardToken)
	}
	if fixture.stub.stateCalls[fixture.market] != 0 {
		t.Fatal("a zero-balance LP reward claim must not read full market state")
	}
}

// A stale historical LP reference with no rewards is not a position. It gets only the lightweight
// reward-token probe, cannot touch market/SY decomposition, and cannot erase a healthy live LP.
func TestPendleStaleZeroBalanceLPDoesNotBreakHealthyLiquidity(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	staleMarket := common.HexToAddress("0x00000000000000000000000000000000000000d1")
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.balances[staleMarket] = new(big.Int)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{Token: staleMarket, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 1},
			{Token: fixture.market, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 25_098_960},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || groups[0].ID != "lp:"+strings.ToLower(fixture.market.Hex()) {
		t.Fatalf("groups = %#v, want only the healthy positive LP", groups)
	}
	if fixture.stub.stateCalls[staleMarket] != 0 {
		t.Fatal("a stale zero-balance LP must not read market state")
	}
}

// Even a stale LP that reports a positive simulated reward is untrusted historical input. Broken
// reward-token metadata drops only that claim; it cannot fail a healthy positive-balance market.
func TestPendleBrokenZeroBalanceLPRewardMetadataDoesNotBreakHealthyLiquidity(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	staleMarket := common.HexToAddress("0x00000000000000000000000000000000000000d2")
	brokenReward := common.HexToAddress("0x00000000000000000000000000000000000000d3")
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.balances[staleMarket] = new(big.Int)
	fixture.stub.ytRewardTokens[staleMarket] = []common.Address{brokenReward}
	fixture.stub.ytRewards[staleMarket] = []*big.Int{big.NewInt(1)}
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{Token: staleMarket, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 1},
			{Token: fixture.market, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 25_098_960},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || groups[0].ID != "lp:"+strings.ToLower(fixture.market.Hex()) {
		t.Fatalf("groups = %#v, want only the healthy positive LP", groups)
	}
	if fixture.stub.stateCalls[staleMarket] != 0 {
		t.Fatal("a zero-balance LP with broken reward metadata must not read market state")
	}
}

// A positive dust LP can point at a stale or malformed market row. Its failed core reads are
// isolated, so a separate healthy market remains visible.
func TestPendleBrokenPositiveMarketDoesNotBreakHealthyLiquidity(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	brokenMarket := common.HexToAddress("0x00000000000000000000000000000000000000d4")
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.balances[brokenMarket] = big.NewInt(1)
	fixture.stub.rewardReverts[brokenMarket] = true
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{Token: brokenMarket, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 1},
			{Token: fixture.market, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 25_098_960},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || groups[0].ID != "lp:"+strings.ToLower(fixture.market.Hex()) {
		t.Fatalf("groups = %#v, want only the healthy positive LP", groups)
	}
}

// Reward discovery is optional to the reserve decomposition. A permissionless SY can make that
// call revert, but the positive LP's SY and PT holdings must still be returned.
func TestPendleRewardTokenRevertDoesNotDropPositiveLiquidity(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.rewardReverts[fixture.market] = true
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) < 2 {
		t.Fatalf("groups = %#v, want LP reserve legs without rewards", groups)
	}
}

// exchangeRate/assetInfo are valuation upgrades, not ownership prerequisites. A broken arbitrary
// SY therefore remains as the exact raw reserve token and cannot erase another healthy market.
func TestPendleBrokenSYStateKeepsRawLiquidityAndHealthyMarket(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	brokenMarket := common.HexToAddress("0x00000000000000000000000000000000000000d5")
	brokenSY := common.HexToAddress("0x00000000000000000000000000000000000000d6")
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.balances[brokenMarket] = big.NewInt(111_565)
	fixture.stub.markets[brokenMarket] = fixture.stub.markets[fixture.market]
	fixture.stub.marketTokens[brokenMarket] = [3]common.Address{
		brokenSY, fixture.principal, fixture.yieldT,
	}
	fixture.stub.symbols[brokenSY] = "SY-BROKEN"
	fixture.stub.decimals[brokenSY] = 18
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{
			{Token: brokenMarket, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 1},
			{Token: fixture.market, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 25_098_960},
		},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 2 {
		t.Fatalf("groups = %#v, want both positive LPs", groups)
	}
	foundRawSY := false
	for _, group := range groups {
		if group.ID != "lp:"+strings.ToLower(brokenMarket.Hex()) {
			continue
		}
		for _, component := range group.Components {
			if component.Token.Address == brokenSY {
				foundRawSY = true
			}
		}
	}
	if !foundRawSY {
		t.Fatalf("groups = %#v, broken SY's raw reserve leg is missing", groups)
	}
}

// Bound hostile dynamic arrays before they multiply claim and metadata work.
func TestPendleOversizedRewardListDoesNotFanOut(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	fixture.stub.ytRewardTokens[fixture.market] = make(
		[]common.Address, pendleMaxRewardTokens+1,
	)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want the underlying LP position", groups)
	}
	if fixture.stub.rewardCalls[fixture.market] != 0 {
		t.Fatalf("redeemRewards calls = %d, want zero for oversized token list",
			fixture.stub.rewardCalls[fixture.market])
	}
}

// 32 of the 874 PTs the index knows carry more than one market, so which one sets the discount
// has to be decided rather than left to map order. The deepest SY reserve wins: it is the rate
// the most capital has agreed to. A market that reverts drops out instead of failing the scan.
func TestPendleDeepestMarketSetsTheDiscount(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	shallow := common.HexToAddress("0x1111111111111111111111111111111111111111")
	broken := common.HexToAddress("0x2222222222222222222222222222222222222222")
	fixture.stub.markets[shallow] = pendleStubMarket{
		totalPt: big.NewInt(1), totalSy: big.NewInt(1), totalLp: big.NewInt(1),
		expiry: 1_795_651_200, lnImpliedRate: big.NewInt(900_000_000_000_000_000),
	}
	fixture.stub.markets[broken] = pendleStubMarket{revert: true}
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{
			fixture.principal: {shallow, broken, fixture.market},
		},
	}

	component := fixture.onlyComponent(t, fixture.run(t, indexer))

	if component.PriceBasis == nil {
		t.Fatal("the PT was left unpriced")
	}
	if component.PriceBasis.RatioRaw != "989030473175155466" {
		t.Fatalf("basis ratio = %s, want the deepest market's discount", component.PriceBasis.RatioRaw)
	}
	if fixture.stub.stateCalls[shallow] == 0 || fixture.stub.stateCalls[broken] == 0 {
		t.Fatal("every candidate market must be read before one is chosen")
	}
}

// An expired PT redeems for its asset one for one, so it prices without consulting a market at
// all. Reaching the index for markets it no longer needs would be a round trip per scan.
func TestPendleExpiredPTPricesAtParWithoutAMarketLookup(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: fixture.block.Timestamp - 1, CreatedBlock: 25_098_960,
		}},
	}

	component := fixture.onlyComponent(t, fixture.run(t, indexer))

	if indexer.marketsCalled {
		t.Fatal("an expired PT must not trigger a market lookup")
	}
	if component.PriceBasis == nil {
		t.Fatal("an expired PT must price at par")
	}
	if component.PriceBasis.RatioRaw != priceBasisRatioOne.String() {
		t.Fatalf("basis ratio = %s, want parity", component.PriceBasis.RatioRaw)
	}
}

// Without a usable market the PT keeps its own unquoted identity and no basis. That is the
// behaviour that shipped: the position is still reported, and the response carries a pricing
// gap rather than a fabricated number.
func TestPendlePTWithoutAUsableMarketStaysReportedAndUnpriced(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
	}

	component := fixture.onlyComponent(t, fixture.run(t, indexer))

	if component.Token.Address != fixture.principal {
		t.Fatalf("component token = %s, want the PT itself", component.Token.Address)
	}
	if component.AmountRaw != "1648554788087306450847013" {
		t.Fatalf("component amount = %s", component.AmountRaw)
	}
	if component.PriceBasis != nil {
		t.Fatalf("basis = %#v, want none without a market", component.PriceBasis)
	}
}

// A liquidity position decomposes into a SY leg and a PT leg. The SY leg already priced, through
// the accounting asset it converts into; the PT leg did not, because it is reported as the PT the
// reserve actually holds. Both legs now reach the same quoted asset.
//
// This also pins the readState output positions: expiry and lastLnImpliedRate are read out of the
// same call as the reserves, and an off-by-one there would discount the PT by a fee rate or a
// reserve percent instead of by the market's implied rate.
func TestPendleLiquidityPTLegPricesThroughTheSameAsset(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	var assetLeg, principalLeg *Component
	for index := range groups[0].Components {
		component := &groups[0].Components[index]
		switch component.Token.Address {
		case fixture.asset:
			assetLeg = component
		case fixture.principal:
			principalLeg = component
		}
	}
	if assetLeg == nil || principalLeg == nil {
		t.Fatalf("components = %#v, want both a SY-derived asset leg and a PT leg", groups[0].Components)
	}
	if assetLeg.PriceBasis != nil {
		t.Fatalf("the asset leg is quoted directly and needs no basis: %#v", assetLeg.PriceBasis)
	}
	if principalLeg.PriceBasis == nil {
		t.Fatal("the PT reserve leg was left unpriced")
	}
	if principalLeg.PriceBasis.Token.Address != fixture.asset {
		t.Fatalf("PT leg basis token = %s, want the same asset as the SY leg",
			principalLeg.PriceBasis.Token.Address)
	}
	if principalLeg.PriceBasis.RatioRaw != "989030473175155466" {
		t.Fatalf("PT leg basis ratio = %s", principalLeg.PriceBasis.RatioRaw)
	}
	if indexer.marketsCalled {
		t.Fatal("a liquidity position already reads its market and must not look one up")
	}
}

// A standard SY's yield token is the holder's safest concrete claim and may use a different
// decimal base from assetInfo. Monad SY-sUSDat is the live regression: SY and sUSDat use 18
// decimals while the accounting asset uses 6; DeBank also reports the LP leg as sUSDat.
func TestPendleLiquidityPrefersRedeemableYieldTokenAcrossDifferentAssetDecimals(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	yieldToken := common.HexToAddress("0x00000000000000000000000000000000000000b1")
	fixture.stub.decimals[fixture.sy] = 18
	fixture.stub.decimals[fixture.asset] = 6
	fixture.stub.decimals[yieldToken] = 18
	fixture.stub.symbols[yieldToken] = "sUSDat"
	fixture.stub.decimals[fixture.principal] = 6
	fixture.stub.exchangeRates[fixture.sy] = big.NewInt(1_016_186)
	fixture.stub.yieldTokens[fixture.sy] = yieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.pyIndices[fixture.yieldT] = big.NewInt(1_016_186)
	fixture.stub.markets[fixture.market] = pendleStubMarket{
		totalPt:       new(big.Int),
		totalSy:       big.NewInt(1_000_000_000_000_000_000),
		totalLp:       big.NewInt(1),
		expiry:        1_795_651_200,
		lnImpliedRate: big.NewInt(47_125_524_316_414_759),
	}
	fixture.stub.balances[fixture.market] = big.NewInt(1)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %#v, want one liquidity asset component", groups)
	}
	component := groups[0].Components[0]
	if component.Token.Address != yieldToken {
		t.Fatalf("component token = %s, want redeemable yield token %s", component.Token.Address, yieldToken)
	}
	if component.Token.Decimals != 18 {
		t.Fatalf("yield-token decimals = %d, want 18", component.Token.Decimals)
	}
	if component.AmountRaw != "1000000000000000000" {
		t.Fatalf("yield-token amount = %s, want one natural token", component.AmountRaw)
	}
}

// Some standard SYs deliberately have no accounting asset. Monad's official shMON market is the
// live shape: assetInfo returns address(0), while yieldToken and previewRedeem are both usable.
// That LP must report its redeemable shMON leg without querying zero-address ERC20 metadata; its
// PT remains visible but unpriced because there is no accounting asset for a defensible basis.
func TestPendleLiquiditySupportsAZeroAccountingAssetWithRedeemableYieldToken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	yieldToken := common.HexToAddress("0x1B68c5aA0c0512D1c5F07192C0266766D6B65FD5")
	fixture.stub.assets[fixture.sy] = common.Address{}
	fixture.stub.decimals[common.Address{}] = 18
	fixture.stub.yieldTokens[fixture.sy] = yieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.symbols[yieldToken] = "shMON"
	fixture.stub.decimals[yieldToken] = 18
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 || len(groups[0].Components) != 2 {
		t.Fatalf("groups = %#v, want redeemable yield-token and PT legs", groups)
	}
	var yieldLeg, principalLeg *Component
	for index := range groups[0].Components {
		component := &groups[0].Components[index]
		switch component.Token.Address {
		case yieldToken:
			yieldLeg = component
		case fixture.principal:
			principalLeg = component
		}
	}
	if yieldLeg == nil {
		t.Fatalf("components = %#v, want redeemable yield-token leg", groups[0].Components)
	}
	if principalLeg == nil || principalLeg.PriceBasis != nil {
		t.Fatalf("PT component = %#v, want a visible but unpriced PT leg", principalLeg)
	}
}

// A preview is only an upgrade when its result can actually be described as an ERC20. A
// permissionless SY can return a token with broken metadata; retain the safe accounting-asset
// conversion rather than failing the account or losing the reserve leg.
func TestPendleLiquidityFallsBackWhenPreviewTokenMetadataIsBroken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	brokenYieldToken := common.HexToAddress("0x00000000000000000000000000000000000000ba")
	fixture.stub.yieldTokens[fixture.sy] = brokenYieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	for _, component := range groups[0].Components {
		if component.Token.Address == fixture.asset {
			return
		}
	}
	t.Fatalf("components = %#v, want accounting-asset fallback", groups[0].Components)
}

// The same broken preview token on a native-accounting SY has no assetInfo fallback. Preserve
// the raw SY reserve share, which is still an exact on-chain holding.
func TestPendleZeroAssetLiquidityFallsBackToRawSYWhenPreviewMetadataIsBroken(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	brokenYieldToken := common.HexToAddress("0x00000000000000000000000000000000000000bb")
	fixture.stub.assets[fixture.sy] = common.Address{}
	fixture.stub.decimals[common.Address{}] = 18
	fixture.stub.yieldTokens[fixture.sy] = brokenYieldToken
	fixture.stub.previewRates[fixture.sy] = pendleExchangeRateOne
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	for _, component := range groups[0].Components {
		if component.Token.Address == fixture.sy {
			return
		}
	}
	t.Fatalf("components = %#v, want raw SY fallback", groups[0].Components)
}

// A zero-accounting-asset SY may also reject the optional preview at a historical block. The
// holder still owns the market's raw SY reserve share, so degrade to that token rather than
// silently dropping the leg.
func TestPendleLiquidityKeepsRawSYWhenZeroAssetPreviewReverts(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.assets[fixture.sy] = common.Address{}
	fixture.stub.decimals[common.Address{}] = 18
	delete(fixture.stub.previewRates, fixture.sy)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	for _, component := range groups[0].Components {
		if component.Token.Address == fixture.sy {
			want := pendleReserveShare(
				fixture.stub.balances[fixture.market],
				fixture.stub.markets[fixture.market].totalSy,
				fixture.stub.markets[fixture.market].totalLp,
			)
			if component.AmountRaw != want.String() {
				t.Fatalf("raw SY amount = %s, want %s", component.AmountRaw, want)
			}
			return
		}
	}
	t.Fatalf("components = %#v, want raw SY fallback", groups[0].Components)
}

// previewRedeem is best-effort and heterogeneous SY implementations can reject it. One such
// failure must degrade only that reserve leg to the always-defined assetInfo/exchangeRate path,
// not erase the LP or fail the account scan.
func TestPendleLiquidityFallsBackToAccountingAssetWhenPreviewRedeemReverts(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.yieldTokens[fixture.sy] = common.HexToAddress("0x00000000000000000000000000000000000000b3")
	delete(fixture.stub.previewRates, fixture.sy)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	wantAmount := pendleSYToAsset(
		pendleReserveShare(
			fixture.stub.balances[fixture.market],
			fixture.stub.markets[fixture.market].totalSy,
			fixture.stub.markets[fixture.market].totalLp,
		),
		fixture.stub.exchangeRates[fixture.sy],
	)
	var fallback *Component
	for index := range groups[0].Components {
		if groups[0].Components[index].Token.Address == fixture.asset {
			fallback = &groups[0].Components[index]
		}
	}
	if fallback == nil {
		t.Fatalf("components = %#v, want accounting-asset fallback", groups[0].Components)
	}
	if fallback.AmountRaw != wantAmount.String() {
		t.Fatalf("fallback amount = %s, want %s", fallback.AmountRaw, wantAmount)
	}
}

// Older or non-standard SY implementations may not expose yieldToken at the pinned block. That
// optional convenience read must not erase the long-standing assetInfo/exchangeRate fallback.
func TestPendleLiquidityFallsBackWhenYieldTokenIsUnavailable(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	delete(fixture.stub.yieldTokens, fixture.sy)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	indexer := &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
			CreatedBlock: 25_098_960,
		}},
	}

	groups := fixture.run(t, indexer)
	if len(groups) != 1 {
		t.Fatalf("groups = %#v, want one liquidity group", groups)
	}
	for _, component := range groups[0].Components {
		if component.Token.Address == fixture.asset {
			return
		}
	}
	t.Fatalf("components = %#v, want accounting-asset fallback", groups[0].Components)
}

func TestPendleSYToAssetConvertsDifferentRawDecimalBases(t *testing.T) {
	amount := big.NewInt(1_000_000_000_000_000_000)
	if got := pendleSYToAsset(amount, big.NewInt(1_016_186)); got.String() != "1016186" {
		t.Fatalf("asset amount = %s, want 1016186 raw units", got)
	}
}

// The factor is what a PY pair can actually redeem, so it is exercised at the boundary rather
// than only in the happy case. Both of PendlePYOracleLib's pyIndex branches agree on it — the
// cached branch takes pyIndexStored, the fresh one max(syIndex, pyIndexStored), and the clamp at
// one collapses the difference — which is why only pyIndexStored is read.
func TestPendleSolvencyFactorImpairsOnlyWhenTheSYHasFallenBehind(t *testing.T) {
	one := priceBasisRatioOne
	for _, test := range []struct {
		name    string
		syIndex *big.Int
		stored  *big.Int
		want    string
	}{
		{name: "accrued past the index", syIndex: big.NewInt(11e17), stored: one, want: one.String()},
		{name: "exactly at the index", syIndex: one, stored: one, want: one.String()},
		{name: "ten percent behind", syIndex: big.NewInt(9e17), stored: one, want: "900000000000000000"},
		{name: "index ratcheted above", syIndex: one, stored: big.NewInt(125e16), want: "800000000000000000"},
	} {
		factor, err := pendleSolvencyFactor(test.syIndex, test.stored)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if factor.String() != test.want {
			t.Fatalf("%s: factor = %s, want %s", test.name, factor, test.want)
		}
		if factor.Cmp(one) > 0 {
			t.Fatalf("%s: a claim can never redeem more than its asset", test.name)
		}
	}
	for _, test := range [][2]*big.Int{
		{nil, one}, {one, nil}, {big.NewInt(0), one}, {one, big.NewInt(0)},
	} {
		if _, err := pendleSolvencyFactor(test[0], test[1]); err == nil {
			t.Fatalf("missing indices were accepted: %v", test)
		}
	}
}

// The regression for the review that caught this: an SY whose exchange rate has fallen below the
// index its pair ratcheted to cannot redeem the pair whole, and reporting the raw implied-rate
// discount would overstate every PT and YT written against it.
func TestPendleInsolventSYImpairsEveryPendleBasis(t *testing.T) {
	insolvent := func(t *testing.T) pendlePricingFixture {
		t.Helper()
		fixture := newPendlePricingFixture(t)
		// The SY has lost 10% against the index the pair ratcheted to.
		fixture.stub.exchangeRates[fixture.sy] = big.NewInt(9e17)
		fixture.stub.pyIndices[fixture.yieldT] = priceBasisRatioOne
		return fixture
	}
	principalRef := func(fixture pendlePricingFixture, expiry uint64) pendlePositionRef {
		return pendlePositionRef{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: expiry, CreatedBlock: 25_098_960,
		}
	}

	t.Run("unexpired PT", func(t *testing.T) {
		fixture := insolvent(t)
		component := fixture.onlyComponent(t, fixture.run(t, &stubPendleIndexer{
			refs:    []pendlePositionRef{principalRef(fixture, 1_795_651_200)},
			markets: map[common.Address][]common.Address{fixture.principal: {fixture.market}},
		}))
		if component.PriceBasis == nil {
			t.Fatal("the PT was left unpriced")
		}
		// 0.989030473175155466 of the asset, then 90% of that.
		if component.PriceBasis.RatioRaw != "890127425857639919" {
			t.Fatalf("basis ratio = %s, want the discount impaired by solvency",
				component.PriceBasis.RatioRaw)
		}
	})

	// The case the review called out explicitly: parity at expiry is a property of a solvent SY,
	// not of expiry, so an expired PT against an impaired SY is worth the factor and not one.
	t.Run("expired PT", func(t *testing.T) {
		fixture := insolvent(t)
		component := fixture.onlyComponent(t, fixture.run(t, &stubPendleIndexer{
			refs: []pendlePositionRef{principalRef(fixture, fixture.block.Timestamp-1)},
		}))
		if component.PriceBasis == nil {
			t.Fatal("the expired PT was left unpriced")
		}
		if component.PriceBasis.RatioRaw != "900000000000000000" {
			t.Fatalf("basis ratio = %s, want parity impaired by solvency",
				component.PriceBasis.RatioRaw)
		}
	})

	t.Run("YT", func(t *testing.T) {
		fixture := insolvent(t)
		component := fixture.onlyComponent(t, fixture.run(t, &stubPendleIndexer{
			refs: []pendlePositionRef{{
				Token: fixture.yieldT, Kind: pendleYT, PT: fixture.principal,
				SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
			}},
			markets: map[common.Address][]common.Address{fixture.principal: {fixture.market}},
		}))
		if component.PriceBasis == nil {
			t.Fatal("the YT was left unpriced")
		}
		if component.PriceBasis.RatioRaw != "9872574142360080" {
			t.Fatalf("basis ratio = %s, want the complement impaired by solvency",
				component.PriceBasis.RatioRaw)
		}
	})

	t.Run("liquidity PT leg", func(t *testing.T) {
		fixture := insolvent(t)
		fixture.stub.balances[fixture.market] = big.NewInt(111_565)
		groups := fixture.run(t, &stubPendleIndexer{
			refs: []pendlePositionRef{{
				Token: fixture.market, Kind: pendleLP, PT: fixture.principal,
				CreatedBlock: 25_098_960,
			}},
		})
		if len(groups) != 1 {
			t.Fatalf("groups = %#v, want one liquidity group", groups)
		}
		var principalLeg *Component
		for index := range groups[0].Components {
			if groups[0].Components[index].Token.Address == fixture.principal {
				principalLeg = &groups[0].Components[index]
			}
		}
		if principalLeg == nil || principalLeg.PriceBasis == nil {
			t.Fatalf("components = %#v, want a priced PT reserve leg", groups[0].Components)
		}
		if principalLeg.PriceBasis.RatioRaw != "890127425857639919" {
			t.Fatalf("PT leg basis ratio = %s, want the discount impaired by solvency",
				principalLeg.PriceBasis.RatioRaw)
		}
	})
}

// A yield token that will not report its index leaves the pair unpriced. Defaulting the factor to
// one would silently assume solvency, which is the assumption that overstates precisely the
// positions this factor exists to correct.
func TestPendleUnreadablePYIndexLeavesThePairUnpriced(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	delete(fixture.stub.pyIndices, fixture.yieldT)
	component := fixture.onlyComponent(t, fixture.run(t, &stubPendleIndexer{
		refs: []pendlePositionRef{{
			Token: fixture.principal, Kind: pendlePT, PT: fixture.principal,
			SY: fixture.sy, Expiry: 1_795_651_200, CreatedBlock: 25_098_960,
		}},
		markets: map[common.Address][]common.Address{fixture.principal: {fixture.market}},
	}))
	if component.Token.Address != fixture.principal {
		t.Fatalf("component token = %s, want the PT still reported", component.Token.Address)
	}
	if component.PriceBasis != nil {
		t.Fatalf("basis = %#v, want none without a PY index", component.PriceBasis)
	}
}
