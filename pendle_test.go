package portfolio

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
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
	t             *testing.T
	balances      map[common.Address]*big.Int
	markets       map[common.Address]pendleStubMarket
	marketTokens  map[common.Address][3]common.Address
	exchangeRates map[common.Address]*big.Int
	assets        map[common.Address]common.Address
	symbols       map[common.Address]string
	decimals      map[common.Address]uint8
	stateCalls    map[common.Address]int
}

func (s *pendleStubRPC) dispatch(to common.Address, data []byte) (string, map[string]any) {
	fail := map[string]any{"code": -32000, "message": "execution reverted"}
	if len(data) < 4 {
		return "", fail
	}
	selector := string(data[:4])
	switch selector {
	case string(erc20ABI.Methods["symbol"].ID):
		symbol, known := s.symbols[to]
		if !known {
			return "", fail
		}
		out, _ := erc20ABI.Methods["symbol"].Outputs.Pack(symbol)
		return "0x" + common.Bytes2Hex(out), nil
	case string(erc20ABI.Methods["decimals"].ID):
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
		out, _ := pendleMarketABI.Methods["getRewardTokens"].Outputs.Pack([]common.Address{})
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["exchangeRate"].ID):
		rate, known := s.exchangeRates[to]
		if !known {
			return "", fail
		}
		out, _ := pendleStandardizedYieldABI.Methods["exchangeRate"].Outputs.Pack(rate)
		return "0x" + common.Bytes2Hex(out), nil
	case string(pendleStandardizedYieldABI.Methods["assetInfo"].ID):
		asset, known := s.assets[to]
		if !known {
			return "", fail
		}
		out, _ := pendleStandardizedYieldABI.Methods["assetInfo"].Outputs.Pack(
			uint8(0), asset, uint8(18),
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
			response["result"] = "0x1"
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
		t: t,
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
		exchangeRates: map[common.Address]*big.Int{fixture.sy: pendleExchangeRateOne},
		assets:        map[common.Address]common.Address{fixture.sy: fixture.asset},
		symbols:       map[common.Address]string{},
		decimals:      map[common.Address]uint8{},
		stateCalls:    map[common.Address]int{},
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
	client, err := DialRPC(t.Context(), Ethereum, server.URL)
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
