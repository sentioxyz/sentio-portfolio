package portfolio

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestSparkSavingsDeployments(t *testing.T) {
	var adapter *AaveAdapter
	for _, candidate := range aaveAdapters() {
		if candidate.Info().ID == "spark" {
			adapter = candidate.(*AaveAdapter)
			break
		}
	}
	if adapter == nil {
		t.Fatal("Spark adapter is not registered")
	}
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, Base, Arbitrum, Avalanche, Optimism}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Spark chains = %v, want %v", got, want)
	}
	for _, test := range []struct {
		chainID ChainID
		count   int
		assets  []common.Address
	}{
		{chainID: Ethereum, count: 5},
		{
			chainID: Base,
			count:   1,
			assets: []common.Address{
				common.HexToAddress("0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"),
			},
		},
		{
			chainID: Arbitrum,
			count:   2,
			assets: []common.Address{
				common.HexToAddress("0xaf88d065e77c8cC2239327C5EDb3A432268e5831"),
				common.HexToAddress("0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9"),
			},
		},
		{
			chainID: Avalanche,
			count:   1,
			assets: []common.Address{
				common.HexToAddress("0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E"),
			},
		},
		{
			chainID: Optimism,
			count:   1,
			assets: []common.Address{
				common.HexToAddress("0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85"),
			},
		},
	} {
		vaults := adapter.vaults[test.chainID]
		if len(vaults) != test.count {
			t.Fatalf("Spark chain %d vault count = %d, want %d", test.chainID, len(vaults), test.count)
		}
		for index, want := range test.assets {
			if got, wantID := vaults[index].ID, strings.ToLower(vaults[index].Address.Hex()); got != wantID {
				t.Fatalf("Spark chain %d vault %d ID = %q, want canonical address %q", test.chainID, index, got, wantID)
			}
			if got := vaults[index].Asset.Address; got != want {
				t.Fatalf("Spark chain %d vault %d asset = %s, want %s", test.chainID, index, got, want)
			}
			if vaults[index].ActivationBlock == 0 {
				t.Fatalf("Spark chain %d vault %d has no activation block", test.chainID, index)
			}
		}
	}
}

func TestAaveV3HistoricalProviderErasAreContiguous(t *testing.T) {
	type era struct {
		provider string
		start    uint64
		end      uint64
	}
	want := map[ChainID][]era{
		Polygon: {
			{provider: "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", start: 25_826_028, end: 41_174_631},
			{provider: "0x9441B65EE553F70df9C77d45d3283B6BC24F222d", start: 41_174_632, end: 59_108_788},
			{provider: "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", start: 59_108_789, end: 62_249_156},
			{provider: "0x7F23D86Ee20D869112572136221e173428DD740B", start: 62_249_157, end: 67_532_798},
			{provider: "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", start: 67_532_799, end: 72_592_540},
			{provider: "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", start: 72_592_541},
		},
		Avalanche: {
			{provider: "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", start: 11_970_506, end: 28_384_510},
			{provider: "0x50ddd0Cd4266299527d25De9CBb55fE0EB8dAc30", start: 28_384_511, end: 47_712_700},
			{provider: "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", start: 47_712_701, end: 50_972_229},
			{provider: "0x7F23D86Ee20D869112572136221e173428DD740B", start: 50_972_230, end: 56_836_940},
			{provider: "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", start: 56_836_941, end: 63_632_022},
			{provider: "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", start: 63_632_023},
		},
		Optimism: {
			{provider: "0x69FA688f1Dc47d4B5d8029D5a35FB7a548310654", start: 4_365_693, end: 86_483_662},
			{provider: "0xd9Ca4878dd38B021583c1B669905592EAe76E044", start: 86_483_663, end: 122_423_343},
			{provider: "0x7deEB8aCE4220643D8edeC871a23807E4d006eE5", start: 122_423_344, end: 125_827_825},
			{provider: "0x7F23D86Ee20D869112572136221e173428DD740B", start: 125_827_826, end: 131_542_951},
			{provider: "0x14496b405D62c24F91f04Cda1c69Dc526D56fDE5", start: 131_542_952, end: 136_976_691},
			{provider: "0x243Aa95cAC2a25651eda86e80bEe66114413c43b", start: 136_976_692},
		},
	}
	var adapter *AaveAdapter
	for _, candidate := range aaveAdapters() {
		if candidate.Info().ID == "aave-v3" {
			adapter = candidate.(*AaveAdapter)
			break
		}
	}
	if adapter == nil {
		t.Fatal("Aave v3 adapter is not registered")
	}
	for chainID, eras := range want {
		markets := adapter.markets[chainID]
		if len(markets) != len(eras) {
			t.Fatalf("Aave v3 chain %d eras = %d, want %d", chainID, len(markets), len(eras))
		}
		for index, expected := range eras {
			market := markets[index]
			if market.DataProvider != common.HexToAddress(expected.provider) ||
				market.ActivationBlock != expected.start || market.DeactivationBlock != expected.end {
				t.Fatalf("Aave v3 chain %d era %d = %+v, want %+v", chainID, index, market, expected)
			}
			active, err := adapter.activeMarkets(chainID, expected.start)
			if err != nil || len(active) != 1 || active[0].DataProvider != market.DataProvider {
				t.Fatalf("Aave v3 chain %d start %d active = %+v, err=%v", chainID, expected.start, active, err)
			}
			if index > 0 && markets[index-1].DeactivationBlock+1 != market.ActivationBlock {
				t.Fatalf("Aave v3 chain %d eras %d/%d are not contiguous", chainID, index-1, index)
			}
		}
	}
}

func TestAaveV2NewChainHistoricalStarts(t *testing.T) {
	want := map[ChainID]uint64{Polygon: 12_687_302, Avalanche: 4_607_174}
	for _, candidate := range aaveAdapters() {
		if candidate.Info().ID != "aave-v2" {
			continue
		}
		adapter := candidate.(*AaveAdapter)
		for chainID, start := range want {
			markets := adapter.markets[chainID]
			if len(markets) != 1 || markets[0].ActivationBlock != start {
				t.Fatalf("Aave v2 chain %d markets = %+v, want start %d", chainID, markets, start)
			}
		}
		return
	}
	t.Fatal("Aave v2 adapter is not registered")
}

func TestAaveV2RewardDeployments(t *testing.T) {
	want := map[ChainID]struct {
		controller string
		token      string
		start      uint64
	}{
		Ethereum: {
			controller: "0xd784927Ff2f95ba542BfC824c8a8a98F3495f6b5",
			token:      "0x7Fc66500c84A76Ad7e9c93437bFc5Ac33E2dDaE9",
			start:      12_251_569,
		},
		Polygon: {
			controller: "0x357D51124f59836DeD84c8a1730D72B749d8BC23",
			token:      "0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270",
			start:      12_486_774,
		},
		Avalanche: {
			controller: "0x01D83Fe6A10D2f2B7AF17034343746188272cAc9",
			token:      "0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7",
			start:      3_424_262,
		},
	}
	for chainID, expected := range want {
		deployment, exists := aaveV2RewardDeployments[chainID]
		if !exists {
			t.Fatalf("Aave v2 chain %d has no reward deployment", chainID)
		}
		if deployment.Controller != common.HexToAddress(expected.controller) ||
			deployment.Token.Address != common.HexToAddress(expected.token) ||
			deployment.Token.ChainID != chainID || deployment.ActivationBlock != expected.start {
			t.Fatalf("Aave v2 chain %d reward deployment = %+v, want %+v", chainID, deployment, expected)
		}
	}
}

func TestDecodeAaveIncentiveAssetsIncludesDebtTokens(t *testing.T) {
	aToken := common.HexToAddress("0x0000000000000000000000000000000000000011")
	stableDebt := common.HexToAddress("0x0000000000000000000000000000000000000022")
	variableDebt := common.HexToAddress("0x0000000000000000000000000000000000000033")
	assets, err := decodeAaveIncentiveAssets(
		[]aaveReserve{{Symbol: "USDC"}},
		[][]any{{aToken, stableDebt, variableDebt}},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 3 {
		t.Fatalf("asset count = %d, want 3", len(assets))
	}
	for index, want := range []common.Address{aToken, stableDebt, variableDebt} {
		if assets[index] != want {
			t.Fatalf("asset %d = %s, want %s", index, assets[index], want)
		}
	}
}

func TestDecodeAaveIncentiveAssetsSkipsZeroAndDuplicates(t *testing.T) {
	aToken := common.HexToAddress("0x0000000000000000000000000000000000000011")
	assets, err := decodeAaveIncentiveAssets(
		[]aaveReserve{{Symbol: "USDC"}, {Symbol: "USDT"}},
		[][]any{
			{aToken, common.Address{}, common.Address{}},
			{aToken, common.Address{}, common.Address{}},
		},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0] != aToken {
		t.Fatalf("assets = %v, want [%s]", assets, aToken)
	}
}

func TestDecodeScaledBalanceIncentiveAssetsExcludesStableDebt(t *testing.T) {
	aToken := common.HexToAddress("0x0000000000000000000000000000000000000011")
	stableDebt := common.HexToAddress("0x0000000000000000000000000000000000000022")
	variableDebt := common.HexToAddress("0x0000000000000000000000000000000000000033")
	assets, err := decodeAaveIncentiveAssets(
		[]aaveReserve{{Symbol: "USDC"}},
		[][]any{{aToken, stableDebt, variableDebt}},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 || assets[0] != aToken || assets[1] != variableDebt {
		t.Fatalf("assets = %v, want [%s %s]", assets, aToken, variableDebt)
	}
}
