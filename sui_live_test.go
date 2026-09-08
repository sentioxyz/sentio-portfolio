package portfolio

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// The live tests probe real Sui mainnet endpoints. They are contract checks on each transport's
// schema and consistency semantics rather than on any particular balance, so they assert shapes
// and invariants, not amounts. PORTFOLIO_SUI_LIVE_ADDRESS optionally names an address whose
// holdings are enumerated and whose coin metadata is resolved.

func TestSuiLiveGraphQLReader(t *testing.T) {
	endpoint := os.Getenv("PORTFOLIO_SUI_GRAPHQL_URL")
	if endpoint == "" {
		t.Skip("set PORTFOLIO_SUI_GRAPHQL_URL to a Sui mainnet GraphQL endpoint to probe it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := DialSuiGraphQL(ctx, 0, endpoint, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	latest := suiLiveReaderChecks(ctx, t, client, true)

	first, last, err := client.BalancesRange(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first > latest.Sequence || last < latest.Sequence-1 {
		t.Fatalf("balances range %d-%d does not cover the latest checkpoint %d", first, last, latest.Sequence)
	}
	t.Logf("balances answerable for %d checkpoints", last-first+1)

	// A checkpoint far before the consistent range must be refused, not answered from the head.
	framework, _ := ParseSuiAddress("0x2")
	_, err = client.Holdings(ctx, framework, &SuiCheckpoint{Sequence: first / 2})
	if !errors.Is(err, errSuiOutsideConsistentRange) {
		t.Fatalf("checkpoint %d outside the range: %v, want errSuiOutsideConsistentRange", first/2, err)
	}
}

func TestSuiLiveJSONRPCReader(t *testing.T) {
	endpoint := os.Getenv("PORTFOLIO_SUI_JSONRPC_URL")
	if endpoint == "" {
		t.Skip("set PORTFOLIO_SUI_JSONRPC_URL to a Sui mainnet JSON-RPC endpoint to probe it")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := DialSuiJSONRPC(ctx, 0, endpoint, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	latest := suiLiveReaderChecks(ctx, t, client, false)

	// The transport reads only the head, so a pinned read is refused rather than approximated.
	framework, _ := ParseSuiAddress("0x2")
	_, err = client.Holdings(ctx, framework, &latest)
	if !errors.Is(err, errSuiPinnedReadUnsupported) {
		t.Fatalf("pinned read: %v, want errSuiPinnedReadUnsupported", err)
	}
}

// suiLiveReaderChecks runs the transport-neutral part of the probe and returns the latest
// checkpoint it observed.
func suiLiveReaderChecks(ctx context.Context, t *testing.T, client SuiReader, exact bool) SuiCheckpoint {
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
	// it holds must decode with every coin type normalized.
	framework, _ := ParseSuiAddress("0x2")
	held, err := client.Holdings(ctx, framework, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held.Exact != exact {
		t.Fatalf("holdings exact = %t, want %t", held.Exact, exact)
	}
	if held.HeadBeforeRead > held.Checkpoint.Sequence || held.Checkpoint.Sequence < latest.Sequence {
		t.Fatalf("holdings window %d..%d is not ordered after the latest checkpoint %d",
			held.HeadBeforeRead, held.Checkpoint.Sequence, latest.Sequence)
	}
	t.Logf("framework address holds %d coin types; window %d..%d", len(held.Balances), held.HeadBeforeRead, held.Checkpoint.Sequence)

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
		t.Logf("%s holds %d coin types in window %d..%d (exact=%t); %d with usable metadata, %d without",
			owner.Hex(), len(holdings.Balances), holdings.HeadBeforeRead, holdings.Checkpoint.Sequence,
			holdings.Exact, len(metadata), len(unusable))
	}
	return latest
}
