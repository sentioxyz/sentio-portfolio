package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const aaveV4MaximumReservesPerSpoke = 256

var aaveV4SpokeABI = MustABI(`[
  {"type":"function","name":"getReserveCount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getReserve","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"}],"outputs":[{"type":"tuple","components":[{"name":"underlying","type":"address"},{"name":"hub","type":"address"},{"name":"assetId","type":"uint16"},{"name":"decimals","type":"uint8"},{"name":"collateralRisk","type":"uint24"},{"name":"flags","type":"uint8"},{"name":"dynamicConfigKey","type":"uint32"}]}]},
  {"type":"function","name":"getUserSuppliedAssets","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getUserTotalDebt","stateMutability":"view","inputs":[{"name":"reserveId","type":"uint256"},{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type aaveV4Spoke struct {
	Label           string
	Address         common.Address
	ActivationBlock uint64
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

type AaveV4Adapter struct {
	adapterBase
	spokes      []aaveV4Spoke
	allowedHubs map[common.Address]struct{}
}

var aaveV4EthereumSpokes = []aaveV4Spoke{
	{Label: "Bluechip", Address: common.HexToAddress("0x973a023A77420ba610f06b3858aD991Df6d85A08"), ActivationBlock: 24_720_920},
	{Label: "Ethena Correlated", Address: common.HexToAddress("0x58131E79531caB1d52301228d1f7b842F26B9649"), ActivationBlock: 24_720_926},
	{Label: "Ethena Ecosystem", Address: common.HexToAddress("0xba1B3D55D249692b669A164024A838309B7508AF"), ActivationBlock: 24_720_923},
	{Label: "Forex", Address: common.HexToAddress("0xD8B93635b8C6d0fF98CbE90b5988E3F2d1Cd9da1"), ActivationBlock: 24_720_917},
	{Label: "Gold", Address: common.HexToAddress("0x65407b940966954b23dfA3caA5C0702bB42984DC"), ActivationBlock: 24_720_914},
	{Label: "Lombard BTC", Address: common.HexToAddress("0x7EC68b5695e803e98a21a9A05d744F28b0a7753D"), ActivationBlock: 24_720_911},
	{Label: "Main", Address: common.HexToAddress("0x94e7A5dCbE816e498b89aB752661904E2F56c485"), ActivationBlock: 24_720_899},
	{Label: "USDG Pendle", Address: common.HexToAddress("0x956d8e0A89cfa3744428C4641b5a53B56167a7f9"), ActivationBlock: 25_094_394},
	{Label: "EtherFi", Address: common.HexToAddress("0xbF10BDfE177dE0336aFD7fcCF80A904E15386219"), ActivationBlock: 24_720_905},
	{Label: "Kelp", Address: common.HexToAddress("0x3131FE68C4722e726fe6B2819ED68e514395B9a4"), ActivationBlock: 24_720_908},
	{Label: "Lido", Address: common.HexToAddress("0xe1900480ac69f0B296841Cd01cC37546d92F35Cd"), ActivationBlock: 24_720_902},
}

func newAaveV4Adapter() Adapter {
	return &AaveV4Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "aave-v4", Name: "Aave V4", Chains: []ChainID{Ethereum}}},
		spokes:      aaveV4EthereumSpokes,
		allowedHubs: map[common.Address]struct{}{
			common.HexToAddress("0xCca852Bc40e560adC3b1Cc58CA5b55638ce826c9"): {},
			common.HexToAddress("0x06002e9c4412CB7814a791eA3666D905871E536A"): {},
			common.HexToAddress("0x943827DCA022D0F354a8a8c332dA1e5Eb9f9F931"): {},
			common.HexToAddress("0x62d63197660c080236193CA60b70E49A08E90368"): {},
		},
	}
}

func decodeAaveV4Reserve(value any) (aaveV4ReserveData, error) {
	converted := abi.ConvertType(value, new(aaveV4ReserveData))
	reserve, ok := converted.(*aaveV4ReserveData)
	if !ok || reserve == nil {
		return aaveV4ReserveData{}, fmt.Errorf("reserve is %T", value)
	}
	return *reserve, nil
}

func (a *AaveV4Adapter) activeSpokes(block uint64) []aaveV4Spoke {
	active := make([]aaveV4Spoke, 0, len(a.spokes))
	for _, spoke := range a.spokes {
		if block >= spoke.ActivationBlock {
			active = append(active, spoke)
		}
	}
	return active
}

func (a *AaveV4Adapter) reserveCatalog(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) ([]aaveV4Reserve, error) {
	spokes := a.activeSpokes(block.Number)
	countCalls := make([]ContractCall, 0, len(spokes))
	for _, spoke := range spokes {
		countCalls = append(countCalls, ContractCall{Contract: spoke.Address, ABI: aaveV4SpokeABI, Method: "getReserveCount"})
	}
	counts, err := client.ParallelCalls(ctx, block, countCalls)
	if err != nil {
		return nil, fmt.Errorf("reserve counts: %w", err)
	}
	type reserveRef struct {
		spoke aaveV4Spoke
		id    *big.Int
	}
	refs := make([]reserveRef, 0)
	for index, values := range counts {
		count, decodeErr := BigIntAt(values, 0)
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
		if _, exists := a.allowedHubs[row.Hub]; !exists {
			return nil, fmt.Errorf("%s reserve %s has unknown hub %s", refs[index].spoke.Label, refs[index].id, row.Hub)
		}
		if row.Decimals > 36 {
			return nil, fmt.Errorf("%s reserve %s has invalid decimals %d", refs[index].spoke.Label, refs[index].id, row.Decimals)
		}
		reserves = append(reserves, aaveV4Reserve{
			Spoke: refs[index].spoke, ReserveID: refs[index].id, Hub: row.Hub,
			Token: Token{ChainID: Ethereum, Address: row.Underlying, Decimals: row.Decimals},
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
	if block.ChainID != Ethereum {
		return nil, nil
	}
	reserves, err := a.reserveCatalog(ctx, client, block)
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
	for _, spoke := range a.spokes {
		rows := components[spoke.Address]
		if len(rows) == 0 {
			continue
		}
		marketID := "spoke:" + strings.ToLower(spoke.Address.Hex())
		groups = append(groups, Group{
			ID: marketID, MarketID: marketID, Label: "Lending · " + spoke.Label,
			Components: rows, NetValuePolicy: "floor-zero",
			Metadata: map[string]any{"spoke": spoke.Address, "market": spoke.Label, "activationBlock": spoke.ActivationBlock},
		})
	}
	return groups, nil
}
