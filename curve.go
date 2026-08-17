package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const curveMaxMarkets = 1_000

var (
	curveCrvUSDFactory = common.HexToAddress("0xC9332fdCB1C491Dcc683bAe86Fe3cb70360738BC")
	curveCrvUSDToken   = common.HexToAddress("0xf939E0A03FB07F59A73314E73794Be0E57ac1b4E")

	// DeBank exposes the underlying reUSD for Savings reUSD collateral. It keeps other
	// ERC-4626 collateral, including sDOLA and sfrxUSD, as the wrapper token. Keep these
	// presentation rules explicit instead of inferring them from an optional interface.
	curveUnderlyingCollateral = map[common.Address]struct{}{
		common.HexToAddress("0x557AB1e003951A73c12D16F0fEA8490E39C33C35"): {},
	}
)

const (
	curveCrvUSDActivation = uint64(17_257_955)
)

type curveLendingDeployment struct {
	oneWayFactory    common.Address
	oneWayActivation uint64
	v2Factory        common.Address
	v2Activation     uint64
}

var curveLendingDeployments = map[ChainID]curveLendingDeployment{
	Ethereum: {
		oneWayFactory:    common.HexToAddress("0xeA6876DDE9e3467564acBeE1Ed5bac88783205E0"),
		oneWayActivation: 19_422_660,
		v2Factory:        common.HexToAddress("0x8f6B56EC5ddF1F2691a1059f1D3cd97Ac9EaB0bd"),
		v2Activation:     25_523_555,
	},
	Arbitrum: {
		oneWayFactory:    common.HexToAddress("0xcaEC110C784c9DF37240a8Ce096D352A75922DeA"),
		oneWayActivation: 193_652_535,
	},
}

var curveControllerFactoryABI = MustABI(`[
  {"type":"function","name":"n_collaterals","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"controllers","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]}
]`)

var curveControllerABI = MustABI(`[
  {"type":"function","name":"collateral_token","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"borrowed_token","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"loan_exists","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"bool"}]},
  {"type":"function","name":"user_state","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"}]}
]`)

var curveOneWayFactoryABI = MustABI(`[
  {"type":"function","name":"market_count","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"controllers","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"vaults","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"collateral_tokens","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"borrowed_tokens","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"gauge_for_vault","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"address"}]}
]`)

// A static tuple has the same wire encoding as these seven top-level address
// outputs. Describing it flat keeps decoding independent of generated struct
// names while preserving the contract's markets(uint256) selector.
var curveV2FactoryABI = MustABI(`[
  {"type":"function","name":"market_count","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"markets","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"},{"type":"address"},{"type":"address"},{"type":"address"},{"type":"address"},{"type":"address"},{"type":"address"}]}
]`)

var curveVaultABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var curveGaugeABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"reward_count","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"reward_tokens","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"claimable_reward","stateMutability":"view","inputs":[{"type":"address"},{"type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

func curveBoundedCount(values []any, label string) (int, error) {
	count, err := BigIntAt(values, 0)
	if err != nil {
		return 0, err
	}
	if !count.IsUint64() || count.Uint64() > curveMaxMarkets {
		return 0, fmt.Errorf("%s count %s is outside the safety bound", label, count)
	}
	return int(count.Uint64()), nil
}

func readERC20Token(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	address common.Address,
) (Token, error) {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: address, ABI: erc20ABI, Method: "symbol"},
		{Contract: address, ABI: erc20ABI, Method: "decimals"},
	})
	if err != nil {
		return Token{}, err
	}
	symbol, err := StringAt(rows[0], 0)
	if err != nil {
		return Token{}, err
	}
	decimals, err := Uint8At(rows[1], 0)
	if err != nil {
		return Token{}, err
	}
	return Token{ChainID: block.ChainID, Address: address, Symbol: symbol, Decimals: decimals}, nil
}

func readERC20Tokens(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	addresses []common.Address,
) (map[common.Address]Token, error) {
	unique := make(map[common.Address]struct{}, len(addresses))
	for _, address := range addresses {
		if address != (common.Address{}) {
			unique[address] = struct{}{}
		}
	}
	ordered := make([]common.Address, 0, len(unique))
	for address := range unique {
		ordered = append(ordered, address)
	}
	sort.Slice(ordered, func(left, right int) bool {
		return strings.ToLower(ordered[left].Hex()) < strings.ToLower(ordered[right].Hex())
	})
	result := make(map[common.Address]Token, len(ordered))
	for _, address := range ordered {
		token, err := readERC20Token(ctx, client, block, address)
		if err != nil {
			return nil, fmt.Errorf("token %s metadata: %w", address, err)
		}
		result[address] = token
	}
	return result, nil
}

type curveControllerPosition struct {
	controller      common.Address
	collateralToken common.Address
	borrowedToken   common.Address
	collateral      *big.Int
	borrowedBalance *big.Int
	debt            *big.Int
	bandCount       *big.Int
}

type curveCrvUSDAdapter struct{ adapterBase }

func newCurveCrvUSDAdapter() Adapter {
	return &curveCrvUSDAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "crvusd", Name: "Curve crvUSD", Chains: []ChainID{Ethereum},
	}}}
}

func curveCrvUSDControllers(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) ([]common.Address, error) {
	row, err := client.Call(ctx, block, curveCrvUSDFactory, curveControllerFactoryABI, "n_collaterals")
	if err != nil {
		return nil, err
	}
	count, err := curveBoundedCount(row, "crvUSD controller")
	if err != nil || count == 0 {
		return nil, err
	}
	calls := make([]ContractCall, count)
	for index := range calls {
		calls[index] = ContractCall{
			Contract: curveCrvUSDFactory,
			ABI:      curveControllerFactoryABI,
			Method:   "controllers",
			Args:     []any{big.NewInt(int64(index))},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	controllers := make([]common.Address, len(rows))
	seen := make(map[common.Address]struct{}, len(rows))
	for index, values := range rows {
		controller, decodeErr := AddressAt(values, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if controller == (common.Address{}) {
			return nil, fmt.Errorf("crvUSD controller %d is zero", index)
		}
		if _, exists := seen[controller]; exists {
			return nil, fmt.Errorf("duplicate crvUSD controller %s", controller)
		}
		seen[controller] = struct{}{}
		controllers[index] = controller
	}
	return controllers, nil
}

func curveActiveControllerPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	controllers []common.Address,
	borrowedToken common.Address,
) ([]curveControllerPosition, error) {
	flagCalls := make([]ContractCall, len(controllers))
	for index, controller := range controllers {
		flagCalls[index] = ContractCall{
			Contract: controller, ABI: curveControllerABI, Method: "loan_exists", Args: []any{account},
		}
	}
	flags, err := client.ParallelCalls(ctx, block, flagCalls)
	if err != nil {
		return nil, err
	}
	active := make([]common.Address, 0)
	for index, row := range flags {
		exists, decodeErr := BoolAt(row, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if exists {
			active = append(active, controllers[index])
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(active)*3)
	for _, controller := range active {
		calls = append(calls, ContractCall{Contract: controller, ABI: curveControllerABI, Method: "collateral_token"})
		if borrowedToken == (common.Address{}) {
			calls = append(calls, ContractCall{Contract: controller, ABI: curveControllerABI, Method: "borrowed_token"})
		}
		calls = append(calls, ContractCall{
			Contract: controller, ABI: curveControllerABI, Method: "user_state", Args: []any{account},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	stride := 2
	if borrowedToken == (common.Address{}) {
		stride = 3
	}
	result := make([]curveControllerPosition, len(active))
	for index, controller := range active {
		collateral, decodeErr := AddressAt(rows[index*stride], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		borrowed := borrowedToken
		stateIndex := index*stride + 1
		if borrowed == (common.Address{}) {
			borrowed, decodeErr = AddressAt(rows[index*stride+1], 0)
			if decodeErr != nil {
				return nil, decodeErr
			}
			stateIndex++
		}
		state := rows[stateIndex]
		values := make([]*big.Int, 4)
		for valueIndex := range values {
			values[valueIndex], decodeErr = BigIntAt(state, valueIndex)
			if decodeErr != nil {
				return nil, decodeErr
			}
		}
		result[index] = curveControllerPosition{
			controller: controller, collateralToken: collateral, borrowedToken: borrowed,
			collateral: values[0], borrowedBalance: values[1], debt: values[2], bandCount: values[3],
		}
	}
	return result, nil
}

func curveControllerGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	positions []curveControllerPosition,
	tokens map[common.Address]Token,
	labelPrefix string,
) ([]Group, error) {
	groups := make([]Group, 0, len(positions))
	for _, position := range positions {
		collateral, collateralOK := tokens[position.collateralToken]
		borrowed, borrowedOK := tokens[position.borrowedToken]
		if !collateralOK || !borrowedOK {
			return nil, fmt.Errorf("controller %s token metadata is incomplete", position.controller)
		}
		components := make([]Component, 0, 3)
		labelCollateral := collateral
		if position.collateral.Sign() > 0 {
			displayedToken, displayedAmount, conversionMethod, err := curveNormalizeCollateral(
				ctx, client, block, collateral, position.collateral,
			)
			if err != nil {
				return nil, fmt.Errorf("controller %s collateral: %w", position.controller, err)
			}
			labelCollateral = displayedToken
			components = append(components, NewComponent("asset", displayedToken, displayedAmount, Source{
				Contract: position.controller, Method: "user_state.collateral" + conversionMethod,
			}))
		}
		if position.borrowedBalance.Sign() > 0 {
			components = append(components, NewComponent("asset", borrowed, position.borrowedBalance, Source{
				Contract: position.controller, Method: "user_state.borrowedBalance",
			}))
		}
		if position.debt.Sign() > 0 {
			components = append(components, NewComponent("debt", borrowed, position.debt, Source{
				Contract: position.controller, Method: "user_state.debt",
			}))
		}
		if len(components) == 0 {
			continue
		}
		id := strings.ToLower(position.controller.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: id,
			Label:          fmt.Sprintf("%s %s/%s", labelPrefix, labelCollateral.Symbol, borrowed.Symbol),
			Components:     components,
			NetValuePolicy: "floor-zero",
			Metadata:       map[string]any{"bandCount": position.bandCount.String()},
		})
	}
	return groups, nil
}

func (a *curveCrvUSDAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum || block.Number < curveCrvUSDActivation {
		return nil, nil
	}
	controllers, err := curveCrvUSDControllers(ctx, client, block)
	if err != nil {
		return nil, fmt.Errorf("enumerate controllers: %w", err)
	}
	positions, err := curveActiveControllerPositions(
		ctx, client, block, account, controllers, curveCrvUSDToken,
	)
	if err != nil {
		return nil, fmt.Errorf("controller positions: %w", err)
	}
	addresses := []common.Address{curveCrvUSDToken}
	for _, position := range positions {
		addresses = append(addresses, position.collateralToken)
	}
	tokens, err := readERC20Tokens(ctx, client, block, addresses)
	if err != nil {
		return nil, err
	}
	return curveControllerGroups(ctx, client, block, positions, tokens, "Curve crvUSD")
}

// curveNormalizeCollateral applies the protocol presentation rules above. Once a token is
// configured for normalization, every required ERC-4626 read is strict: silently keeping the
// wrapper on an RPC or ABI error would produce a plausible but wrong position.
func curveNormalizeCollateral(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	collateral Token,
	amount *big.Int,
) (Token, *big.Int, string, error) {
	if _, normalize := curveUnderlyingCollateral[collateral.Address]; !normalize {
		return collateral, new(big.Int).Set(amount), "", nil
	}
	row, err := client.Call(ctx, block, collateral.Address, curveVaultABI, "asset")
	if err != nil {
		return Token{}, nil, "", err
	}
	underlying, err := AddressAt(row, 0)
	if err != nil {
		return Token{}, nil, "", err
	}
	if underlying == (common.Address{}) {
		return Token{}, nil, "", fmt.Errorf("ERC-4626 asset is zero")
	}
	convertedRow, err := client.Call(
		ctx, block, collateral.Address, curveVaultABI, "convertToAssets", amount,
	)
	if err != nil {
		return Token{}, nil, "", fmt.Errorf("ERC-4626 convertToAssets: %w", err)
	}
	converted, err := BigIntAt(convertedRow, 0)
	if err != nil {
		return Token{}, nil, "", err
	}
	underlyingToken, err := readERC20Token(ctx, client, block, underlying)
	if err != nil {
		return Token{}, nil, "", err
	}
	return underlyingToken, converted, "*collateral.convertToAssets", nil
}

type curveLendingMarket struct {
	generation      string
	vault           common.Address
	controller      common.Address
	collateralToken common.Address
	borrowedToken   common.Address
	gauge           common.Address
}

type curveLendingAdapter struct {
	adapterBase
	deployments map[ChainID]curveLendingDeployment
}

func newCurveLendingAdapter() Adapter {
	chains := make([]ChainID, 0, len(curveLendingDeployments))
	for _, chainID := range SupportedChainIDs {
		if _, exists := curveLendingDeployments[chainID]; exists {
			chains = append(chains, chainID)
		}
	}
	return &curveLendingAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "curve-lending", Name: "Curve Lending", Chains: chains,
		}},
		deployments: curveLendingDeployments,
	}
}

func enumerateCurveOneWayMarkets(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment curveLendingDeployment,
) ([]curveLendingMarket, error) {
	if deployment.oneWayFactory == (common.Address{}) || block.Number < deployment.oneWayActivation {
		return nil, nil
	}
	row, err := client.Call(ctx, block, deployment.oneWayFactory, curveOneWayFactoryABI, "market_count")
	if err != nil {
		return nil, err
	}
	count, err := curveBoundedCount(row, "Curve one-way market")
	if err != nil || count == 0 {
		return nil, err
	}
	calls := make([]ContractCall, 0, count*2)
	for index := 0; index < count; index++ {
		argument := []any{big.NewInt(int64(index))}
		for _, method := range []string{"controllers", "vaults"} {
			calls = append(calls, ContractCall{
				Contract: deployment.oneWayFactory, ABI: curveOneWayFactoryABI, Method: method, Args: argument,
			})
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	markets := make([]curveLendingMarket, count)
	tokenCalls := make([]ContractCall, 0, count*2)
	gaugeCalls := make([]ContractCall, count)
	for index := 0; index < count; index++ {
		addresses := make([]common.Address, 2)
		for offset := range addresses {
			addresses[offset], err = AddressAt(rows[index*2+offset], 0)
			if err != nil {
				return nil, err
			}
		}
		markets[index] = curveLendingMarket{
			generation: "one-way", controller: addresses[0], vault: addresses[1],
		}
		tokenCalls = append(tokenCalls,
			ContractCall{Contract: addresses[0], ABI: curveControllerABI, Method: "collateral_token"},
			ContractCall{Contract: addresses[0], ABI: curveControllerABI, Method: "borrowed_token"},
		)
		gaugeCalls[index] = ContractCall{
			Contract: deployment.oneWayFactory, ABI: curveOneWayFactoryABI,
			Method: "gauge_for_vault", Args: []any{addresses[1]},
		}
	}
	tokenRows, err := client.ParallelCalls(ctx, block, tokenCalls)
	if err != nil {
		return nil, err
	}
	for index := range markets {
		markets[index].collateralToken, err = AddressAt(tokenRows[index*2], 0)
		if err != nil {
			return nil, err
		}
		markets[index].borrowedToken, err = AddressAt(tokenRows[index*2+1], 0)
		if err != nil {
			return nil, err
		}
	}
	gauges, err := client.ParallelCallsAllowFailure(ctx, block, gaugeCalls)
	if err != nil {
		return nil, err
	}
	for index, row := range gauges {
		if row.Error != nil {
			// The Vyper factory's gauge_for_vault HashMap getter reverts when a
			// market has no gauge. Only that contract-level absence is optional;
			// transport and decode errors remain explicit failures.
			if strings.Contains(strings.ToLower(row.Error.Error()), "execution reverted") {
				continue
			}
			return nil, fmt.Errorf("market %s gauge: %w", markets[index].vault, row.Error)
		}
		markets[index].gauge, err = AddressAt(row.Values, 0)
		if err != nil {
			return nil, err
		}
	}
	return markets, nil
}

func enumerateCurveV2Markets(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment curveLendingDeployment,
) ([]curveLendingMarket, error) {
	if deployment.v2Factory == (common.Address{}) || block.Number < deployment.v2Activation {
		return nil, nil
	}
	row, err := client.Call(ctx, block, deployment.v2Factory, curveV2FactoryABI, "market_count")
	if err != nil {
		return nil, err
	}
	count, err := curveBoundedCount(row, "Curve v2 market")
	if err != nil || count == 0 {
		return nil, err
	}
	calls := make([]ContractCall, count)
	for index := range calls {
		calls[index] = ContractCall{
			Contract: deployment.v2Factory, ABI: curveV2FactoryABI,
			Method: "markets", Args: []any{big.NewInt(int64(index))},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	markets := make([]curveLendingMarket, count)
	for index, row := range rows {
		addresses := make([]common.Address, 5)
		for offset := range addresses {
			addresses[offset], err = AddressAt(row, offset)
			if err != nil {
				return nil, fmt.Errorf("market %d output %d: %w", index, offset, err)
			}
		}
		markets[index] = curveLendingMarket{
			generation: "v2", vault: addresses[0], controller: addresses[1],
			collateralToken: addresses[3], borrowedToken: addresses[4],
		}
	}
	return markets, nil
}

func enumerateCurveLendingMarkets(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	deployment curveLendingDeployment,
) ([]curveLendingMarket, error) {
	oneWay, err := enumerateCurveOneWayMarkets(ctx, client, block, deployment)
	if err != nil {
		return nil, fmt.Errorf("one-way factory: %w", err)
	}
	v2, err := enumerateCurveV2Markets(ctx, client, block, deployment)
	if err != nil {
		return nil, fmt.Errorf("v2 factory: %w", err)
	}
	markets := append(oneWay, v2...)
	seen := make(map[common.Address]struct{}, len(markets))
	for _, market := range markets {
		if market.vault == (common.Address{}) || market.controller == (common.Address{}) {
			return nil, errors.New("Curve lending factory returned a zero market address")
		}
		if _, exists := seen[market.vault]; exists {
			return nil, fmt.Errorf("Curve lending factories returned duplicate vault %s", market.vault)
		}
		seen[market.vault] = struct{}{}
	}
	return markets, nil
}

type activeCurveLendingMarket struct {
	market       curveLendingMarket
	directShares *big.Int
	gaugeShares  *big.Int
	loan         bool
}

func activeCurveLendingMarkets(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	markets []curveLendingMarket,
) ([]activeCurveLendingMarket, error) {
	calls := make([]ContractCall, 0, len(markets)*3)
	for _, market := range markets {
		calls = append(calls,
			ContractCall{Contract: market.vault, ABI: curveVaultABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: market.controller, ABI: curveControllerABI, Method: "loan_exists", Args: []any{account}},
		)
		if market.gauge != (common.Address{}) {
			calls = append(calls, ContractCall{
				Contract: market.gauge, ABI: curveGaugeABI, Method: "balanceOf", Args: []any{account},
			})
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	result := make([]activeCurveLendingMarket, 0)
	offset := 0
	for _, market := range markets {
		direct, decodeErr := BigIntAt(rows[offset], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		offset++
		loan, decodeErr := BoolAt(rows[offset], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		offset++
		gauge := new(big.Int)
		if market.gauge != (common.Address{}) {
			gauge, decodeErr = BigIntAt(rows[offset], 0)
			if decodeErr != nil {
				return nil, decodeErr
			}
			offset++
		}
		if direct.Sign() > 0 || gauge.Sign() > 0 || loan {
			result = append(result, activeCurveLendingMarket{
				market: market, directShares: direct, gaugeShares: gauge, loan: loan,
			})
		}
	}
	return result, nil
}

func curveLendingPrincipalGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	active []activeCurveLendingMarket,
	tokens map[common.Address]Token,
) ([]Group, error) {
	groups := make([]Group, 0, len(active))
	for _, item := range active {
		borrowed, borrowedOK := tokens[item.market.borrowedToken]
		collateral, collateralOK := tokens[item.market.collateralToken]
		if !borrowedOK || !collateralOK {
			return nil, fmt.Errorf("market %s token metadata is incomplete", item.market.vault)
		}
		components := make([]Component, 0, 4)
		shares := new(big.Int).Add(item.directShares, item.gaugeShares)
		if shares.Sign() > 0 {
			row, err := client.Call(
				ctx, block, item.market.vault, curveVaultABI, "convertToAssets", shares,
			)
			if err != nil {
				return nil, err
			}
			assets, err := BigIntAt(row, 0)
			if err != nil {
				return nil, err
			}
			if assets.Sign() > 0 {
				component := NewComponent("asset", borrowed, assets, Source{
					Contract: item.market.vault, Method: "convertToAssets(vault balance + gauge balance)",
				})
				component.Metadata = map[string]any{
					"directShares": item.directShares.String(), "gaugeShares": item.gaugeShares.String(),
				}
				components = append(components, component)
			}
		}
		bandCount := new(big.Int)
		labelCollateral := collateral
		if item.loan {
			row, err := client.Call(
				ctx, block, item.market.controller, curveControllerABI, "user_state", account,
			)
			if err != nil {
				return nil, err
			}
			values := make([]*big.Int, 4)
			for index := range values {
				values[index], err = BigIntAt(row, index)
				if err != nil {
					return nil, err
				}
			}
			bandCount = values[3]
			if values[0].Sign() > 0 {
				displayedToken, displayedAmount, conversionMethod, conversionErr := curveNormalizeCollateral(
					ctx, client, block, collateral, values[0],
				)
				if conversionErr != nil {
					return nil, fmt.Errorf("market %s collateral: %w", item.market.vault, conversionErr)
				}
				labelCollateral = displayedToken
				components = append(components, NewComponent("asset", displayedToken, displayedAmount, Source{
					Contract: item.market.controller, Method: "user_state.collateral" + conversionMethod,
				}))
			}
			if values[1].Sign() > 0 {
				components = append(components, NewComponent("asset", borrowed, values[1], Source{
					Contract: item.market.controller, Method: "user_state.borrowedBalance",
				}))
			}
			if values[2].Sign() > 0 {
				components = append(components, NewComponent("debt", borrowed, values[2], Source{
					Contract: item.market.controller, Method: "user_state.debt",
				}))
			}
		}
		if len(components) == 0 {
			continue
		}
		id := strings.ToLower(item.market.vault.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: id,
			Label:          fmt.Sprintf("Curve Lending %s/%s", labelCollateral.Symbol, borrowed.Symbol),
			Components:     components,
			NetValuePolicy: "floor-zero",
			Metadata: map[string]any{
				"generation": item.market.generation, "bandCount": bandCount.String(),
			},
		})
	}
	return groups, nil
}

type curveGaugeReward struct {
	market curveLendingMarket
	token  common.Address
	amount *big.Int
}

func curveLendingRewardGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	markets []curveLendingMarket,
) ([]Group, error) {
	gauges := make([]curveLendingMarket, 0)
	for _, market := range markets {
		if market.gauge != (common.Address{}) {
			gauges = append(gauges, market)
		}
	}
	countCalls := make([]ContractCall, len(gauges))
	for index, market := range gauges {
		countCalls[index] = ContractCall{Contract: market.gauge, ABI: curveGaugeABI, Method: "reward_count"}
	}
	countRows, err := client.ParallelCalls(ctx, block, countCalls)
	if err != nil {
		return nil, err
	}
	tokenCalls := make([]ContractCall, 0)
	tokenMarkets := make([]curveLendingMarket, 0)
	for index, market := range gauges {
		count, decodeErr := curveBoundedCount(countRows[index], "Curve gauge reward")
		if decodeErr != nil {
			return nil, decodeErr
		}
		if count > 32 {
			return nil, fmt.Errorf("gauge %s reward count %d exceeds 32", market.gauge, count)
		}
		for rewardIndex := 0; rewardIndex < count; rewardIndex++ {
			tokenCalls = append(tokenCalls, ContractCall{
				Contract: market.gauge, ABI: curveGaugeABI,
				Method: "reward_tokens", Args: []any{big.NewInt(int64(rewardIndex))},
			})
			tokenMarkets = append(tokenMarkets, market)
		}
	}
	tokenRows, err := client.ParallelCalls(ctx, block, tokenCalls)
	if err != nil {
		return nil, err
	}
	rewards := make([]curveGaugeReward, len(tokenRows))
	claimCalls := make([]ContractCall, len(tokenRows))
	for index, row := range tokenRows {
		token, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		rewards[index] = curveGaugeReward{market: tokenMarkets[index], token: token}
		claimCalls[index] = ContractCall{
			Contract: tokenMarkets[index].gauge, ABI: curveGaugeABI,
			Method: "claimable_reward", Args: []any{account, token},
		}
	}
	claimRows, err := client.ParallelCalls(ctx, block, claimCalls)
	if err != nil {
		return nil, err
	}
	tokenAddresses := make([]common.Address, 0)
	for index, row := range claimRows {
		rewards[index].amount, err = BigIntAt(row, 0)
		if err != nil {
			return nil, err
		}
		if rewards[index].amount.Sign() > 0 {
			tokenAddresses = append(tokenAddresses, rewards[index].token)
		}
	}
	metadata, err := readERC20Tokens(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0)
	for _, reward := range rewards {
		if reward.amount.Sign() == 0 {
			continue
		}
		token, exists := metadata[reward.token]
		if !exists {
			return nil, fmt.Errorf("reward token %s metadata is missing", reward.token)
		}
		id := strings.ToLower(reward.market.vault.Hex()) + ":rewards:" + strings.ToLower(reward.token.Hex())
		groups = append(groups, Group{
			ID: id, MarketID: strings.ToLower(reward.market.vault.Hex()),
			Label: "Curve Lending " + token.Symbol + " reward",
			Components: []Component{NewComponent("reward", token, reward.amount, Source{
				Contract: reward.market.gauge, Method: "claimable_reward",
			})},
		})
	}
	return groups, nil
}

func (a *curveLendingAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := a.deployments[block.ChainID]
	if !exists || block.Number < deployment.oneWayActivation {
		return nil, nil
	}
	markets, err := enumerateCurveLendingMarkets(ctx, client, block, deployment)
	if err != nil {
		return nil, fmt.Errorf("enumerate markets: %w", err)
	}
	active, err := activeCurveLendingMarkets(ctx, client, block, account, markets)
	if err != nil {
		return nil, fmt.Errorf("active markets: %w", err)
	}
	addresses := make([]common.Address, 0, len(active)*2)
	for _, item := range active {
		addresses = append(addresses, item.market.collateralToken, item.market.borrowedToken)
	}
	tokens, err := readERC20Tokens(ctx, client, block, addresses)
	if err != nil {
		return nil, err
	}
	principal, err := curveLendingPrincipalGroups(ctx, client, block, account, active, tokens)
	if err != nil {
		return nil, fmt.Errorf("principal: %w", err)
	}
	rewards, err := curveLendingRewardGroups(ctx, client, block, account, markets)
	if err != nil {
		return nil, fmt.Errorf("rewards: %w", err)
	}
	return append(principal, rewards...), nil
}
