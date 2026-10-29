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

// listaMoolahDeployment anchors one Moolah core: the market and vault sets themselves
// come from the Lista indexer at the pinned block (Moolah market creation is
// permissionless and has no enumeration view, so any embedded list rots — the 2026-08-13
// manifest held 56 of 484 live markets within a week). Only closed history stays static:
// the hand-deployed seed vaults predate the factory and cannot grow.
type listaMoolahDeployment struct {
	ChainID            ChainID
	Address            common.Address
	ActivationBlock    uint64
	VaultFactory       common.Address
	VaultFactoryWindow deploymentWindow
	SeedVaults         map[common.Address]struct{}
}

func listaSeedVaults(addresses ...string) map[common.Address]struct{} {
	seeds := make(map[common.Address]struct{}, len(addresses))
	for _, address := range addresses {
		seeds[common.HexToAddress(address)] = struct{}{}
	}
	return seeds
}

var listaMoolahDeployments = map[ChainID]listaMoolahDeployment{
	BSC: {
		ChainID:            BSC,
		Address:            common.HexToAddress("0x8F73b65B4caAf64FBA2aF91cC5D4a2A1318E5D8C"),
		ActivationBlock:    48_172_369,
		VaultFactory:       common.HexToAddress("0x2a0cb6401fd3c6196750dc6b46702040761d9671"),
		VaultFactoryWindow: deploymentWindow{ActivationBlock: 48_172_369},
		SeedVaults: listaSeedVaults(
			"0x57134a64B7cD9F9eb72F8255A671F5Bf2fe3E2d0",
			"0xfa27f172e0b6ebcEF9c51ABf817E2cb142FbE627",
			"0xe46b8e65006e6450bdd8cb7d3274ab4f76f4c705",
			"0x6d6783c146f2b0b2774c1725297f1845dc502525",
			"0x384729e442b7636709896e9a3bef63ef70c22fb0",
			"0x68e83ca4c2869fc6e92774e549ff9d547eae24ab",
			"0x9a17fd5cb8efc25d11567e713ae795a89775a759",
			"0x4e82fa869f8d05c8f94900d4652fdb82f3c7a004",
		),
	},
	Ethereum: {
		ChainID:         Ethereum,
		Address:         common.HexToAddress("0xf820fb4680712cd7263a0d3d024d5b5aea82fd70"),
		ActivationBlock: 23_445_769,
		SeedVaults: listaSeedVaults(
			"0x1a9bee2f5c85f6b4a0221fb1c733246af5306ae3",
		),
	},
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
	Version     int                `json:"version"`
	GeneratedAt string             `json:"generatedAt"`
	Sources     []string           `json:"sources"`
	Scope       string             `json:"scope"`
	CDP         listaCDPDeployment `json:"cdp"`
}

//go:embed lista-markets.json
var listaManifestJSON []byte

var listaDeployments = mustListaManifest()

func mustListaManifest() listaManifest {
	var manifest listaManifest
	if err := json.Unmarshal(listaManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode Lista manifest: %w", err))
	}
	if manifest.Version != 1 || manifest.GeneratedAt == "" || len(manifest.Sources) == 0 || manifest.Scope == "" ||
		manifest.CDP.Interaction == (common.Address{}) || len(manifest.CDP.CandidateTokens) == 0 {
		panic("invalid Lista manifest")
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
	moolah  map[ChainID]listaMoolahDeployment
	indexer listaPositionIndexer
	cdp     listaCDPDeployment
}

func newListaAdapter(config SentioIndexerConfig) Adapter {
	return newListaAdapterWithIndexer(newListaIndexer(config))
}

func newListaAdapterWithIndexer(indexer listaPositionIndexer) *ListaAdapter {
	return &ListaAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "lista", Name: "Lista DAO", Chains: []ChainID{Ethereum, BSC},
		}},
		moolah:  listaMoolahDeployments,
		indexer: indexer,
		cdp:     listaDeployments.CDP,
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
		return false, fmt.Errorf("Lista index returned uncreated Moolah market %s", held.holding.id)
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
	deployment listaMoolahDeployment,
	marketIDs []common.Hash,
) ([]Group, error) {
	if len(marketIDs) == 0 {
		return nil, nil
	}
	positionCalls := make([]ContractCall, len(marketIDs)+1)
	positionCalls[0] = ContractCall{Contract: deployment.Address, ABI: listaMoolahABI, Method: "feeRecipient"}
	for index, id := range marketIDs {
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
		holding := listaMoolahHolding{marketIDs[index], supply, borrow, collateral}
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
	vaults []common.Address,
) ([]Group, error) {
	if len(vaults) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(vaults))
	for index, vault := range vaults {
		calls[index] = ContractCall{Contract: vault, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Lista Moolah vault balances: %w", err)
	}
	groups := make([]Group, 0)
	for index, row := range rows {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s balance: %w", vaults[index], decodeErr)
		}
		if shares.Sign() == 0 {
			continue
		}
		state, callErr := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: vaults[index], ABI: erc4626ABI, Method: "asset"},
			{Contract: vaults[index], ABI: erc4626ABI, Method: "convertToAssets", Args: []any{shares}},
		})
		if callErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s state: %w", vaults[index], callErr)
		}
		asset, decodeErr := AddressAt(state[0], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s asset: %w", vaults[index], decodeErr)
		}
		amount, decodeErr := BigIntAt(state[1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault %s converted assets: %w", vaults[index], decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		metadata, tokenErr := readToken(ctx, client, block, asset)
		if tokenErr != nil {
			return nil, fmt.Errorf("Lista Moolah vault asset metadata: %w", tokenErr)
		}
		component := NewComponent("asset", metadata, amount,
			Source{Contract: vaults[index], Method: "convertToAssets(balanceOf)"})
		component.Metadata = map[string]any{"shares": shares.String()}
		id := strings.ToLower(vaults[index].Hex())
		groups = append(groups, Group{
			ID: "vault:" + id, MarketID: id, Label: "Yield", Components: []Component{component},
			Metadata: map[string]any{"vault": vaults[index]},
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
	groups := make([]Group, 0)
	if deployment, exists := a.moolah[block.ChainID]; exists && block.Number >= deployment.ActivationBlock {
		refs, err := a.indexer.PositionRefs(ctx, client, block, account, deployment)
		if err != nil {
			return nil, err
		}
		moolah, err := a.moolahGroups(ctx, client, block, account, deployment, refs.MarketIDs)
		if err != nil {
			return nil, err
		}
		for index := range moolah {
			if moolah[index].Metadata == nil {
				moolah[index].Metadata = map[string]any{}
			}
			moolah[index].Metadata["indexerBlock"] = refs.IndexerBlock
		}
		groups = append(groups, moolah...)
		vaults, err := a.vaultGroups(ctx, client, block, account, refs.Vaults)
		if err != nil {
			return nil, err
		}
		groups = append(groups, vaults...)
	}
	cdp, err := a.cdpGroups(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	return append(groups, cdp...), nil
}
