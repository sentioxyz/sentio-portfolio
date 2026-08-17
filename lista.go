package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var listaMoolahABI = MustABI(`[
  {"type":"function","name":"feeRecipient","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"position","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"},{"name":"user","type":"address"}],"outputs":[{"name":"supplyShares","type":"uint256"},{"name":"borrowShares","type":"uint128"},{"name":"collateral","type":"uint128"}]},
  {"type":"function","name":"market","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"totalSupplyAssets","type":"uint128"},{"name":"totalSupplyShares","type":"uint128"},{"name":"totalBorrowAssets","type":"uint128"},{"name":"totalBorrowShares","type":"uint128"},{"name":"lastUpdate","type":"uint128"},{"name":"fee","type":"uint128"}]},
  {"type":"function","name":"idToMarketParams","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"loanToken","type":"address"},{"name":"collateralToken","type":"address"},{"name":"oracle","type":"address"},{"name":"irm","type":"address"},{"name":"lltv","type":"uint256"}]}
]`)

var listaInteractionABI = MustABI(`[
  {"type":"function","name":"collaterals","stateMutability":"view","inputs":[{"name":"token","type":"address"}],"outputs":[{"name":"gem","type":"address"},{"name":"ilk","type":"bytes32"},{"name":"live","type":"uint256"},{"name":"clip","type":"address"}]},
  {"type":"function","name":"locked","stateMutability":"view","inputs":[{"name":"token","type":"address"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"borrowed","stateMutability":"view","inputs":[{"name":"token","type":"address"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type listaVaultDeployment struct {
	Address         common.Address `json:"address"`
	ActivationBlock uint64         `json:"activationBlock"`
}

type listaMoolahDeployment struct {
	ChainID         ChainID                `json:"chainId"`
	Address         common.Address         `json:"address"`
	ActivationBlock uint64                 `json:"activationBlock"`
	Markets         []common.Hash          `json:"markets"`
	Vaults          []listaVaultDeployment `json:"vaults"`
}

type listaTokenConfig struct {
	Address  common.Address `json:"address"`
	Symbol   string         `json:"symbol"`
	Decimals uint8          `json:"decimals"`
}

type listaCDPDeployment struct {
	ChainID         ChainID          `json:"chainId"`
	Interaction     common.Address   `json:"interaction"`
	ActivationBlock uint64           `json:"activationBlock"`
	DebtToken       listaTokenConfig `json:"debtToken"`
	CandidateTokens []common.Address `json:"candidateTokens"`
}

type listaManifest struct {
	Version     int                     `json:"version"`
	GeneratedAt string                  `json:"generatedAt"`
	Sources     []string                `json:"sources"`
	Scope       string                  `json:"scope"`
	Moolah      []listaMoolahDeployment `json:"moolah"`
	CDP         listaCDPDeployment      `json:"cdp"`
}

//go:embed lista-markets.json
var listaManifestJSON []byte

var listaDeployments = mustListaManifest()

func mustListaManifest() listaManifest {
	var manifest listaManifest
	if err := json.Unmarshal(listaManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode Lista manifest: %w", err))
	}
	if manifest.Version != 1 || manifest.GeneratedAt == "" || len(manifest.Sources) == 0 || manifest.Scope == "" || len(manifest.Moolah) == 0 ||
		manifest.CDP.Interaction == (common.Address{}) || len(manifest.CDP.CandidateTokens) == 0 {
		panic("invalid Lista manifest")
	}
	seenChains := make(map[ChainID]struct{}, len(manifest.Moolah))
	seenVaults := make(map[ChainID]map[common.Address]struct{}, len(manifest.Moolah))
	for _, deployment := range manifest.Moolah {
		if deployment.ChainID != Ethereum && deployment.ChainID != BSC {
			panic(fmt.Sprintf("unsupported Lista Moolah chain %d", deployment.ChainID))
		}
		if _, exists := seenChains[deployment.ChainID]; exists {
			panic(fmt.Sprintf("duplicate Lista Moolah deployment on chain %d", deployment.ChainID))
		}
		seenChains[deployment.ChainID] = struct{}{}
		if deployment.Address == (common.Address{}) || deployment.ActivationBlock == 0 || len(deployment.Markets) == 0 {
			panic(fmt.Sprintf("invalid Lista Moolah deployment on chain %d", deployment.ChainID))
		}
		seenMarkets := make(map[common.Hash]struct{}, len(deployment.Markets))
		for _, market := range deployment.Markets {
			if market == (common.Hash{}) {
				panic("Lista manifest contains zero market id")
			}
			if _, exists := seenMarkets[market]; exists {
				panic(fmt.Sprintf("duplicate Lista market %s", market))
			}
			seenMarkets[market] = struct{}{}
		}
		seenVaults[deployment.ChainID] = make(map[common.Address]struct{}, len(deployment.Vaults))
		for _, vault := range deployment.Vaults {
			if vault.Address == (common.Address{}) || vault.ActivationBlock == 0 {
				panic(fmt.Sprintf("invalid Lista vault on chain %d", deployment.ChainID))
			}
			if _, exists := seenVaults[deployment.ChainID][vault.Address]; exists {
				panic(fmt.Sprintf("duplicate Lista vault %s on chain %d", vault.Address, deployment.ChainID))
			}
			seenVaults[deployment.ChainID][vault.Address] = struct{}{}
		}
	}
	if manifest.CDP.ChainID != BSC || manifest.CDP.ActivationBlock == 0 ||
		manifest.CDP.DebtToken.Address == (common.Address{}) || manifest.CDP.DebtToken.Symbol == "" {
		panic("invalid Lista CDP deployment")
	}
	seenCandidates := make(map[common.Address]struct{}, len(manifest.CDP.CandidateTokens))
	for _, candidate := range manifest.CDP.CandidateTokens {
		if candidate == (common.Address{}) {
			panic("Lista CDP manifest contains a zero collateral candidate")
		}
		if _, exists := seenCandidates[candidate]; exists {
			panic(fmt.Sprintf("duplicate Lista CDP collateral candidate %s", candidate))
		}
		seenCandidates[candidate] = struct{}{}
	}
	return manifest
}

type ListaAdapter struct {
	adapterBase
	moolah map[ChainID]listaMoolahDeployment
	cdp    listaCDPDeployment
}

func newListaAdapter() Adapter {
	moolah := make(map[ChainID]listaMoolahDeployment, len(listaDeployments.Moolah))
	for _, deployment := range listaDeployments.Moolah {
		moolah[deployment.ChainID] = deployment
	}
	return &ListaAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "lista", Name: "Lista DAO", Chains: []ChainID{Ethereum, BSC},
		}},
		moolah: moolah,
		cdp:    listaDeployments.CDP,
	}
}

type listaMoolahHolding struct {
	id           common.Hash
	supplyShares *big.Int
	borrowShares *big.Int
	collateral   *big.Int
}

func (h listaMoolahHolding) hasStoredPosition() bool {
	return h.supplyShares.Sign() > 0 || h.borrowShares.Sign() > 0 || h.collateral.Sign() > 0
}

func listaShouldLoadMoolahMarket(
	holding listaMoolahHolding,
	account common.Address,
	feeRecipient common.Address,
) bool {
	if holding.hasStoredPosition() {
		return true
	}
	return feeRecipient != (common.Address{}) && account == feeRecipient
}

type listaMoolahHeldMarket struct {
	holding listaMoolahHolding
	params  listaMoolahMarketParams
	state   listaMoolahMarketState
	rate    *big.Int
}

func listaShouldProcessMoolahMarket(held listaMoolahHeldMarket) (bool, error) {
	if held.state.LastUpdate.Sign() > 0 {
		return true, nil
	}
	if held.holding.hasStoredPosition() {
		return false, fmt.Errorf("Lista Moolah manifest contains uncreated market %s", held.holding.id)
	}
	return false, nil
}

type listaMoolahMarketParams = morphoMarketParams

type listaMoolahMarketState = morphoMarketState

func listaDecodeHeldMarket(
	holding listaMoolahHolding,
	marketRow []any,
	paramsRow []any,
) (listaMoolahHeldMarket, error) {
	values := make([]*big.Int, 6)
	labels := []string{
		"total supply assets", "total supply shares", "total borrow assets", "total borrow shares", "last update", "fee",
	}
	for index := range values {
		value, err := BigIntAt(marketRow, index)
		if err != nil {
			return listaMoolahHeldMarket{}, fmt.Errorf("Lista Moolah market %s %s: %w", holding.id, labels[index], err)
		}
		values[index] = value
	}
	addresses := make([]common.Address, 4)
	addressLabels := []string{"loan token", "collateral token", "oracle", "IRM"}
	for index := range addresses {
		address, err := AddressAt(paramsRow, index)
		if err != nil {
			return listaMoolahHeldMarket{}, fmt.Errorf("Lista Moolah market %s %s: %w", holding.id, addressLabels[index], err)
		}
		addresses[index] = address
	}
	lltv, err := BigIntAt(paramsRow, 4)
	if err != nil {
		return listaMoolahHeldMarket{}, fmt.Errorf("Lista Moolah market %s LLTV: %w", holding.id, err)
	}
	return listaMoolahHeldMarket{
		holding: holding,
		params: listaMoolahMarketParams{
			LoanToken: addresses[0], CollateralToken: addresses[1], Oracle: addresses[2], Irm: addresses[3], Lltv: lltv,
		},
		state: listaMoolahMarketState{
			TotalSupplyAssets: values[0], TotalSupplyShares: values[1], TotalBorrowAssets: values[2],
			TotalBorrowShares: values[3], LastUpdate: values[4], Fee: values[5],
		},
		rate: new(big.Int),
	}, nil
}

func tokenMapForAddresses(
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
	sort.Slice(ordered, func(i, j int) bool { return strings.ToLower(ordered[i].Hex()) < strings.ToLower(ordered[j].Hex()) })
	result := make(map[common.Address]Token, len(ordered))
	for _, address := range ordered {
		metadata, err := readToken(ctx, client, block, address)
		if err != nil {
			return nil, fmt.Errorf("Lista token %s metadata: %w", address, err)
		}
		result[address] = metadata
	}
	return result, nil
}

func (a *ListaAdapter) moolahGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := a.moolah[block.ChainID]
	if !exists || block.Number < deployment.ActivationBlock {
		return nil, nil
	}
	positionCalls := make([]ContractCall, len(deployment.Markets)+1)
	positionCalls[0] = ContractCall{Contract: deployment.Address, ABI: listaMoolahABI, Method: "feeRecipient"}
	for index, id := range deployment.Markets {
		positionCalls[index+1] = ContractCall{
			Contract: deployment.Address, ABI: listaMoolahABI, Method: "position", Args: []any{id, account},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, positionCalls)
	if err != nil {
		return nil, fmt.Errorf("Lista Moolah fee recipient and positions: %w", err)
	}
	feeRecipient, err := AddressAt(rows[0], 0)
	if err != nil {
		return nil, fmt.Errorf("Lista Moolah fee recipient: %w", err)
	}
	rows = rows[1:]
	holdings := make([]listaMoolahHolding, 0)
	stateCalls := make([]ContractCall, 0)
	for index, row := range rows {
		supply, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah supply shares: %w", decodeErr)
		}
		borrow, decodeErr := BigIntAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah borrow shares: %w", decodeErr)
		}
		collateral, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah collateral: %w", decodeErr)
		}
		holding := listaMoolahHolding{deployment.Markets[index], supply, borrow, collateral}
		if !listaShouldLoadMoolahMarket(holding, account, feeRecipient) {
			continue
		}
		id := holding.id
		holdings = append(holdings, holding)
		stateCalls = append(stateCalls,
			ContractCall{Contract: deployment.Address, ABI: listaMoolahABI, Method: "market", Args: []any{id}},
			ContractCall{Contract: deployment.Address, ABI: listaMoolahABI, Method: "idToMarketParams", Args: []any{id}},
		)
	}
	if len(holdings) == 0 {
		return nil, nil
	}
	state, err := client.ParallelCalls(ctx, block, stateCalls)
	if err != nil {
		return nil, fmt.Errorf("Lista Moolah held-market state: %w", err)
	}
	heldMarkets := make([]listaMoolahHeldMarket, 0, len(holdings))
	tokenAddresses := make([]common.Address, 0, len(holdings)*2)
	rateCalls := make([]ContractCall, 0, len(holdings))
	rateIndexes := make([]int, 0, len(holdings))
	for index := range holdings {
		held, decodeErr := listaDecodeHeldMarket(holdings[index], state[index*2], state[index*2+1])
		if decodeErr != nil {
			return nil, decodeErr
		}
		process, validationErr := listaShouldProcessMoolahMarket(held)
		if validationErr != nil {
			return nil, validationErr
		}
		if !process {
			continue
		}
		heldMarkets = append(heldMarkets, held)
		heldIndex := len(heldMarkets) - 1
		tokenAddresses = append(tokenAddresses, held.params.LoanToken, held.params.CollateralToken)
		if held.params.Irm != (common.Address{}) && held.state.TotalBorrowAssets.Sign() > 0 &&
			held.state.LastUpdate.Uint64() < block.Timestamp {
			rateCalls = append(rateCalls, ContractCall{
				Contract: held.params.Irm, ABI: morphoIRMABI, Method: "borrowRateView",
				Args: []any{held.params, held.state},
			})
			rateIndexes = append(rateIndexes, heldIndex)
		}
	}
	if len(rateCalls) > 0 {
		rates, rateErr := client.ParallelCalls(ctx, block, rateCalls)
		if rateErr != nil {
			return nil, fmt.Errorf("Lista Moolah borrow rates: %w", rateErr)
		}
		for index, row := range rates {
			rate, decodeErr := BigIntAt(row, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("Lista Moolah market %s borrow rate: %w", heldMarkets[rateIndexes[index]].holding.id, decodeErr)
			}
			heldMarkets[rateIndexes[index]].rate = rate
		}
	}
	tokens, err := tokenMapForAddresses(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0, len(heldMarkets))
	for _, held := range heldMarkets {
		holding := held.holding
		elapsed := uint64(0)
		if held.state.LastUpdate.Uint64() < block.Timestamp {
			elapsed = block.Timestamp - held.state.LastUpdate.Uint64()
		}
		expected, pendingFeeShares := morphoExpectedMarketBalances(held.state, held.rate, elapsed)
		effectiveSupplyShares := morphoEffectiveSupplyShares(
			holding.supplyShares, pendingFeeShares, account, feeRecipient,
		)
		components := make([]Component, 0, 3)
		if holding.collateral.Sign() > 0 {
			components = append(components, NewComponent("asset", tokens[held.params.CollateralToken], holding.collateral,
				Source{Contract: deployment.Address, Method: "position(id,account).collateral"}))
		}
		if effectiveSupplyShares.Sign() > 0 {
			numerator, denominator := morphoShareFraction(effectiveSupplyShares, expected.TotalSupplyAssets, expected.TotalSupplyShares)
			amount := new(big.Int).Div(numerator, denominator)
			component := NewComponent("asset", tokens[held.params.LoanToken], amount,
				Source{Contract: deployment.Address, Method: "expected supply assets after interest, rounded down"})
			component.Metadata = map[string]any{
				"shares": effectiveSupplyShares.String(), "storedShares": holding.supplyShares.String(),
				"borrowRatePerSecondWad": held.rate.String(),
			}
			if feeRecipient != (common.Address{}) && account == feeRecipient {
				component.Metadata["pendingFeeShares"] = pendingFeeShares.String()
			}
			components = append(components, component)
		}
		if holding.borrowShares.Sign() > 0 {
			borrowAssets := new(big.Int).Add(expected.TotalBorrowAssets, big.NewInt(1))
			borrowShares := new(big.Int).Add(expected.TotalBorrowShares, big.NewInt(1_000_000))
			amount := morphoMulDivUp(holding.borrowShares, borrowAssets, borrowShares)
			component := NewComponent("debt", tokens[held.params.LoanToken], amount,
				Source{Contract: deployment.Address, Method: "expected borrow assets after interest, rounded up"})
			component.Metadata = map[string]any{"shares": holding.borrowShares.String(), "borrowRatePerSecondWad": held.rate.String()}
			components = append(components, component)
		}
		if len(components) == 0 {
			continue
		}
		id := strings.ToLower(holding.id.Hex())
		groups = append(groups, Group{
			ID: "moolah:" + id, MarketID: id, Label: "Lending", Components: components,
			NetValuePolicy: "floor-zero", Metadata: map[string]any{"marketId": holding.id},
		})
	}
	return groups, nil
}

func (a *ListaAdapter) vaultGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := a.moolah[block.ChainID]
	if !exists {
		return nil, nil
	}
	vaults := make([]listaVaultDeployment, 0, len(deployment.Vaults))
	for _, vault := range deployment.Vaults {
		if vault.ActivationBlock <= block.Number {
			vaults = append(vaults, vault)
		}
	}
	calls := make([]ContractCall, len(vaults))
	for index, vault := range vaults {
		calls[index] = ContractCall{Contract: vault.Address, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Lista Moolah vault balances: %w", err)
	}
	groups := make([]Group, 0)
	for index, row := range rows {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s balance: %w", vaults[index].Address, decodeErr)
		}
		if shares.Sign() == 0 {
			continue
		}
		state, callErr := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: vaults[index].Address, ABI: erc4626ABI, Method: "asset"},
			{Contract: vaults[index].Address, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{shares}},
		})
		if callErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s state: %w", vaults[index].Address, callErr)
		}
		asset, decodeErr := AddressAt(state[0], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s asset: %w", vaults[index].Address, decodeErr)
		}
		amount, decodeErr := BigIntAt(state[1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s converted assets: %w", vaults[index].Address, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		metadata, tokenErr := readToken(ctx, client, block, asset)
		if tokenErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault asset metadata: %w", tokenErr)
		}
		component := NewComponent("asset", metadata, amount,
			Source{Contract: vaults[index].Address, Method: "convertToAssets(balanceOf)"})
		component.Metadata = map[string]any{"shares": shares.String()}
		id := strings.ToLower(vaults[index].Address.Hex())
		groups = append(groups, Group{
			ID: "vault:" + id, MarketID: id, Label: "Yield", Components: []Component{component},
			Metadata: map[string]any{"vault": vaults[index].Address},
		})
	}
	return groups, nil
}

func (a *ListaAdapter) cdpGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != a.cdp.ChainID || block.Number < a.cdp.ActivationBlock {
		return nil, nil
	}
	headerCalls := make([]ContractCall, len(a.cdp.CandidateTokens))
	for index, address := range a.cdp.CandidateTokens {
		headerCalls[index] = ContractCall{Contract: a.cdp.Interaction, ABI: listaInteractionABI, Method: "collaterals", Args: []any{address}}
	}
	headers, err := client.ParallelCalls(ctx, block, headerCalls)
	if err != nil {
		return nil, fmt.Errorf("Lista CDP collateral registry: %w", err)
	}
	tokens := make([]common.Address, 0)
	positionCalls := make([]ContractCall, 0)
	for index, row := range headers {
		live, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista CDP collateral live flag: %w", decodeErr)
		}
		if live.Sign() == 0 {
			continue
		}
		address := a.cdp.CandidateTokens[index]
		tokens = append(tokens, address)
		positionCalls = append(positionCalls,
			ContractCall{Contract: a.cdp.Interaction, ABI: listaInteractionABI, Method: "locked", Args: []any{address, account}},
			ContractCall{Contract: a.cdp.Interaction, ABI: listaInteractionABI, Method: "borrowed", Args: []any{address, account}},
		)
	}
	positions, err := client.ParallelCalls(ctx, block, positionCalls)
	if err != nil {
		return nil, fmt.Errorf("Lista CDP positions: %w", err)
	}
	groups := make([]Group, 0)
	debtToken := Token{ChainID: BSC, Address: a.cdp.DebtToken.Address, Symbol: a.cdp.DebtToken.Symbol, Decimals: a.cdp.DebtToken.Decimals}
	for index, address := range tokens {
		locked, decodeErr := BigIntAt(positions[index*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista CDP collateral %s locked amount: %w", address, decodeErr)
		}
		borrowed, decodeErr := BigIntAt(positions[index*2+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista CDP collateral %s borrowed amount: %w", address, decodeErr)
		}
		if locked.Sign() == 0 && borrowed.Sign() == 0 {
			continue
		}
		metadata, tokenErr := readToken(ctx, client, block, address)
		if tokenErr != nil {
			return nil, fmt.Errorf("Lista CDP collateral metadata: %w", tokenErr)
		}
		components := make([]Component, 0, 2)
		if locked.Sign() > 0 {
			components = append(components, NewComponent("asset", metadata, locked,
				Source{Contract: a.cdp.Interaction, Method: "locked(token,account)"}))
		}
		if borrowed.Sign() > 0 {
			components = append(components, NewComponent("debt", debtToken, borrowed,
				Source{Contract: a.cdp.Interaction, Method: "borrowed(token,account)"}))
		}
		id := strings.ToLower(address.Hex())
		groups = append(groups, Group{
			ID: "cdp:" + id, MarketID: id, Label: "CDP", Components: components,
			NetValuePolicy: "floor-zero", Metadata: map[string]any{"collateralToken": address},
		})
	}
	return groups, nil
}

func (a *ListaAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	moolah, err := a.moolahGroups(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	vaults, err := a.vaultGroups(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	cdp, err := a.cdpGroups(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	return append(append(moolah, vaults...), cdp...), nil
}
