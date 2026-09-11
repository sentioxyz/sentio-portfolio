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
func TestSuiProtocolLiveHistory(t *testing.T) {
	raw := os.Getenv("PORTFOLIO_SUI_HISTORY_CASE")
	if raw == "" {
		t.Skip("set PORTFOLIO_SUI_HISTORY_CASE and runtime RPC/index configuration")
	}
	var sample struct {
		Protocol   string
		Owner      string
		Checkpoint uint64
		Expected   map[string]string // component kind + ':' + normalized coin type
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
	config := SentioIndexerConfig{GraphQLURL: os.Getenv("PORTFOLIO_SUI_HISTORY_GRAPHQL_URL"), StatusURL: os.Getenv("PORTFOLIO_SUI_HISTORY_STATUS_URL"), ProcessorVersion: os.Getenv("PORTFOLIO_SUI_HISTORY_VERSION")}
	reader := NewSuiProtocolReader(EngineConfig{SentioIndexers: map[string]SentioIndexerConfig{sample.Protocol: config}})
	pin, err := rpc.CheckpointBySequence(ctx, sample.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.Read(ctx, sample.Protocol, owner, pin, rpc)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	if result.Checkpoint != pin {
		t.Fatal("result pin changed")
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
		t.Fatalf("quantities at checkpoint %d: got %v, want %v", pin.Sequence, actual, sample.Expected)
	}
	t.Logf("reconciled %d quantities at checkpoint %d", len(actual), pin.Sequence)
}
