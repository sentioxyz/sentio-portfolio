package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"reflect"
	"testing"
	"time"
)

// Expected quantities must come from an independent chain/explorer observation,
// supplied at runtime. No deployment identifiers or captured responses belong here.
func TestSuiProtocolLiveLatest(t *testing.T) {
	raw := os.Getenv("PORTFOLIO_SUI_LATEST_CASE")
	if raw == "" {
		t.Skip("set PORTFOLIO_SUI_LATEST_CASE and runtime gRPC configuration")
	}
	var sample struct {
		Protocol string
		Owner    string
		Expected map[string]string // component kind + ':' + normalized coin type
	}
	if err := json.Unmarshal([]byte(raw), &sample); err != nil {
		t.Fatal(err)
	}
	if len(sample.Expected) == 0 {
		t.Fatal("independent expected quantities are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rpc, err := DialSuiGRPC(ctx, 0, os.Getenv("PORTFOLIO_SUI_GRPC_URL"), SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	defer rpc.Close()
	owner, err := ParseSuiAddress(sample.Owner)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewSuiProtocolReader()
	result, err := reader.ReadLatest(ctx, sample.Protocol, owner, rpc)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	if result.HeadBeforeRead > result.Checkpoint.Sequence {
		t.Fatal("invalid latest observation window")
	}
	amounts := make(map[string]*big.Int)
	for _, group := range result.Groups {
		for _, component := range group.Components {
			key := component.Kind + ":" + component.Coin.CoinType
			if amounts[key] == nil {
				amounts[key] = new(big.Int)
			}
			n, ok := new(big.Int).SetString(component.AmountRaw, 10)
			if !ok {
				t.Fatal("invalid quantity")
			}
			amounts[key].Add(amounts[key], n)
		}
	}
	actual := make(map[string]string)
	for key, n := range amounts {
		actual[key] = n.String()
	}
	if !reflect.DeepEqual(actual, sample.Expected) {
		t.Fatalf("quantities at checkpoint %d: got %v, want %v", result.Checkpoint.Sequence, actual, sample.Expected)
	}
	t.Logf("reconciled %d quantities at checkpoint %d", len(actual), result.Checkpoint.Sequence)
}
