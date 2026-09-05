package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const (
	aaveV4MaximumReservesPerSpoke = 256
	aaveV4MaximumSpokes           = 512
)

var aaveV4HubABI = MustABI(`[
  {"type":"function","name":"getAssetCount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getSpokeCount","stateMutability":"view","inputs":[{"name":"assetId","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getSpokeAddress","stateMutability":"view","inputs":[{"name":"assetId","type":"uint256"},{"name":"index","type":"uint256"}],"outputs":[{"type":"address"}]}
]`)

var aaveV4SpokeABI = MustABI(`[
  {"type":"function","name":"getReserveCount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getReserve","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"}],"outputs":[{"type":"tuple","components":[{"name":"underlying","type":"address"},{"name":"hub","type":"address"},{"name":"assetId","type":"uint16"},{"name":"decimals","type":"uint8"},{"name":"collateralRisk","type":"uint24"},{"name":"flags","type":"uint8"},{"name":"dynamicConfigKey","type":"uint32"}]}]},
  {"type":"function","name":"getUserSuppliedAssets","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getUserTotalDebt","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type aaveV4Spoke struct {
	Label   string
	Address common.Address
}

type aaveV4ReserveData struct {
	Underlying       common.Address
	Hub              common.Address
	AssetId          uint16
	Decimals         uint8
	CollateralRisk   *big.Int
	Flags            uint8
	DynamicConfigKey uint32
}

type aaveV4Reserve struct {
	Spoke     aaveV4Spoke
	ReserveID *big.Int
	Hub       common.Address
	Token     Token
}

func aaveV4ReserveToken(chainID ChainID, row aaveV4ReserveData) Token {
	return Token{ChainID: chainID, Address: row.Underlying, Decimals: row.Decimals}
}

type AaveV4Adapter struct {
	adapterBase
	hubs        map[ChainID][]aaveV4Hub
	spokeLabels map[common.Address]string
	allowedHubs map[ChainID]map[common.Address]struct{}
}

// Spokes are enumerated from the hubs at the pinned block (getAssetCount ->
// getSpokeCount -> getSpokeAddress), so a newly listed spoke can never be missed the way
// the old hardcoded list missed 39 of 51 live spokes. Only display names are static:
// a stale entry here degrades a label, never a position.
var aaveV4SpokeLabels = map[common.Address]string{
	common.HexToAddress("0x973a023A77420ba610f06b3858aD991Df6d85A08"): "Bluechip",
	common.HexToAddress("0x58131E79531caB1d52301228d1f7b842F26B9649"): "Ethena Correlated",
	common.HexToAddress("0xba1B3D55D249692b669A164024A838309B7508AF"): "Ethena Ecosystem",
	common.HexToAddress("0xD8B93635b8C6d0fF98CbE90b5988E3F2d1Cd9da1"): "Forex",
	common.HexToAddress("0x65407b940966954b23dfA3caA5C0702bB42984DC"): "Gold",
	common.HexToAddress("0x7EC68b5695e803e98a21a9A05d744F28b0a7753D"): "Lombard BTC",
	common.HexToAddress("0x94e7A5dCbE816e498b89aB752661904E2F56c485"): "Main",
	common.HexToAddress("0x956d8e0A89cfa3744428C4641b5a53B56167a7f9"): "USDG Pendle",
	common.HexToAddress("0x774B9655413C34809c1F1B16b654465a89EbE989"): "USDG Syrup",
	common.HexToAddress("0xbF10BDfE177dE0336aFD7fcCF80A904E15386219"): "EtherFi",
	common.HexToAddress("0x3131FE68C4722e726fe6B2819ED68e514395B9a4"): "Kelp",
	common.HexToAddress("0xe1900480ac69f0B296841Cd01cC37546d92F35Cd"): "Lido",
}

type aaveV4Hub struct {
	Address common.Address
	// The hub's deployment block: an eth_call against a hub that has no code yet
	// returns empty data and fails the strict enumeration batch, so a fixed-block scan
	// inside the gap would drop the whole Aave v4 surface. Deployment blocks are closed
	// history, verified by eth_getCode binary search.
	Window deploymentWindow
}

var aaveV4EthereumHubs = []aaveV4Hub{
	{Address: common.HexToAddress("0xCca852Bc40e560adC3b1Cc58CA5b55638ce826c9"), Window: deploymentWindow{ActivationBlock: 24_720_891}},
	{Address: common.HexToAddress("0x06002e9c4412CB7814a791eA3666D905871E536A"), Window: deploymentWindow{ActivationBlock: 24_720_895}},
	{Address: common.HexToAddress("0x943827DCA022D0F354a8a8c332dA1e5Eb9f9F931"), Window: deploymentWindow{ActivationBlock: 24_720_887}},
	{Address: common.HexToAddress("0x62d63197660c080236193CA60b70E49A08E90368"), Window: deploymentWindow{ActivationBlock: 25_318_132}},
}

var aaveV4Hubs = map[ChainID][]aaveV4Hub{
	Ethereum: aaveV4EthereumHubs,
	Avalanche: {
		{
			Address: common.HexToAddress("0xd07369fAE4A5BB13c9Ce446B052c7867B1AbDf6e"),
			Window:  deploymentWindow{ActivationBlock: 89_721_368},
		},
	},
}

func activeAaveV4Hubs(hubs []aaveV4Hub, block uint64) []common.Address {
	active := make([]common.Address, 0, len(hubs))
	for _, hub := range hubs {
		if hub.Window.ActiveAt(block) {
			active = append(active, hub.Address)
		}
	}
	return active
}

func newAaveV4Adapter() Adapter {
	allowed := make(map[ChainID]map[common.Address]struct{}, len(aaveV4Hubs))
	for chainID, hubs := range aaveV4Hubs {
		allowed[chainID] = make(map[common.Address]struct{}, len(hubs))
		for _, hub := range hubs {
			allowed[chainID][hub.Address] = struct{}{}
		}
	}
	return &AaveV4Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "aave-v4", Name: "Aave V4", Chains: deploymentChains(aaveV4Hubs)}},
		hubs:        aaveV4Hubs,
		spokeLabels: aaveV4SpokeLabels,
		allowedHubs: allowed,
	}
}

func aaveV4SpokeLabel(labels map[common.Address]string, spoke common.Address) string {
	if label, exists := labels[spoke]; exists {
		return label
	}
	return strings.ToLower(spoke.Hex())
}

// enumerateSpokes resolves the live spoke set from the hubs at the pinned block:
// getAssetCount -> getSpokeCount(assetId) -> getSpokeAddress(assetId, index), deduplicated
// in discovery order. Because the set is read from chain state at the scan block, newly
// listed spokes appear immediately and historical scans see exactly the spokes listed then.
func (a *AaveV4Adapter) enumerateSpokes(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) ([]aaveV4Spoke, error) {
	hubs := activeAaveV4Hubs(a.hubs[block.ChainID], block.Number)
	if len(hubs) == 0 {
		return nil, nil
	}
	countCalls := make([]ContractCall, len(hubs))
	for index, hub := range hubs {
		countCalls[index] = ContractCall{Contract: hub, ABI: aaveV4HubABI, Method: "getAssetCount"}
	}
	countRows, err := client.ParallelCalls(ctx, block, countCalls)
	if err != nil {
		return nil, fmt.Errorf("hub asset counts: %w", err)
	}
	type hubAsset struct {
		hub     common.Address
		assetID *big.Int
	}
	assets := make([]hubAsset, 0)
	for index, values := range countRows {
		count, decodeErr := BigIntAt(values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("hub %s asset count: %w", hubs[index], decodeErr)
		}
		if !count.IsUint64() || count.Uint64() > aaveV4MaximumReservesPerSpoke {
			return nil, fmt.Errorf("hub %s asset count %s exceeds maximum", hubs[index], count)
		}
		for id := uint64(0); id < count.Uint64(); id++ {
			assets = append(assets, hubAsset{hub: hubs[index], assetID: new(big.Int).SetUint64(id)})
		}
	}
	spokeCountCalls := make([]ContractCall, len(assets))
	for index, asset := range assets {
		spokeCountCalls[index] = ContractCall{
			Contract: asset.hub, ABI: aaveV4HubABI, Method: "getSpokeCount", Args: []any{asset.assetID},
		}
	}
	spokeCountRows, err := client.ParallelCalls(ctx, block, spokeCountCalls)
	if err != nil {
		return nil, fmt.Errorf("hub spoke counts: %w", err)
	}
	type attachment struct {
		asset hubAsset
		index *big.Int
	}
	attachments := make([]attachment, 0)
	for index, values := range spokeCountRows {
		count, decodeErr := BigIntAt(values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("hub %s asset %s spoke count: %w", assets[index].hub, assets[index].assetID, decodeErr)
		}
		if !count.IsUint64() || count.Uint64() > aaveV4MaximumSpokes {
			return nil, fmt.Errorf("hub %s asset %s spoke count %s exceeds maximum", assets[index].hub, assets[index].assetID, count)
		}
		for position := uint64(0); position < count.Uint64(); position++ {
			attachments = append(attachments, attachment{asset: assets[index], index: new(big.Int).SetUint64(position)})
		}
	}
	addressCalls := make([]ContractCall, len(attachments))
	for index, entry := range attachments {
		addressCalls[index] = ContractCall{
			Contract: entry.asset.hub, ABI: aaveV4HubABI, Method: "getSpokeAddress",
			Args: []any{entry.asset.assetID, entry.index},
		}
	}
	addressRows, err := client.ParallelCalls(ctx, block, addressCalls)
	if err != nil {
		return nil, fmt.Errorf("hub spoke addresses: %w", err)
	}
	seen := make(map[common.Address]struct{})
	spokes := make([]aaveV4Spoke, 0)
	for index, values := range addressRows {
		address, decodeErr := AddressAt(values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("spoke address %d: %w", index, decodeErr)
		}
		if address == (common.Address{}) {
			return nil, fmt.Errorf("hub %s listed a zero spoke", attachments[index].asset.hub)
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		spokes = append(spokes, aaveV4Spoke{Label: aaveV4SpokeLabel(a.spokeLabels, address), Address: address})
	}
	if len(spokes) > aaveV4MaximumSpokes {
		return nil, fmt.Errorf("spoke count %d exceeds maximum %d", len(spokes), aaveV4MaximumSpokes)
	}
	return spokes, nil
}

func decodeAaveV4Reserve(value any) (aaveV4ReserveData, error) {
	converted := abi.ConvertType(value, new(aaveV4ReserveData))
	reserve, ok := converted.(*aaveV4ReserveData)
	if !ok || reserve == nil {
		return aaveV4ReserveData{}, fmt.Errorf("reserve is %T", value)
	}
	return *reserve, nil
}

func (a *AaveV4Adapter) reserveCatalog(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	spokes []aaveV4Spoke,
) ([]aaveV4Reserve, error) {
	countCalls := make([]ContractCall, 0, len(spokes))
	for _, spoke := range spokes {
		countCalls = append(countCalls, ContractCall{Contract: spoke.Address, ABI: aaveV4SpokeABI, Method: "getReserveCount"})
	}
	// The hubs list every authorized liquidity consumer, and only the lending market
	// spokes implement getReserveCount (12 of 51 today) — a revert here classifies the
	// entry as a non-market module, not as an error. Transport failures still fail.
	counts, err := client.ParallelCallsAllowFailure(ctx, block, countCalls)
	if err != nil {
		return nil, fmt.Errorf("reserve counts: %w", err)
	}
	type reserveRef struct {
		spoke aaveV4Spoke
		id    *big.Int
	}
	refs := make([]reserveRef, 0)
	for index, row := range counts {
		if row.Error != nil {
			if executionReverted(row.Error) {
				continue
			}
			return nil, fmt.Errorf("%s reserve count: %w", spokes[index].Label, row.Error)
		}
		count, decodeErr := BigIntAt(row.Values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s reserve count: %w", spokes[index].Label, decodeErr)
		}
		if !count.IsUint64() || count.Uint64() > aaveV4MaximumReservesPerSpoke {
			return nil, fmt.Errorf("%s reserve count %s exceeds maximum %d", spokes[index].Label, count, aaveV4MaximumReservesPerSpoke)
		}
		for id := uint64(0); id < count.Uint64(); id++ {
			refs = append(refs, reserveRef{spoke: spokes[index], id: new(big.Int).SetUint64(id)})
		}
	}
	reserveCalls := make([]ContractCall, 0, len(refs))
	for _, ref := range refs {
		reserveCalls = append(reserveCalls, ContractCall{Contract: ref.spoke.Address, ABI: aaveV4SpokeABI, Method: "getReserve", Args: []any{ref.id}})
	}
	rows, err := client.ParallelCalls(ctx, block, reserveCalls)
	if err != nil {
		return nil, fmt.Errorf("reserves: %w", err)
	}
	reserves := make([]aaveV4Reserve, 0, len(rows))
	for index, values := range rows {
		if len(values) != 1 {
			return nil, fmt.Errorf("%s reserve %s returned %d values", refs[index].spoke.Label, refs[index].id, len(values))
		}
		row, decodeErr := decodeAaveV4Reserve(values[0])
		if decodeErr != nil {
			return nil, fmt.Errorf("%s reserve %s: %w", refs[index].spoke.Label, refs[index].id, decodeErr)
		}
		if row.Underlying == (common.Address{}) {
			return nil, fmt.Errorf("%s reserve %s has zero underlying", refs[index].spoke.Label, refs[index].id)
		}
		if _, exists := a.allowedHubs[block.ChainID][row.Hub]; !exists {
			return nil, fmt.Errorf("%s reserve %s has unknown hub %s", refs[index].spoke.Label, refs[index].id, row.Hub)
		}
		if row.Decimals > 36 {
			return nil, fmt.Errorf("%s reserve %s has invalid decimals %d", refs[index].spoke.Label, refs[index].id, row.Decimals)
		}
		reserves = append(reserves, aaveV4Reserve{
			Spoke: refs[index].spoke, ReserveID: refs[index].id, Hub: row.Hub,
			Token: aaveV4ReserveToken(block.ChainID, row),
		})
	}
	return reserves, nil
}

func (a *AaveV4Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if _, supported := a.hubs[block.ChainID]; !supported {
		return nil, nil
	}
	spokes, err := a.enumerateSpokes(ctx, client, block)
	if err != nil {
		return nil, err
	}
	reserves, err := a.reserveCatalog(ctx, client, block, spokes)
	if err != nil {
		return nil, err
	}
	positionCalls := make([]ContractCall, 0, len(reserves)*2)
	for _, reserve := range reserves {
		positionCalls = append(positionCalls,
			ContractCall{Contract: reserve.Spoke.Address, ABI: aaveV4SpokeABI, Method: "getUserSuppliedAssets", Args: []any{reserve.ReserveID, account}},
			ContractCall{Contract: reserve.Spoke.Address, ABI: aaveV4SpokeABI, Method: "getUserTotalDebt", Args: []any{reserve.ReserveID, account}},
		)
	}
	amounts, err := client.ParallelCalls(ctx, block, positionCalls)
	if err != nil {
		return nil, fmt.Errorf("account positions: %w", err)
	}
	type activeReserve struct {
		reserve        aaveV4Reserve
		supplied, debt *big.Int
	}
	active := make([]activeReserve, 0)
	for index, reserve := range reserves {
		supplied, suppliedErr := BigIntAt(amounts[index*2], 0)
		if suppliedErr != nil {
			return nil, fmt.Errorf("%s reserve %s supplied: %w", reserve.Spoke.Label, reserve.ReserveID, suppliedErr)
		}
		debt, debtErr := BigIntAt(amounts[index*2+1], 0)
		if debtErr != nil {
			return nil, fmt.Errorf("%s reserve %s debt: %w", reserve.Spoke.Label, reserve.ReserveID, debtErr)
		}
		if supplied.Sign() > 0 || debt.Sign() > 0 {
			active = append(active, activeReserve{reserve: reserve, supplied: supplied, debt: debt})
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	tokenAddresses := make([]common.Address, 0)
	seenTokens := make(map[common.Address]struct{})
	for _, row := range active {
		if _, exists := seenTokens[row.reserve.Token.Address]; exists {
			continue
		}
		seenTokens[row.reserve.Token.Address] = struct{}{}
		tokenAddresses = append(tokenAddresses, row.reserve.Token.Address)
	}
	symbolCalls := make([]ContractCall, 0, len(tokenAddresses))
	for _, address := range tokenAddresses {
		symbolCalls = append(symbolCalls, ContractCall{Contract: address, ABI: erc20ABI, Method: "symbol"})
	}
	symbolRows, err := client.ParallelCalls(ctx, block, symbolCalls)
	if err != nil {
		return nil, fmt.Errorf("token symbols: %w", err)
	}
	symbols := make(map[common.Address]string, len(tokenAddresses))
	for index, values := range symbolRows {
		symbol, decodeErr := StringAt(values, 0)
		if decodeErr != nil || symbol == "" || len(symbol) > 64 {
			return nil, fmt.Errorf("token %s has invalid symbol", tokenAddresses[index])
		}
		symbols[tokenAddresses[index]] = symbol
	}
	components := make(map[common.Address][]Component)
	for _, row := range active {
		token := row.reserve.Token
		token.Symbol = symbols[token.Address]
		metadata := map[string]any{"reserveId": row.reserve.ReserveID.String(), "hub": row.reserve.Hub}
		if row.supplied.Sign() > 0 {
			component := NewComponent("asset", token, row.supplied, Source{Contract: row.reserve.Spoke.Address, Method: "getUserSuppliedAssets"})
			component.Metadata = metadata
			components[row.reserve.Spoke.Address] = append(components[row.reserve.Spoke.Address], component)
		}
		if row.debt.Sign() > 0 {
			component := NewComponent("debt", token, row.debt, Source{Contract: row.reserve.Spoke.Address, Method: "getUserTotalDebt"})
			component.Metadata = metadata
			components[row.reserve.Spoke.Address] = append(components[row.reserve.Spoke.Address], component)
		}
	}
	groups := make([]Group, 0, len(components))
	for _, spoke := range spokes {
		rows := components[spoke.Address]
		if len(rows) == 0 {
			continue
		}
		marketID := "spoke:" + strings.ToLower(spoke.Address.Hex())
		groups = append(groups, Group{
			ID: marketID, MarketID: marketID, Label: "Lending · " + spoke.Label,
			Components: rows, NetValuePolicy: "floor-zero",
			Metadata: map[string]any{"spoke": spoke.Address, "market": spoke.Label},
		})
	}
	return groups, nil
}
