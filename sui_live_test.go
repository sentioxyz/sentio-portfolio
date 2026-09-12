package portfolio

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// The live test probes a real Sui mainnet gRPC endpoint. It is a contract check on the service's
// schema and head-only semantics rather than on any particular balance, so it asserts shapes and
// invariants, not amounts. PORTFOLIO_SUI_LIVE_ADDRESS optionally names an address whose holdings
// are enumerated and whose coin metadata is resolved.
func TestSuiLiveGRPCReader(t *testing.T) {
	endpoint := os.Getenv("PORTFOLIO_SUI_GRPC_URL")
	if endpoint == "" {
		t.Skip("set PORTFOLIO_SUI_GRPC_URL to a Sui mainnet gRPC endpoint (grpc://host:port or grpcs://host:port) to probe it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := DialSuiGRPC(ctx, 0, endpoint, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	latest, err := client.LatestCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Sequence == 0 || latest.Digest == ([32]byte{}) || latest.Timestamp.IsZero() {
		t.Fatalf("latest checkpoint = %+v", latest)
	}
	if age := time.Since(latest.Timestamp); age > 10*time.Minute {
		t.Fatalf("latest checkpoint is %s old", age)
	}
	t.Logf("latest checkpoint %d at %s", latest.Sequence, latest.Timestamp)

	again, err := client.CheckpointBySequence(ctx, latest.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if again != latest {
		t.Fatalf("checkpoint %d re-read as %+v, want %+v", latest.Sequence, again, latest)
	}
	if _, err := client.CheckpointBySequence(ctx, latest.Sequence+1_000_000_000); !errors.Is(err, errSuiCheckpointUnavailable) {
		t.Fatalf("future checkpoint: %v, want errSuiCheckpointUnavailable", err)
	}

	// The framework package address is a popular airdrop target, so it holds spam coins; whatever
	// it holds must decode with every coin type normalized and report both head observations.
	framework, _ := ParseSuiAddress("0x2")
	held, err := client.Holdings(ctx, framework, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held.HistoryUnsupported {
		t.Fatal("a head read was marked HistoryUnsupported")
	}
	if held.HeadBeforeRead == 0 || held.Checkpoint.Sequence == 0 || held.Checkpoint.Timestamp.IsZero() || held.Checkpoint.Digest == ([32]byte{}) {
		t.Fatalf("holdings omitted head observations: %+v", held)
	}
	t.Logf("framework address holds %d coin types; heads before=%d after=%d", len(held.Balances), held.HeadBeforeRead, held.Checkpoint.Sequence)

	// A pinned read is answered with nothing, by decision, without asking the service.
	pinned, err := client.Holdings(ctx, framework, &latest)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned.HistoryUnsupported || len(pinned.Balances) != 0 || pinned.Checkpoint != latest {
		t.Fatalf("pinned read = %+v, want no balances marked HistoryUnsupported at the pin", pinned)
	}

	metadata, unusable, err := client.CoinMetadata(ctx, []string{
		"0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI",
		"0x0000000000000000000000000000000000000000000000000000000000000001::nonexistent::NOPE",
	})
	if err != nil {
		t.Fatal(err)
	}
	sui, ok := metadata["0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"]
	if !ok || sui.Symbol != "SUI" || sui.Decimals != 9 {
		t.Fatalf("SUI metadata = %+v", sui)
	}
	if len(unusable) != 1 {
		t.Fatalf("unusable = %v, want the nonexistent coin only", unusable)
	}

	if raw := os.Getenv("PORTFOLIO_SUI_LIVE_ADDRESS"); raw != "" {
		owner, err := ParseSuiAddress(raw)
		if err != nil {
			t.Fatal(err)
		}
		holdings, err := client.Holdings(ctx, owner, nil)
		if err != nil {
			t.Fatal(err)
		}
		coinTypes := make([]string, 0, len(holdings.Balances))
		for _, balance := range holdings.Balances {
			if balance.Amount.Sign() <= 0 {
				t.Fatalf("balance %+v is not positive", balance)
			}
			coinTypes = append(coinTypes, balance.CoinType)
		}
		metadata, unusable, err := client.CoinMetadata(ctx, coinTypes)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s holds %d coin types in window %d..%d; %d with usable metadata, %d without",
			owner.Hex(), len(holdings.Balances), holdings.HeadBeforeRead, holdings.Checkpoint.Sequence,
			len(metadata), len(unusable))
	}
}
