package portfolio

import (
	"reflect"
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
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, Base, Arbitrum}; !reflect.DeepEqual(got, want) {
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
	} {
		vaults := adapter.vaults[test.chainID]
		if len(vaults) != test.count {
			t.Fatalf("Spark chain %d vault count = %d, want %d", test.chainID, len(vaults), test.count)
		}
		for index, want := range test.assets {
			if got := vaults[index].Asset.Address; got != want {
				t.Fatalf("Spark chain %d vault %d asset = %s, want %s", test.chainID, index, got, want)
			}
			if vaults[index].ActivationBlock == 0 {
				t.Fatalf("Spark chain %d vault %d has no activation block", test.chainID, index)
			}
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
