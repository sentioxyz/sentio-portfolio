package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Polymarket holds every outcome of every market as an ERC-1155 token of one Conditional Tokens
// contract on Polygon. A share of an outcome pays the outcome's payout in collateral once the
// market resolves, and trades on Polymarket's order book until then.
//
// The account is the address that holds the shares: the wallet polymarket.com shows on a profile
// (a deposit wallet, or a legacy proxy or Safe), not the key that signs for it.
//
// Its index (polymarketIndexer) lists, per wallet, every outcome token any transfer moved in or
// out of it since the index's start block. The index only decides which tokens to read: each
// amount is the contract's balanceOf at the pin, and the condition and outcome the index names
// for a token are checked against the token's id before the share is reported. A token the index
// never saw — held since before its start and not moved since — is not read.

const (
	// polymarketIndexStart is the processor's start block, the day the exchanges' second version
	// launched. The adapter reads nothing before it.
	polymarketIndexStart = 86_107_178
	// polymarketMaxIndexedPositions bounds the outcome tokens one wallet's index rows may name as
	// possibly held. A resolved market's share stays held until redeemed, so a trader who never
	// redeems a lost bet runs to the thousands.
	polymarketMaxIndexedPositions = 25_000
	// polymarketBalanceBatch is how many tokens one balanceOfBatch call reads. Every id is a storage
	// read, and a JSON-RPC batch of calls shares one request's deadline (rpcCallTimeout).
	polymarketBalanceBatch = 100
	// polymarketShareDecimals is the scale of an outcome share: a share of a market splits from
	// one unit of its collateral, so the two share the collateral's six decimals.
	polymarketShareDecimals = 6
	// polymarketDustUnits is Polymarket's dust floor, a tenth of a share: its positions API, which
	// polymarket.com and DeBank show, leaves out a holding of no more than that. Market making
	// leaves an active wallet hundreds of such remainders, each worth ten cents at most, so the
	// adapter leaves them out too rather than read, report and quote each one.
	polymarketDustUnits = 100_000
)

var (
	// polymarketCTF is the Conditional Tokens contract every outcome token lives in. Its creation
	// block is closed history, found by eth_getCode binary search.
	polymarketCTF       = common.HexToAddress("0x4D97DCd97eC945f40cF65F87097ACe5EA0476045")
	polymarketCTFWindow = deploymentWindow{ActivationBlock: 4_023_686}
	// polymarketUSDCe is the collateral every condition settles in: directly for a standard
	// market, through the neg-risk adapter's wrapper otherwise.
	polymarketUSDCe = Token{
		ChainID: Polygon, Address: common.HexToAddress("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"),
		Symbol: "USDC.e", Decimals: 6,
	}
	// polymarketWrappedCollateral is the neg-risk adapter's collateral token, which wraps USDC.e
	// one for one. A neg-risk market's outcome tokens are positions in it.
	polymarketWrappedCollateral = common.HexToAddress("0x3A3BD7bb9528E159577F7C2e685CC81A765002E2")
	// polymarketMulticall is Multicall3, which reads the contract's per-condition views in bulk.
	// Its creation block is closed history, found by eth_getCode binary search.
	polymarketMulticall       = common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")
	polymarketMulticallWindow = deploymentWindow{ActivationBlock: 25_770_160}
)

// polymarketViewBatch is how many contract views one Multicall3 call carries. A wallet that never
// redeems a lost bet still holds its shares, so a trader's resolutions run to the thousands.
const polymarketViewBatch = 100

const (
	// polymarketRPCBatch is the calls one JSON-RPC batch carries, as RPCClient.ParallelCalls sends
	// them.
	polymarketRPCBatch = 16
	// polymarketRPCConcurrency is how many of one wallet's JSON-RPC batches are in flight at once.
	// A trader holding thousands of tokens needs tens of batches for its balances and as many for
	// its resolutions, which one after another outlast an interactive scan.
	polymarketRPCConcurrency = 8
)

// polymarketCalls makes calls at the pin, polymarketRPCBatch to a JSON-RPC batch and
// polymarketRPCConcurrency batches at once, and returns their results in the calls' order.
func polymarketCalls(ctx context.Context, client *RPCClient, block BlockRef, calls []ContractCall) ([][]any, error) {
	if len(calls) <= polymarketRPCBatch {
		return client.ParallelCalls(ctx, block, calls)
	}
	results := make([][]any, len(calls))
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, polymarketRPCConcurrency)
	var (
		wait  sync.WaitGroup
		once  sync.Once
		first error
	)
	for start := 0; start < len(calls); start += polymarketRPCBatch {
		end := min(start+polymarketRPCBatch, len(calls))
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case slots <- struct{}{}:
			case <-callCtx.Done():
				return
			}
			defer func() { <-slots }()
			rows, err := client.ParallelCalls(callCtx, block, calls[start:end])
			if err != nil {
				once.Do(func() {
					first = fmt.Errorf("batch from call %d: %w", start, err)
					cancel()
				})
				return
			}
			copy(results[start:end], rows)
		}()
	}
	wait.Wait()
	// A batch ended by another's failure reports nothing; one ended by the caller's own deadline
	// reports that.
	if first != nil && (ctx.Err() == nil || !errors.Is(first, context.Canceled)) {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

var polymarketMulticallABI = MustABI(`[
  {"type":"function","name":"aggregate3","stateMutability":"payable",
   "inputs":[{"name":"calls","type":"tuple[]","components":[
     {"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}]}],
   "outputs":[{"name":"returnData","type":"tuple[]","components":[
     {"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}]}]}
]`)

type polymarketMulticallCall struct {
	Target       common.Address
	AllowFailure bool
	CallData     []byte
}

type polymarketMulticallResult struct {
	Success    bool
	ReturnData []byte
}

// polymarketViews reads views of the Conditional Tokens contract at the pin through Multicall3,
// polymarketViewBatch to a call, and decodes each as a direct call would be. A view that reverts
// fails the read, as it would directly.
func polymarketViews(ctx context.Context, client *RPCClient, block BlockRef, views []ContractCall) ([][]any, error) {
	calls := make([]ContractCall, 0, (len(views)+polymarketViewBatch-1)/polymarketViewBatch)
	for start := 0; start < len(views); start += polymarketViewBatch {
		chunk := views[start:min(start+polymarketViewBatch, len(views))]
		packed := make([]polymarketMulticallCall, len(chunk))
		for index, view := range chunk {
			data, err := view.ABI.Pack(view.Method, view.Args...)
			if err != nil {
				return nil, fmt.Errorf("view %d encode %s: %w", start+index, view.Method, err)
			}
			packed[index] = polymarketMulticallCall{Target: view.Contract, CallData: data}
		}
		calls = append(calls, ContractCall{
			Contract: polymarketMulticall, ABI: polymarketMulticallABI, Method: "aggregate3", Args: []any{packed},
		})
	}
	rows, err := polymarketCalls(ctx, client, block, calls)
	if err != nil {
		return nil, err
	}
	values := make([][]any, 0, len(views))
	for call, row := range rows {
		chunk := views[call*polymarketViewBatch : min((call+1)*polymarketViewBatch, len(views))]
		var results []polymarketMulticallResult
		if len(row) != 1 {
			return nil, fmt.Errorf("Multicall3 returned %d values", len(row))
		}
		if converted, ok := abi.ConvertType(row[0], new([]polymarketMulticallResult)).(*[]polymarketMulticallResult); ok {
			results = *converted
		}
		if len(results) != len(chunk) {
			return nil, fmt.Errorf("Multicall3 returned %d results for %d views", len(results), len(chunk))
		}
		for index, result := range results {
			view := chunk[index]
			if !result.Success {
				return nil, fmt.Errorf("view %s of %s reverted", view.Method, view.Contract.Hex())
			}
			decoded, err := view.ABI.Unpack(view.Method, result.ReturnData)
			if err != nil {
				return nil, fmt.Errorf("view %s of %s: %w", view.Method, view.Contract.Hex(), err)
			}
			values = append(values, decoded)
		}
	}
	return values, nil
}

var polymarketCTFABI = MustABI(`[
  {"type":"function","name":"balanceOfBatch","stateMutability":"view",
   "inputs":[{"name":"owners","type":"address[]"},{"name":"ids","type":"uint256[]"}],
   "outputs":[{"name":"","type":"uint256[]"}]},
  {"type":"function","name":"payoutDenominator","stateMutability":"view",
   "inputs":[{"name":"","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
  {"type":"function","name":"payoutNumerators","stateMutability":"view",
   "inputs":[{"name":"","type":"bytes32"},{"name":"","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]}
]`)

// polymarketToken is one outcome token as the index describes it.
type polymarketToken struct {
	ID           *big.Int
	ConditionID  common.Hash
	OutcomeIndex int
	IndexSet     int64
}

type polymarketPositionIndexer interface {
	// TokenIDs lists every outcome token the index has seen move in or out of account, and the
	// block the index answered at.
	TokenIDs(ctx context.Context, client *RPCClient, block BlockRef, account common.Address) (polymarketRead, error)
}

// polymarketRead is one read of the index: the tokens it named and the lookup of their
// conditions, held to the same block.
type polymarketRead struct {
	IndexerBlock uint64
	TokenIDs     []*big.Int
	// Tokens describes the named tokens, at the block the token ids were read at.
	Tokens func(ctx context.Context, ids []*big.Int) (map[string]polymarketToken, error)
}

type PolymarketAdapter struct {
	adapterBase
	indexer polymarketPositionIndexer
}

func newPolymarketAdapter(config SentioIndexerConfig) Adapter {
	return newPolymarketAdapterWithIndexer(newPolymarketIndexer(config))
}

func newPolymarketAdapterWithIndexer(indexer polymarketPositionIndexer) *PolymarketAdapter {
	return &PolymarketAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "polymarket", Name: "Polymarket", Chains: []ChainID{Polygon}}},
		indexer:     indexer,
	}
}

func (a *PolymarketAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Polygon || block.Number < polymarketIndexStart ||
		!polymarketCTFWindow.ActiveAt(block.Number) || !polymarketMulticallWindow.ActiveAt(block.Number) {
		return []Group{}, nil
	}
	read, err := a.indexer.TokenIDs(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	held, err := polymarketBalances(ctx, client, block, account, read.TokenIDs)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return []Group{}, nil
	}
	ids := make([]*big.Int, 0, len(held))
	for _, holding := range held {
		ids = append(ids, holding.id)
	}
	tokens, err := read.Tokens(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("Polymarket outcome token lookup: %w", err)
	}
	return polymarketGroups(ctx, client, block, held, tokens, read.IndexerBlock)
}

type polymarketHolding struct {
	id     *big.Int
	amount *big.Int
}

// polymarketBalances reads the account's balance of every token at the pin and keeps those above
// the dust floor, in the order the tokens were named.
func polymarketBalances(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	ids []*big.Int,
) ([]polymarketHolding, error) {
	calls := make([]ContractCall, 0, (len(ids)+polymarketBalanceBatch-1)/polymarketBalanceBatch)
	for start := 0; start < len(ids); start += polymarketBalanceBatch {
		chunk := ids[start:min(start+polymarketBalanceBatch, len(ids))]
		owners := make([]common.Address, len(chunk))
		for index := range owners {
			owners[index] = account
		}
		calls = append(calls, ContractCall{
			Contract: polymarketCTF, ABI: polymarketCTFABI, Method: "balanceOfBatch",
			Args: []any{owners, chunk},
		})
	}
	if len(calls) == 0 {
		return nil, nil
	}
	rows, err := polymarketCalls(ctx, client, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Polymarket balances: %w", err)
	}
	held := make([]polymarketHolding, 0)
	for call, row := range rows {
		chunk := ids[call*polymarketBalanceBatch : min((call+1)*polymarketBalanceBatch, len(ids))]
		balances, ok := row[0].([]*big.Int)
		if !ok || len(balances) != len(chunk) {
			return nil, fmt.Errorf("Polymarket balanceOfBatch returned %d balances for %d tokens", len(balances), len(chunk))
		}
		for index, balance := range balances {
			if balance.Cmp(big.NewInt(polymarketDustUnits)) > 0 {
				held = append(held, polymarketHolding{id: chunk[index], amount: balance})
			}
		}
	}
	return held, nil
}

// polymarketPosition is a held token the index's description of has been checked.
type polymarketPosition struct {
	holding polymarketHolding
	token   polymarketToken
	negRisk bool
}

// polymarketGroups reports one group per outcome token held. It checks the index's description of
// every token against the token's id, and reads whether the token's condition had resolved at the
// pin: a resolved share is worth its payout in collateral, and one that pays nothing is not
// reported. A token that fails a check is left out and reported with the groups that passed.
func polymarketGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	held []polymarketHolding,
	tokens map[string]polymarketToken,
	indexerBlock uint64,
) ([]Group, error) {
	var failures []error
	positions := make([]polymarketPosition, 0, len(held))
	conditionIDs := make([]common.Hash, 0)
	seenConditions := make(map[common.Hash]struct{})
	for _, holding := range held {
		token, exists := tokens[holding.id.String()]
		if !exists {
			failures = append(failures, fmt.Errorf("Polymarket index does not describe held token %s", holding.id))
			continue
		}
		negRisk, err := polymarketVerifyToken(holding.id, token)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		positions = append(positions, polymarketPosition{holding: holding, token: token, negRisk: negRisk})
		if _, seen := seenConditions[token.ConditionID]; !seen {
			seenConditions[token.ConditionID] = struct{}{}
			conditionIDs = append(conditionIDs, token.ConditionID)
		}
	}
	if len(positions) == 0 {
		return []Group{}, errors.Join(failures...)
	}
	calls := make([]ContractCall, len(conditionIDs))
	for index, conditionID := range conditionIDs {
		calls[index] = ContractCall{
			Contract: polymarketCTF, ABI: polymarketCTFABI, Method: "payoutDenominator", Args: []any{conditionID},
		}
	}
	rows, err := polymarketViews(ctx, client, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Polymarket resolutions: %w", err)
	}
	denominators := make(map[common.Hash]*big.Int, len(conditionIDs))
	for index, conditionID := range conditionIDs {
		denominator, err := BigIntAt(rows[index], 0)
		if err != nil {
			return nil, fmt.Errorf("Polymarket condition %s resolution: %w", conditionID.Hex(), err)
		}
		denominators[conditionID] = denominator
	}
	payoutCalls := make([]ContractCall, 0)
	payoutOf := make([]int, 0)
	for index, position := range positions {
		if denominators[position.token.ConditionID].Sign() > 0 {
			payoutOf = append(payoutOf, index)
			payoutCalls = append(payoutCalls, ContractCall{
				Contract: polymarketCTF, ABI: polymarketCTFABI, Method: "payoutNumerators",
				Args: []any{position.token.ConditionID, big.NewInt(int64(position.token.OutcomeIndex))},
			})
		}
	}
	numerators := make(map[int]*big.Int, len(payoutCalls))
	if len(payoutCalls) > 0 {
		payoutRows, err := polymarketViews(ctx, client, block, payoutCalls)
		if err != nil {
			return nil, fmt.Errorf("Polymarket payouts: %w", err)
		}
		for call, row := range payoutRows {
			numerator, err := BigIntAt(row, 0)
			if err != nil {
				return nil, fmt.Errorf("Polymarket payout: %w", err)
			}
			numerators[payoutOf[call]] = numerator
		}
	}
	groups := make([]Group, 0, len(positions))
	for index, position := range positions {
		denominator := denominators[position.token.ConditionID]
		numerator, resolved := numerators[index]
		if resolved && numerator.Sign() == 0 {
			// The condition resolved against this outcome: the share redeems for nothing.
			continue
		}
		if resolved && numerator.Cmp(denominator) > 0 {
			failures = append(failures, fmt.Errorf(
				"Polymarket condition %s pays %s/%s on outcome %d",
				position.token.ConditionID.Hex(), numerator, denominator, position.token.OutcomeIndex,
			))
			continue
		}
		groups = append(groups, polymarketGroup(position, numerator, denominator, indexerBlock))
	}
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].MarketID != groups[right].MarketID {
			return groups[left].MarketID < groups[right].MarketID
		}
		return groups[left].ID < groups[right].ID
	})
	return groups, errors.Join(failures...)
}

// polymarketVerifyToken checks the index's description of a held token against the token's id: the
// id must be the position of the described outcome's collection in one of the two collaterals
// Polymarket markets settle in. Polymarket outcomes are single outcomes, so the collection's index
// set is the outcome's bit. It reports whether the collateral is the neg-risk wrapper.
//
// The account holds the token, so the condition was prepared with the outcome among its slots:
// the contract mints no position of an unprepared condition or of an index set beyond its slots.
func polymarketVerifyToken(id *big.Int, token polymarketToken) (bool, error) {
	if token.OutcomeIndex < 0 || token.OutcomeIndex >= 62 || token.IndexSet != int64(1)<<token.OutcomeIndex {
		return false, fmt.Errorf(
			"Polymarket token %s names outcome %d with index set %d", id, token.OutcomeIndex, token.IndexSet,
		)
	}
	collection := polymarketCollectionID(token.ConditionID, big.NewInt(token.IndexSet))
	for _, collateral := range []common.Address{polymarketUSDCe.Address, polymarketWrappedCollateral} {
		if polymarketPositionID(collateral, collection).Cmp(id) == 0 {
			return collateral == polymarketWrappedCollateral, nil
		}
	}
	return false, fmt.Errorf(
		"Polymarket token %s is not outcome %d of condition %s", id, token.OutcomeIndex, token.ConditionID.Hex(),
	)
}

var (
	// polymarketCurveP is the alt_bn128 base field's prime, the field collection ids are points in.
	polymarketCurveP, _ = new(big.Int).SetString(
		"21888242871839275222246405745257275088696311157297823662689037894645226208583", 10,
	)
	// polymarketCurveSqrtExponent is (P+1)/4: P is 3 mod 4, so a square's root is its power by it.
	polymarketCurveSqrtExponent = new(big.Int).Rsh(new(big.Int).Add(polymarketCurveP, big.NewInt(1)), 2)
	polymarketCurveB            = big.NewInt(3)
)

// polymarketCollectionID is the Conditional Tokens collection id of an index set of a condition,
// with no parent collection, as the contract's getCollectionId computes it: the hash of the two
// mapped to a point of y² = x³ + 3, the parity of y taken from the hash's top bit and recorded in
// bit 254 of x.
func polymarketCollectionID(conditionID common.Hash, indexSet *big.Int) [32]byte {
	x := new(big.Int).SetBytes(crypto.Keccak256(conditionID[:], common.LeftPadBytes(indexSet.Bytes(), 32)))
	odd := x.Bit(255) == 1
	y := new(big.Int)
	yy := new(big.Int)
	square := new(big.Int)
	for {
		x.Add(x, big.NewInt(1)).Mod(x, polymarketCurveP)
		yy.Exp(x, big.NewInt(3), polymarketCurveP).Add(yy, polymarketCurveB).Mod(yy, polymarketCurveP)
		y.Exp(yy, polymarketCurveSqrtExponent, polymarketCurveP)
		if square.Mul(y, y).Mod(square, polymarketCurveP).Cmp(yy) == 0 {
			break
		}
	}
	if odd != (y.Bit(0) == 1) {
		y.Sub(polymarketCurveP, y)
	}
	if y.Bit(0) == 1 {
		x.SetBit(x, 254, x.Bit(254)^1)
	}
	var collection [32]byte
	x.FillBytes(collection[:])
	return collection
}

// polymarketPositionID is the Conditional Tokens position id of a collection in a collateral:
// keccak256 of the two packed.
func polymarketPositionID(collateral common.Address, collection [32]byte) *big.Int {
	return new(big.Int).SetBytes(crypto.Keccak256(collateral.Bytes(), collection[:]))
}

// polymarketGroup reports one outcome token held. An open market's share is its own asset, quoted
// by the token's id. A resolved one is valued through USDC.e at its payout, which the contract
// fixed at resolution, so a historical scan values it without a quote for the share.
func polymarketGroup(position polymarketPosition, numerator, denominator *big.Int, indexerBlock uint64) Group {
	share := Token{
		ChainID: Polygon, Address: polymarketCTF,
		Symbol: fmt.Sprintf("OUTCOME-%d", position.token.OutcomeIndex), Decimals: polymarketShareDecimals,
		TokenID: position.holding.id.String(),
	}
	component := NewComponent("asset", share, position.holding.amount, Source{
		Contract: polymarketCTF, Method: "balanceOfBatch",
		// The share is an ERC-1155 balance, which the wallet does not report.
		Holds: []common.Address{},
	})
	resolved := numerator != nil
	if resolved {
		ratio := new(big.Int).Mul(numerator, priceBasisRatioOne)
		ratio.Quo(ratio, denominator)
		component.PriceBasis = &PriceBasis{Token: polymarketUSDCe, RatioRaw: ratio.String()}
		component.Metadata = map[string]any{
			"payoutNumerator":   numerator.String(),
			"payoutDenominator": denominator.String(),
		}
	}
	conditionID := strings.ToLower(position.token.ConditionID.Hex())
	return Group{
		ID:         position.holding.id.String(),
		MarketID:   conditionID,
		Label:      fmt.Sprintf("Prediction · outcome %d", position.token.OutcomeIndex),
		Components: []Component{component},
		Metadata: map[string]any{
			"conditionId":  conditionID,
			"outcomeIndex": position.token.OutcomeIndex,
			"negRisk":      position.negRisk,
			"resolved":     resolved,
			"indexerBlock": indexerBlock,
		},
	}
}
