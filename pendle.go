package portfolio

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var (
	// isPT/isYT/isValidMarket are the only enumeration surface Pendle exposes: they answer
	// "does this factory own that token" but cannot list what a factory has created. That is
	// why the token universe comes from the indexer and only tail candidates are classified here.
	pendleYieldContractFactoryABI = MustABI(`[
      {"type":"function","name":"isPT","stateMutability":"view","inputs":[{"name":"token","type":"address"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"isYT","stateMutability":"view","inputs":[{"name":"token","type":"address"}],"outputs":[{"type":"bool"}]}
    ]`)
	pendleMarketFactoryABI = MustABI(`[
      {"type":"function","name":"isValidMarket","stateMutability":"view","inputs":[{"name":"market","type":"address"}],"outputs":[{"type":"bool"}]}
    ]`)
	pendlePrincipalTokenABI = MustABI(`[
      {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"expiry","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"SY","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"YT","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
    ]`)
	// PT, YT and market contracts all expose factory(); an unrelated ERC20 does not, so the
	// call reverting is itself the first filter.
	pendleTokenFactoryABI = MustABI(`[
      {"type":"function","name":"factory","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
    ]`)
	pendleYieldTokenABI = MustABI(`[
      {"type":"function","name":"PT","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"pyIndexStored","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
    ]`)
	pendleMarketABI = MustABI(`[
      {"type":"function","name":"readTokens","stateMutability":"view","inputs":[],"outputs":[{"name":"_SY","type":"address"},{"name":"_PT","type":"address"},{"name":"_YT","type":"address"}]},
      {"type":"function","name":"readState","stateMutability":"view","inputs":[{"name":"router","type":"address"}],"outputs":[{"name":"totalPt","type":"int256"},{"name":"totalSy","type":"int256"},{"name":"totalLp","type":"int256"},{"name":"treasury","type":"address"},{"name":"scalarRoot","type":"int256"},{"name":"expiry","type":"uint256"},{"name":"lnFeeRateRoot","type":"uint256"},{"name":"reserveFeePercent","type":"uint256"},{"name":"lastLnImpliedRate","type":"uint256"}]},
      {"type":"function","name":"getRewardTokens","stateMutability":"view","inputs":[],"outputs":[{"type":"address[]"}]},
      {"type":"function","name":"redeemRewards","stateMutability":"nonpayable","inputs":[{"name":"user","type":"address"}],"outputs":[{"type":"uint256[]"}]}
    ]`)
	pendleStandardizedYieldABI = MustABI(`[
      {"type":"function","name":"exchangeRate","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"assetInfo","stateMutability":"view","inputs":[],"outputs":[{"name":"assetType","type":"uint8"},{"name":"assetAddress","type":"address"},{"name":"assetDecimals","type":"uint8"}]}
    ]`)
)

// pendleExchangeRateOne is the fixed-point one that IStandardizedYield.exchangeRate is scaled by.
var pendleExchangeRateOne = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// pendleImpliedRateYear is the year PendleMarket.readState annualises lastLnImpliedRate over.
// Pendle's own market math uses 365 days, so anything else here would misprice every PT.
const pendleImpliedRateYear = 365 * 24 * 60 * 60

// pendleSolvencyFactor is how much of its claim a PY pair can actually redeem.
//
// A PT redeems for 1/pyIndex of SY, and that SY is worth syIndex of the asset, so the pair is
// only whole while the SY exchange rate has kept up with the index it ratcheted to. When the
// underlying loses value — a slashing, a depeg, socialised bad debt — the rate falls below the
// index and every claim on it is impaired by exactly syIndex/pyIndex.
//
// Pendle applies this to both the PT and the YT rate its oracle publishes, and it does not stop
// at expiry: an expired PT still redeems through the same index, so parity is a property of a
// solvent SY rather than of expiry.
//
// PendlePYOracleLib derives pyIndex from three reads, taking pyIndexStored inside the block the
// yield token last cached it and max(syIndex, pyIndexStored) otherwise. Only pyIndexStored is
// read here because the two branches provably agree on this factor: where syIndex >= stored the
// cached branch gives pyIndex = stored <= syIndex and the fresh branch gives pyIndex = syIndex,
// so both clamp to one; where syIndex < stored both branches give pyIndex = stored. The
// distinction governs Pendle's own accrual, not the ratio of a claim to its asset, so paying two
// more calls per yield token for it would buy nothing.
func pendleSolvencyFactor(syIndex, pyIndexStored *big.Int) (*big.Int, error) {
	if syIndex == nil || pyIndexStored == nil ||
		syIndex.Sign() <= 0 || pyIndexStored.Sign() <= 0 {
		return nil, fmt.Errorf("Pendle SY and PY indices are not both positive")
	}
	if syIndex.Cmp(pyIndexStored) >= 0 {
		return new(big.Int).Set(priceBasisRatioOne), nil
	}
	factor := new(big.Int).Mul(syIndex, priceBasisRatioOne)
	factor.Div(factor, pyIndexStored)
	if factor.Sign() <= 0 {
		return nil, fmt.Errorf("Pendle solvency factor underflowed to zero")
	}
	return factor, nil
}

// pendleApplySolvency scales a raw PT or YT ratio by the factor. Rounding is down, so an impaired
// claim is never reported as worth more than it can redeem.
func pendleApplySolvency(ratio, factor *big.Int) *big.Int {
	scaled := new(big.Int).Mul(ratio, factor)
	return scaled.Div(scaled, priceBasisRatioOne)
}

// pendlePTToAssetRatio prices one PT in units of the SY's accounting asset, before the
// SY-solvency factor its caller applies.
//
// PT is a zero-coupon claim on that asset: it redeems one for one at expiry and trades below
// that beforehand, discounted at the market's implied rate. Pendle carries the rate as a natural
// log compounded continuously over a 365-day year, so the discount is
// exp(-lnImpliedRate * timeToExpiry / year) — the inverse of the exchange rate the AMM itself
// derives from the same two numbers.
//
// The underlying asset price alone will not do: it is the undiscounted redemption value, so it
// overstates a PT by exactly the yield still to accrue.
//
// float64 carries the exponential because the result only ever multiplies a float64 USD price.
// Its ~1e-16 of relative error on a discount factor is orders of magnitude below the agreement
// any price feed offers.
func pendlePTToAssetRatio(lnImpliedRate *big.Int, expiry, timestamp uint64) (*big.Int, error) {
	if expiry == 0 {
		return nil, fmt.Errorf("Pendle PT has no expiry")
	}
	if timestamp >= expiry {
		// Past expiry the market's last implied rate has stopped describing anything, so no
		// market is consulted at all. Parity here is the raw rate only: what an expired PT
		// actually redeems still depends on the SY being solvent.
		return new(big.Int).Set(priceBasisRatioOne), nil
	}
	if lnImpliedRate == nil || lnImpliedRate.Sign() < 0 {
		return nil, fmt.Errorf("Pendle market reported no usable implied rate")
	}
	rate, _ := new(big.Float).SetInt(lnImpliedRate).Float64()
	rate /= 1e18
	years := float64(expiry-timestamp) / float64(pendleImpliedRateYear)
	ratio := math.Exp(-rate * years)
	if ratio <= 0 || ratio > 1 || math.IsNaN(ratio) {
		return nil, fmt.Errorf(
			"Pendle implied rate discounts PT to an impossible %v of its asset", ratio,
		)
	}
	scaled, _ := new(big.Float).Mul(
		big.NewFloat(ratio), new(big.Float).SetInt(priceBasisRatioOne),
	).Int(nil)
	if scaled.Sign() <= 0 {
		return nil, fmt.Errorf("Pendle PT discount underflowed to zero")
	}
	return scaled, nil
}

// pendleYTToAssetRatio is the raw YT rate, in the same accounting asset. Minting and redeeming PY is
// permissionless in both directions before expiry, which holds one PT plus one YT at exactly one
// unit of the asset, so the YT is whatever the PT's discount leaves behind.
//
// At expiry that identity values a YT at nothing, which is not the same as knowing what it is
// worth: an expired YT can still hold interest that accrued before expiry and was never
// redeemed, and this adapter does not read it. Reporting zero would assert a fact we do not
// have, so such a YT gets no basis and stays unpriced.
func pendleYTToAssetRatio(ptRatio *big.Int) (*big.Int, bool) {
	if ptRatio == nil {
		return nil, false
	}
	ratio := new(big.Int).Sub(priceBasisRatioOne, ptRatio)
	if ratio.Sign() <= 0 {
		return nil, false
	}
	return ratio, true
}

type pendleTokenKind string

const (
	pendlePT pendleTokenKind = "pt"
	pendleYT pendleTokenKind = "yt"
	pendleLP pendleTokenKind = "lp"
)

func validPendleTokenKind(kind pendleTokenKind) bool {
	return kind == pendlePT || kind == pendleYT || kind == pendleLP
}

// pendleFactoryGeneration is one deployed factory pair. Pendle ships a new pair instead of
// upgrading in place, so several generations are live on a chain at once and each still owns
// PT/YT/LP tokens an account can hold. Both creation entry points are permissionless, so the
// pairs cannot be discovered on-chain and are anchored here.
//
// Membership is not "whatever emits the two topics": Pendle forks reuse the same event
// signatures (FiraMarketFactory on Ethereum, two unlinked emitters on Base). Every pair below
// was confirmed by marketFactory.yieldContractFactory() resolving back to the paired yield
// factory, a relation that holds 1:1 on all four chains.
type pendleFactoryGeneration struct {
	YieldContractFactory       common.Address
	YieldContractFactoryWindow deploymentWindow
	MarketFactory              common.Address
	MarketFactoryWindow        deploymentWindow
}

type pendleChainConfig struct {
	ChainID ChainID
	// ActivationBlock is the oldest generation's yield factory: before it no Pendle token
	// exists on the chain.
	ActivationBlock uint64
	Generations     []pendleFactoryGeneration
}

func pendleGeneration(
	yieldContractFactory string,
	yieldContractFactoryBlock uint64,
	marketFactory string,
	marketFactoryBlock uint64,
) pendleFactoryGeneration {
	return pendleFactoryGeneration{
		YieldContractFactory:       common.HexToAddress(yieldContractFactory),
		YieldContractFactoryWindow: deploymentWindow{ActivationBlock: yieldContractFactoryBlock},
		MarketFactory:              common.HexToAddress(marketFactory),
		MarketFactoryWindow:        deploymentWindow{ActivationBlock: marketFactoryBlock},
	}
}

// Every activation block is the first block at which the address has code, established by an
// eth_getCode binary search against an archive node.
var pendleChainConfigs = map[ChainID]pendleChainConfig{
	Ethereum: {
		ChainID: Ethereum, ActivationBlock: 16_032_048,
		Generations: []pendleFactoryGeneration{
			pendleGeneration("0x70ee0a6db4f5a2dc4d9c0b57be97b9987e75bafd", 16_032_048, "0x27b1dacd74688af24a64bd3c9c1b143118740784", 16_032_059),
			pendleGeneration("0xdf3601014686674e53d1fa52f7602525483f9122", 18_669_233, "0x1a6fcc85557bc4fb7b534ed835a03ef056552d52", 18_669_498),
			pendleGeneration("0x273b4bfa3bb30fe8f32c467b5f0046834557f072", 20_323_246, "0x3d75bd20c983edb5fd218a1b7e0024f1056c7a2f", 20_323_253),
			pendleGeneration("0x35a338522a435d46f77be32c70e215b813d0e3ac", 20_512_273, "0x6fcf753f2c67b83f7b09746bbc4fa0047b35d050", 20_512_280),
			pendleGeneration("0x3e6eba46abc5ab18ed95f6667d8b2fd4020e4637", 23_638_428, "0x6d247b1c044fa1e22e6b04fa9f71baf99eb29a9f", 23_638_439),
		},
	},
	BSC: {
		ChainID: BSC, ActivationBlock: 29_484_198,
		Generations: []pendleFactoryGeneration{
			pendleGeneration("0xa2530b4cfbf271e2b409a05c2ce520e4cb5fcc88", 29_484_198, "0x2bea6bfd8fbff45aa2a893eb3b6d85d10efcc70e", 29_484_286),
			pendleGeneration("0x40ae6da2d92aa3dcb7f8d7a7209fd12bdfcb7c85", 33_884_363, "0xc40febf5a33b8c92b187d9be0fd3fe0ac2e4b07c", 33_884_419),
			pendleGeneration("0xdb6380041441a94050199b4a46771d8d93553509", 40_539_569, "0x7d20e644d2a9e149e5be9be9ad2ab243a7835d37", 40_539_593),
			pendleGeneration("0xe006760020384a20774dea977c313ef5f51fe17d", 41_294_150, "0x7c7f73f7a320364dbb3c9aaa9bccd402040ee0f9", 41_294_178),
			pendleGeneration("0xd8c12d46dde7a04f782d417fae78516448cb2c5b", 65_608_948, "0x80ce46449df1c977f6ba60495125ce282f83ddfb", 65_609_031),
		},
	},
	Base: {
		ChainID: Base, ActivationBlock: 22_350_319,
		Generations: []pendleFactoryGeneration{
			pendleGeneration("0x963ddbb35c1ae44e2a159e3b5fb5177e0b32660d", 22_350_319, "0x59968008a703dc13e6beaeced644bdce4ee45d13", 22_350_352),
			pendleGeneration("0xddbfa21ecf024971486684e4e1600998adeabc88", 37_206_651, "0x81e80a50e56d10c501ff17b5fe2f662bd9ea4590", 37_206_684),
		},
	},
	Arbitrum: {
		ChainID: Arbitrum, ActivationBlock: 62_977_844,
		Generations: []pendleFactoryGeneration{
			pendleGeneration("0x28de02ac3c3f5ef427e55c321f73fdc7f192e8e4", 62_977_844, "0xf5a7de2d276dbda3eef1b62a9e718eff4d29ddc8", 62_979_673),
			pendleGeneration("0xeb38531db128eca928aea1b1ce9e5609b15ba146", 154_873_257, "0x2fcb47b58350cd377f94d3821e7373df60bd9ced", 154_873_897),
			pendleGeneration("0xc7f8f9f1dde1104664b6fc8f33e49b169c12f41e", 233_004_669, "0xd9f5e9589016da862d2abce980a5a5b99a94f3e8", 233_004_891),
			pendleGeneration("0xff29e023910fb9bfc86729c1050af193a45a0c0c", 242_035_795, "0xd29e76c6f15ada0150d10a1d3f45accd2098283b", 242_035_998),
			pendleGeneration("0xba814bf6e27a6d6bae4a8ac65c8bc3d8e9b0aacf", 392_471_043, "0x49f2f7002669e0e4425fa0203975625ab4af3143", 392_471_311),
		},
	},
}

// pendlePositionRef is one Pendle ERC20 the account has touched. Whether the balance is still
// positive is decided by balanceOf at the pinned block, never by the index.
type pendlePositionRef struct {
	Token        common.Address
	Kind         pendleTokenKind
	PT           common.Address
	SY           common.Address
	Expiry       uint64
	CreatedBlock uint64
}

type pendlePositionIndexer interface {
	PositionRefs(
		context.Context,
		*RPCClient,
		BlockRef,
		common.Address,
	) ([]pendlePositionRef, error)
	// MarketsForPT groups the markets the index recorded for each of the given PTs. A PT names
	// no market on-chain, and Pendle allows more than one, so this is the only way to reach the
	// market whose implied rate discounts it.
	MarketsForPT(
		context.Context,
		BlockRef,
		[]common.Address,
	) (map[common.Address][]common.Address, error)
}

type PendleAdapter struct {
	adapterBase
	indexer pendlePositionIndexer
}

func newPendleAdapter(config SentioIndexerConfig) Adapter {
	return newPendleAdapterWithIndexer(newPendleIndexer(config))
}

func newPendleAdapterWithIndexer(indexer pendlePositionIndexer) *PendleAdapter {
	return &PendleAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "pendle", Name: "Pendle V2", Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		indexer: indexer,
	}
}

func pendleGroupLabel(kind pendleTokenKind) string {
	switch kind {
	case pendlePT:
		return "Principal Token"
	case pendleYT:
		return "Yield Token"
	default:
		return "Liquidity"
	}
}

func (a *PendleAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	chain, supported := pendleChainConfigs[block.ChainID]
	if !supported || block.Number < chain.ActivationBlock {
		return nil, nil
	}
	refs, err := a.indexer.PositionRefs(ctx, client, block, account)
	if err != nil {
		return nil, fmt.Errorf("position enumeration: %w", err)
	}
	held, err := a.heldRefs(ctx, client, block, account, refs)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, nil
	}
	return a.buildGroups(ctx, client, block, held)
}

type pendleHolding struct {
	ref     pendlePositionRef
	account common.Address
	balance *big.Int
}

// heldRefs drops every reference whose balance is zero at the pinned block. The index only
// records that an account once received the token; redemptions, sales and expiries are all
// invisible to it, so the balance read is what decides.
func (a *PendleAdapter) heldRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	refs []pendlePositionRef,
) ([]pendleHolding, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(refs))
	for index, ref := range refs {
		calls[index] = ContractCall{
			Contract: ref.Token, ABI: pendlePrincipalTokenABI,
			Method: "balanceOf", Args: []any{account},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle balances: %w", err)
	}
	held := make([]pendleHolding, 0, len(refs))
	for index, row := range rows {
		balance, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle %s balance: %w", refs[index].Token, decodeErr)
		}
		if balance.Sign() == 0 {
			continue
		}
		held = append(held, pendleHolding{ref: refs[index], account: account, balance: balance})
	}
	return held, nil
}

func (a *PendleAdapter) buildGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) ([]Group, error) {
	groups := make([]Group, 0, len(held))
	direct := make([]pendleHolding, 0, len(held))
	markets := make([]pendleHolding, 0, len(held))
	for _, holding := range held {
		if holding.ref.Kind == pendleLP {
			markets = append(markets, holding)
			continue
		}
		direct = append(direct, holding)
	}
	directGroups, err := a.tokenGroups(ctx, client, block, direct)
	if err != nil {
		return nil, err
	}
	groups = append(groups, directGroups...)
	marketGroups, err := a.marketGroups(ctx, client, block, markets)
	if err != nil {
		return nil, err
	}
	groups = append(groups, marketGroups...)
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, nil
}

func pendleMetadata(ref pendlePositionRef) map[string]any {
	metadata := map[string]any{"kind": string(ref.Kind)}
	if ref.PT != (common.Address{}) {
		metadata["pt"] = ref.PT
	}
	if ref.SY != (common.Address{}) {
		metadata["sy"] = ref.SY
	}
	if ref.Expiry != 0 {
		metadata["expiry"] = strconv.FormatUint(ref.Expiry, 10)
	}
	return metadata
}

// tokenGroups reports PT and YT as the token itself: the holding is the ERC20 balance, which is
// also the basis DeBank prices those positions on.
func (a *PendleAdapter) tokenGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) ([]Group, error) {
	if len(held) == 0 {
		return nil, nil
	}
	bases, err := a.tokenPriceBases(ctx, client, block, held)
	if err != nil {
		return nil, err
	}
	addresses := make([]common.Address, 0, len(held)+len(bases))
	for _, holding := range held {
		addresses = append(addresses, holding.ref.Token)
	}
	for _, basis := range bases {
		addresses = append(addresses, basis.asset)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, addresses)
	if err != nil {
		return nil, fmt.Errorf("Pendle token metadata: %w", err)
	}
	groups := make([]Group, 0, len(held))
	for _, holding := range held {
		token, exists := tokens[holding.ref.Token]
		if !exists {
			return nil, fmt.Errorf("Pendle token %s metadata is absent", holding.ref.Token)
		}
		component := NewComponent("asset", token, holding.balance,
			Source{Contract: holding.ref.Token, Method: "balanceOf"})
		if basis, priced := bases[holding.ref.Token]; priced {
			assetToken, known := tokens[basis.asset]
			if !known {
				return nil, fmt.Errorf("Pendle asset %s metadata is absent", basis.asset)
			}
			component.PriceBasis = &PriceBasis{
				Token: assetToken, RatioRaw: basis.ratio.String(),
			}
		}
		groups = append(groups, Group{
			ID:         string(holding.ref.Kind) + ":" + strings.ToLower(holding.ref.Token.Hex()),
			MarketID:   strings.ToLower(holding.ref.PT.Hex()),
			Label:      pendleGroupLabel(holding.ref.Kind),
			Components: []Component{component},
			Metadata:   pendleMetadata(holding.ref),
		})
	}
	return groups, nil
}

// pendleTokenBasis is the accounting asset one directly held PT or YT prices through, with the
// ratio between a unit of that token and a unit of the asset.
type pendleTokenBasis struct {
	asset common.Address
	ratio *big.Int
}

// tokenPriceBases resolves what discounts each directly held PT and YT to a quoted asset.
//
// Neither is quoted anywhere. A new PT/YT pair appears whenever anyone calls the permissionless
// createYieldContract, so no price registry keeps up, and the same reason a curated token list
// would rot is the reason one cannot be priced by listing it. Both are claims on the SY's
// accounting asset, which is quoted, at a ratio the PT's market publishes.
//
// Anything that cannot be established leaves the token with its own unquoted identity and no
// basis. That is the behaviour that shipped: an unpriced component is a gap the response reports,
// whereas a guessed one is a wrong number nobody can see is wrong.
func (a *PendleAdapter) tokenPriceBases(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) (map[common.Address]pendleTokenBasis, error) {
	seenSY := make(map[common.Address]struct{}, len(held))
	seenPT := make(map[common.Address]struct{}, len(held))
	syAddresses := make([]common.Address, 0, len(held))
	unexpired := make([]common.Address, 0, len(held))
	for _, holding := range held {
		if holding.ref.SY == (common.Address{}) || holding.ref.PT == (common.Address{}) {
			continue
		}
		if _, exists := seenSY[holding.ref.SY]; !exists {
			seenSY[holding.ref.SY] = struct{}{}
			syAddresses = append(syAddresses, holding.ref.SY)
		}
		// An expired PT needs no market: it redeems for the asset one for one, so asking the
		// index for markets it no longer prices would only add a round trip.
		if holding.ref.Expiry == 0 || holding.ref.Expiry <= block.Timestamp {
			continue
		}
		if _, exists := seenPT[holding.ref.PT]; exists {
			continue
		}
		seenPT[holding.ref.PT] = struct{}{}
		unexpired = append(unexpired, holding.ref.PT)
	}
	if len(syAddresses) == 0 {
		return nil, nil
	}
	rates, err := a.marketImpliedRates(ctx, client, block, unexpired)
	if err != nil {
		return nil, err
	}
	syStates, err := pendleSYStates(ctx, client, block, syAddresses)
	if err != nil {
		return nil, err
	}
	yieldTokens, err := pendleYieldTokensFor(ctx, client, block, held)
	if err != nil {
		return nil, err
	}
	distinct := make([]common.Address, 0, len(yieldTokens))
	seenYT := make(map[common.Address]struct{}, len(yieldTokens))
	for _, yieldToken := range yieldTokens {
		if _, exists := seenYT[yieldToken]; exists {
			continue
		}
		seenYT[yieldToken] = struct{}{}
		distinct = append(distinct, yieldToken)
	}
	pyIndices, err := pendlePYIndices(ctx, client, block, distinct)
	if err != nil {
		return nil, err
	}
	bases := make(map[common.Address]pendleTokenBasis, len(held))
	for _, holding := range held {
		sy, known := syStates[holding.ref.SY]
		if !known {
			continue
		}
		pyIndexStored, indexed := pyIndices[yieldTokens[holding.ref.Token]]
		if !indexed {
			continue
		}
		factor, factorErr := pendleSolvencyFactor(sy.exchangeRate, pyIndexStored)
		if factorErr != nil {
			continue
		}
		ptRatio, ratioErr := pendlePTToAssetRatio(
			rates[holding.ref.PT], holding.ref.Expiry, block.Timestamp,
		)
		if ratioErr != nil {
			continue
		}
		raw := ptRatio
		if holding.ref.Kind == pendleYT {
			ytRatio, usable := pendleYTToAssetRatio(ptRatio)
			if !usable {
				continue
			}
			raw = ytRatio
		}
		ratio := pendleApplySolvency(raw, factor)
		if ratio.Sign() <= 0 {
			continue
		}
		bases[holding.ref.Token] = pendleTokenBasis{asset: sy.asset, ratio: ratio}
	}
	return bases, nil
}

// pendleYieldTokensFor maps each directly held token to the yield token that carries its PY
// index. A held YT is its own; a held PT names its sibling through PT.YT(), which is the only
// place the pairing is available — the index records the PT a token belongs to, never the YT.
func pendleYieldTokensFor(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) (map[common.Address]common.Address, error) {
	yieldTokens := make(map[common.Address]common.Address, len(held))
	principals := make([]common.Address, 0, len(held))
	for _, holding := range held {
		if holding.ref.Kind == pendleYT {
			yieldTokens[holding.ref.Token] = holding.ref.Token
			continue
		}
		principals = append(principals, holding.ref.Token)
	}
	if len(principals) == 0 {
		return yieldTokens, nil
	}
	calls := make([]ContractCall, len(principals))
	for index, principal := range principals {
		calls[index] = ContractCall{
			Contract: principal, ABI: pendlePrincipalTokenABI, Method: "YT",
		}
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle yield token: %w", err)
	}
	for index, row := range rows {
		if row.Error != nil {
			continue
		}
		yieldToken, decodeErr := AddressAt(row.Values, 0)
		if decodeErr != nil || yieldToken == (common.Address{}) {
			continue
		}
		yieldTokens[principals[index]] = yieldToken
	}
	return yieldTokens, nil
}

// marketImpliedRates picks one market per PT and returns the implied rate it publishes.
//
// A PT can have several markets — 32 of the 874 the index knows do, up to three for one PT — and
// none of them is canonical on-chain, so the deepest by SY reserve wins: that is the rate the
// most capital has agreed to. Ties break on the lowest address, so the choice never depends on
// map iteration order. A market that reverts, holds no liquidity or reports no rate is skipped
// rather than failing the scan, because the PT it prices is only one component of one group.
func (a *PendleAdapter) marketImpliedRates(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	pts []common.Address,
) (map[common.Address]*big.Int, error) {
	if len(pts) == 0 {
		return nil, nil
	}
	markets, err := a.indexer.MarketsForPT(ctx, block, pts)
	if err != nil {
		return nil, fmt.Errorf("Pendle market lookup: %w", err)
	}
	type marketCandidate struct {
		pt     common.Address
		market common.Address
	}
	candidates := make([]marketCandidate, 0, len(pts))
	for _, pt := range pts {
		for _, market := range markets[pt] {
			candidates = append(candidates, marketCandidate{pt: pt, market: market})
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(candidates))
	for index, candidate := range candidates {
		calls[index] = ContractCall{
			Contract: candidate.market, ABI: pendleMarketABI, Method: "readState",
			Args: []any{common.Address{}},
		}
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle market rates: %w", err)
	}
	rates := make(map[common.Address]*big.Int, len(pts))
	depths := make(map[common.Address]*big.Int, len(pts))
	chosen := make(map[common.Address]common.Address, len(pts))
	for index, row := range rows {
		if row.Error != nil {
			continue
		}
		totalSy, decodeErr := BigIntAt(row.Values, 1)
		if decodeErr != nil || totalSy.Sign() <= 0 {
			continue
		}
		totalLp, decodeErr := BigIntAt(row.Values, 2)
		if decodeErr != nil || totalLp.Sign() <= 0 {
			continue
		}
		lnImpliedRate, decodeErr := BigIntAt(row.Values, 8)
		if decodeErr != nil || lnImpliedRate.Sign() <= 0 {
			continue
		}
		candidate := candidates[index]
		if deepest, exists := depths[candidate.pt]; exists {
			comparison := totalSy.Cmp(deepest)
			if comparison < 0 || (comparison == 0 && bytes.Compare(
				candidate.market.Bytes(), chosen[candidate.pt].Bytes(),
			) >= 0) {
				continue
			}
		}
		depths[candidate.pt] = totalSy
		chosen[candidate.pt] = candidate.market
		rates[candidate.pt] = lnImpliedRate
	}
	return rates, nil
}

// pendleSYState is what one SY contributes to pricing its PT and YT: the asset it denominates
// itself in — the token they redeem for and, unlike them, one a price registry quotes — and the
// exchange rate that decides whether those claims are still whole.
type pendleSYState struct {
	asset        common.Address
	exchangeRate *big.Int
}

// pendleSYStates reads both per SY. An SY that does not answer either call leaves its tokens
// unpriced instead of failing the scan.
func pendleSYStates(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	syAddresses []common.Address,
) (map[common.Address]pendleSYState, error) {
	calls := make([]ContractCall, 0, len(syAddresses)*2)
	for _, sy := range syAddresses {
		calls = append(calls,
			ContractCall{Contract: sy, ABI: pendleStandardizedYieldABI, Method: "assetInfo"},
			ContractCall{Contract: sy, ABI: pendleStandardizedYieldABI, Method: "exchangeRate"},
		)
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle SY state: %w", err)
	}
	states := make(map[common.Address]pendleSYState, len(syAddresses))
	for index, sy := range syAddresses {
		assetRow, rateRow := rows[index*2], rows[index*2+1]
		if assetRow.Error != nil || rateRow.Error != nil {
			continue
		}
		asset, decodeErr := AddressAt(assetRow.Values, 1)
		if decodeErr != nil || asset == (common.Address{}) {
			continue
		}
		rate, decodeErr := BigIntAt(rateRow.Values, 0)
		if decodeErr != nil || rate.Sign() <= 0 {
			continue
		}
		states[sy] = pendleSYState{asset: asset, exchangeRate: rate}
	}
	return states, nil
}

// pendlePYIndices reads the index each yield token has ratcheted to. A yield token that does not
// answer leaves its pair unpriced: assuming a solvent SY is the one assumption that would
// overstate exactly the positions whose value has fallen.
func pendlePYIndices(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	yieldTokens []common.Address,
) (map[common.Address]*big.Int, error) {
	calls := make([]ContractCall, len(yieldTokens))
	for index, yieldToken := range yieldTokens {
		calls[index] = ContractCall{
			Contract: yieldToken, ABI: pendleYieldTokenABI, Method: "pyIndexStored",
		}
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle PY index: %w", err)
	}
	indices := make(map[common.Address]*big.Int, len(yieldTokens))
	for index, row := range rows {
		if row.Error != nil {
			continue
		}
		stored, decodeErr := BigIntAt(row.Values, 0)
		if decodeErr != nil || stored.Sign() <= 0 {
			continue
		}
		indices[yieldTokens[index]] = stored
	}
	return indices, nil
}

type pendleMarketState struct {
	holding      pendleHolding
	sy           common.Address
	pt           common.Address
	yt           common.Address
	totalPt      *big.Int
	totalSy      *big.Int
	totalLp      *big.Int
	exchangeRate *big.Int
	// expiry and lnImpliedRate come from the same readState call as the reserves and are what
	// discount the PT leg to its accounting asset, so the LP path needs no extra round trip.
	expiry        uint64
	lnImpliedRate *big.Int
	// solvency is the SY-solvency factor for this market's PY pair, nil when it could not be
	// established. It discounts the PT reserve leg exactly as it discounts a directly held PT.
	solvency *big.Int
	asset    common.Address
	// assetDecimals is what the SY itself declares its accounting asset to be. exchangeRate
	// converts SY units to asset units at the same decimal base, so a SY whose own decimals
	// disagree with it would silently scale the result.
	assetDecimals uint8
	rewardTokens  []common.Address
	rewards       []*big.Int
}

// readMarketStates gathers everything a market position decomposes through. readTokens is read
// at the pinned block rather than carried by the index because the market event names only the
// market and its PT.
func (a *PendleAdapter) readMarketStates(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) ([]pendleMarketState, error) {
	calls := make([]ContractCall, 0, len(held)*3)
	for _, holding := range held {
		calls = append(calls,
			ContractCall{Contract: holding.ref.Token, ABI: pendleMarketABI, Method: "readTokens"},
			ContractCall{Contract: holding.ref.Token, ABI: pendleMarketABI, Method: "readState",
				Args: []any{common.Address{}}},
			ContractCall{Contract: holding.ref.Token, ABI: pendleMarketABI, Method: "getRewardTokens"},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle market state: %w", err)
	}
	states := make([]pendleMarketState, 0, len(held))
	for index, holding := range held {
		market := holding.ref.Token
		sy, decodeErr := AddressAt(rows[index*3], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s tokens: %w", market, decodeErr)
		}
		pt, decodeErr := AddressAt(rows[index*3], 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s tokens: %w", market, decodeErr)
		}
		yieldToken, decodeErr := AddressAt(rows[index*3], 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s tokens: %w", market, decodeErr)
		}
		totalPt, decodeErr := BigIntAt(rows[index*3+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s state: %w", market, decodeErr)
		}
		totalSy, decodeErr := BigIntAt(rows[index*3+1], 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s state: %w", market, decodeErr)
		}
		totalLp, decodeErr := BigIntAt(rows[index*3+1], 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s state: %w", market, decodeErr)
		}
		if totalLp.Sign() <= 0 || totalPt.Sign() < 0 || totalSy.Sign() < 0 {
			return nil, fmt.Errorf("Pendle market %s reported a non-positive reserve state", market)
		}
		expiry, decodeErr := BigIntAt(rows[index*3+1], 5)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s expiry: %w", market, decodeErr)
		}
		if !expiry.IsUint64() {
			return nil, fmt.Errorf("Pendle market %s reported an out-of-range expiry", market)
		}
		lnImpliedRate, decodeErr := BigIntAt(rows[index*3+1], 8)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s implied rate: %w", market, decodeErr)
		}
		rewardTokens, decodeErr := AddressSliceAt(rows[index*3+2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle market %s reward tokens: %w", market, decodeErr)
		}
		states = append(states, pendleMarketState{
			holding: holding, sy: sy, pt: pt, yt: yieldToken,
			totalPt: totalPt, totalSy: totalSy, totalLp: totalLp,
			expiry: expiry.Uint64(), lnImpliedRate: lnImpliedRate,
			rewardTokens: rewardTokens,
		})
	}
	return a.readMarketSY(ctx, client, block, states)
}

func (a *PendleAdapter) readMarketSY(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	states []pendleMarketState,
) ([]pendleMarketState, error) {
	calls := make([]ContractCall, 0, len(states)*2)
	for _, state := range states {
		calls = append(calls,
			ContractCall{Contract: state.sy, ABI: pendleStandardizedYieldABI, Method: "exchangeRate"},
			ContractCall{Contract: state.sy, ABI: pendleStandardizedYieldABI, Method: "assetInfo"},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle SY state: %w", err)
	}
	for index := range states {
		rate, decodeErr := BigIntAt(rows[index*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle SY %s exchange rate: %w", states[index].sy, decodeErr)
		}
		if rate.Sign() <= 0 {
			return nil, fmt.Errorf("Pendle SY %s reported a non-positive exchange rate", states[index].sy)
		}
		asset, decodeErr := AddressAt(rows[index*2+1], 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle SY %s asset: %w", states[index].sy, decodeErr)
		}
		decimals, decodeErr := Uint8At(rows[index*2+1], 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("Pendle SY %s asset decimals: %w", states[index].sy, decodeErr)
		}
		states[index].exchangeRate = rate
		states[index].asset = asset
		states[index].assetDecimals = decimals
	}
	return a.readMarketSolvency(ctx, client, block, states)
}

// readMarketSolvency establishes each market's SY-solvency factor from its yield token's PY
// index. readTokens already named the yield token, so one call per market is the only extra read
// a liquidity position needs, and a market whose yield token does not answer simply loses the
// basis on its PT leg — the reserve amounts and the SY leg are unaffected.
func (a *PendleAdapter) readMarketSolvency(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	states []pendleMarketState,
) ([]pendleMarketState, error) {
	yieldTokens := make([]common.Address, 0, len(states))
	seen := make(map[common.Address]struct{}, len(states))
	for _, state := range states {
		if state.yt == (common.Address{}) {
			continue
		}
		if _, exists := seen[state.yt]; exists {
			continue
		}
		seen[state.yt] = struct{}{}
		yieldTokens = append(yieldTokens, state.yt)
	}
	if len(yieldTokens) == 0 {
		return a.readMarketRewards(ctx, client, block, states)
	}
	pyIndices, err := pendlePYIndices(ctx, client, block, yieldTokens)
	if err != nil {
		return nil, err
	}
	for index := range states {
		pyIndexStored, indexed := pyIndices[states[index].yt]
		if !indexed {
			continue
		}
		factor, factorErr := pendleSolvencyFactor(states[index].exchangeRate, pyIndexStored)
		if factorErr != nil {
			continue
		}
		states[index].solvency = factor
	}
	return a.readMarketRewards(ctx, client, block, states)
}

// readMarketRewards simulates redeemRewards to read what the position has accrued. The accrued
// field of the userReward view only holds what was indexed at the holder's last interaction and
// reads zero for a position that has simply been sitting, so it cannot be used instead. A market
// that reverts loses only its reward component: the SY and PT legs are already known.
func (a *PendleAdapter) readMarketRewards(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	states []pendleMarketState,
) ([]pendleMarketState, error) {
	calls := make([]ContractCall, 0, len(states))
	claimants := make([]int, 0, len(states))
	for index, state := range states {
		if len(state.rewardTokens) == 0 {
			continue
		}
		calls = append(calls, ContractCall{
			Contract: state.holding.ref.Token, ABI: pendleMarketABI,
			Method: "redeemRewards", Args: []any{state.holding.account},
		})
		claimants = append(claimants, index)
	}
	if len(calls) == 0 {
		return states, nil
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Pendle market rewards: %w", err)
	}
	for position, row := range rows {
		if row.Error != nil {
			continue
		}
		amounts, decodeErr := BigIntSliceAt(row.Values, 0)
		if decodeErr != nil {
			continue
		}
		state := &states[claimants[position]]
		if len(amounts) != len(state.rewardTokens) {
			continue
		}
		state.rewards = amounts
	}
	return states, nil
}

// pendleReserveShare is the holder's slice of one market reserve. Rounding is down, so the sum
// of every holder's share can never exceed the reserve.
func pendleReserveShare(balance, reserve, totalLp *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(balance, reserve), totalLp)
}

// pendleSYToAsset converts SY units into the SY's accounting asset. exchangeRate is asset per SY
// in 1e18 fixed point, with both sides on the same decimal base.
func pendleSYToAsset(syAmount, exchangeRate *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(syAmount, exchangeRate), pendleExchangeRateOne)
}

// marketGroups decomposes an LP balance into the holder's share of the market's SY and PT
// reserves, converting the SY leg through the SY's own exchange rate into its accounting asset.
// This is the basis DeBank reports a Pendle liquidity position on, and it prices through tokens
// the registry already knows rather than the LP token, which nothing quotes.
func (a *PendleAdapter) marketGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []pendleHolding,
) ([]Group, error) {
	if len(held) == 0 {
		return nil, nil
	}
	states, err := a.readMarketStates(ctx, client, block, held)
	if err != nil {
		return nil, err
	}
	addresses := make([]common.Address, 0, len(states)*3)
	for _, state := range states {
		addresses = append(addresses, state.asset, state.pt, state.sy)
		addresses = append(addresses, state.rewardTokens...)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, addresses)
	if err != nil {
		return nil, fmt.Errorf("Pendle market token metadata: %w", err)
	}
	groups := make([]Group, 0, len(states))
	for _, state := range states {
		market := state.holding.ref.Token
		syToken, exists := tokens[state.sy]
		if !exists {
			return nil, fmt.Errorf("Pendle SY %s metadata is absent", state.sy)
		}
		if syToken.Decimals != state.assetDecimals {
			return nil, fmt.Errorf(
				"Pendle SY %s has %d decimals but reports a %d-decimal asset",
				state.sy, syToken.Decimals, state.assetDecimals,
			)
		}
		assetToken, exists := tokens[state.asset]
		if !exists {
			return nil, fmt.Errorf("Pendle market %s asset metadata is absent", market)
		}
		ptToken, exists := tokens[state.pt]
		if !exists {
			return nil, fmt.Errorf("Pendle market %s PT metadata is absent", market)
		}
		share := state.holding.balance
		ptAmount := pendleReserveShare(share, state.totalPt, state.totalLp)
		syAmount := pendleReserveShare(share, state.totalSy, state.totalLp)
		assetAmount := pendleSYToAsset(syAmount, state.exchangeRate)
		components := make([]Component, 0, 2+len(state.rewardTokens))
		if assetAmount.Sign() > 0 {
			components = append(components, NewComponent("asset", assetToken, assetAmount,
				Source{Contract: market, Method: "readState(totalSy)*exchangeRate"}))
		}
		if ptAmount.Sign() > 0 {
			ptComponent := NewComponent("asset", ptToken, ptAmount,
				Source{Contract: market, Method: "readState(totalPt)"})
			// The reserve is reported as the PT itself, the way DeBank reports it, and priced
			// through the same accounting asset as the SY leg. Both legs of a liquidity position
			// therefore value, where before only the SY leg did.
			if raw, ratioErr := pendlePTToAssetRatio(
				state.lnImpliedRate, state.expiry, block.Timestamp,
			); ratioErr == nil && state.solvency != nil {
				if ratio := pendleApplySolvency(raw, state.solvency); ratio.Sign() > 0 {
					ptComponent.PriceBasis = &PriceBasis{
						Token: assetToken, RatioRaw: ratio.String(),
					}
				}
			}
			components = append(components, ptComponent)
		}
		for index, rewardToken := range state.rewardTokens {
			if index >= len(state.rewards) || state.rewards[index].Sign() == 0 {
				continue
			}
			metadata, known := tokens[rewardToken]
			if !known {
				return nil, fmt.Errorf("Pendle market %s reward metadata is absent", market)
			}
			components = append(components, NewComponent("reward", metadata, state.rewards[index],
				Source{Contract: market, Method: "redeemRewards"}))
		}
		if len(components) == 0 {
			continue
		}
		metadata := pendleMetadata(state.holding.ref)
		metadata["market"] = market
		metadata["sy"] = state.sy
		metadata["lpBalance"] = share.String()
		groups = append(groups, Group{
			ID:         string(pendleLP) + ":" + strings.ToLower(market.Hex()),
			MarketID:   strings.ToLower(state.pt.Hex()),
			Label:      pendleGroupLabel(pendleLP),
			Components: components,
			Metadata:   metadata,
		})
	}
	return groups, nil
}
