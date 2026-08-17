package portfolio

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestCurveLendingDeployments(t *testing.T) {
	adapter := newCurveLendingAdapter().(*curveLendingAdapter)
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, Arbitrum}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Curve Lending chains = %v, want %v", got, want)
	}
	arbitrum, exists := adapter.deployments[Arbitrum]
	if !exists {
		t.Fatal("Curve Lending has no Arbitrum deployment")
	}
	if got, want := arbitrum.oneWayFactory,
		common.HexToAddress("0xcaEC110C784c9DF37240a8Ce096D352A75922DeA"); got != want {
		t.Fatalf("Arbitrum one-way factory = %s, want %s", got, want)
	}
	if arbitrum.oneWayActivation != 193_652_535 {
		t.Fatalf("Arbitrum one-way activation = %d, want 193652535", arbitrum.oneWayActivation)
	}
	if arbitrum.v2Factory != (common.Address{}) || arbitrum.v2Activation != 0 {
		t.Fatalf("Arbitrum unexpectedly has Curve Lending v2: %#v", arbitrum)
	}
}

func TestCurveBoundedCount(t *testing.T) {
	tests := []struct {
		name    string
		value   *big.Int
		want    int
		wantErr bool
	}{
		{name: "zero", value: big.NewInt(0), want: 0},
		{name: "maximum", value: big.NewInt(curveMaxMarkets), want: curveMaxMarkets},
		{name: "over maximum", value: big.NewInt(curveMaxMarkets + 1), wantErr: true},
		{name: "negative", value: big.NewInt(-1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := curveBoundedCount([]any{test.value}, "test")
			if (err != nil) != test.wantErr {
				t.Fatalf("curveBoundedCount() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("curveBoundedCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCurveCollateralPresentationRules(t *testing.T) {
	savingsREUSD := common.HexToAddress("0x557AB1e003951A73c12D16F0fEA8490E39C33C35")
	savingsDOLA := common.HexToAddress("0xb45ad160634c528cc3d2926d9807104fa3157305")
	savingsFRXUSD := common.HexToAddress("0xcf62f905562626cfcdd2261162a51fd02fc9c5b6")
	if _, exists := curveUnderlyingCollateral[savingsREUSD]; !exists {
		t.Fatal("Savings reUSD must be normalized to reUSD")
	}
	for _, wrapper := range []common.Address{savingsDOLA, savingsFRXUSD} {
		if _, exists := curveUnderlyingCollateral[wrapper]; exists {
			t.Fatalf("%s must remain a wrapper token", wrapper)
		}
	}
}
