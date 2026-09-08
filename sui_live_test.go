package portfolio

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSuiLiveReader probes a real Sui mainnet GraphQL endpoint. It is a contract check on the
// service's schema and consistency semantics rather than on any particular balance, so it asserts
// shapes and invariants, not amounts.
func TestSuiLiveReader(t *testing.T) {
	endpoint := os.Getenv("PORTFOLIO_SUI_GRAPHQL_URL")
	if endpoint == "" {
		t.Skip("set PORTFOLIO_SUI_GRAPHQL_URL to a Sui mainnet GraphQL endpoint to probe it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := DialSui(ctx, 0, endpoint, SuiMainnetChainIdentifier)
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
	first, last, err := client.BalancesRange(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first > latest.Sequence || last < latest.Sequence-1 {
		t.Fatalf("balances range %d-%d does not cover the latest checkpoint %d", first, last, latest.Sequence)
	}
	t.Logf("latest checkpoint %d at %s; balances answerable for %d checkpoints", latest.Sequence, latest.Timestamp, last-first+1)

	again, err := client.CheckpointBySequence(ctx, latest.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if again != latest {
		t.Fatalf("checkpoint %d re-read as %+v, want %+v", latest.Sequence, again, latest)
	}

	// The framework package address is a popular airdrop target, so it usually holds a page or two
	// of spam coins; whatever it holds must decode with every coin type normalized.
	framework, _ := ParseSuiAddress("0x2")
	held, err := client.Balances(ctx, latest, framework)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("framework address holds %d coin types", len(held))

	// A checkpoint far before the consistent range must be refused, not answered from the head.
	_, err = client.Balances(ctx, SuiCheckpoint{Sequence: first / 2}, framework)
	if err == nil {
		t.Fatal("a checkpoint outside the consistent range was answered")
	}
	t.Logf("checkpoint %d refused: %v", first/2, err)

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

	// Any address named through the environment is read at the pinned checkpoint and every coin
	// type it holds is normalized and priced-able in shape.
	if raw := os.Getenv("PORTFOLIO_SUI_LIVE_ADDRESS"); raw != "" {
		owner, err := ParseSuiAddress(raw)
		if err != nil {
			t.Fatal(err)
		}
		balances, err := client.Balances(ctx, latest, owner)
		if err != nil {
			t.Fatal(err)
		}
		coinTypes := make([]string, 0, len(balances))
		for _, balance := range balances {
			if balance.Amount.Sign() <= 0 {
				t.Fatalf("balance %+v is not positive", balance)
			}
			coinTypes = append(coinTypes, balance.CoinType)
		}
		metadata, unusable, err := client.CoinMetadata(ctx, coinTypes)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s holds %d coin types at checkpoint %d; %d with usable metadata, %d without",
			owner.Hex(), len(balances), latest.Sequence, len(metadata), len(unusable))
	}
}
