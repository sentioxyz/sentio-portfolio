package portfolio

import (
	"context"
	"slices"
	"sync"
	"testing"
)

func TestNaviDiscoveryCachesAndFollowsOnlyNewCreations(t *testing.T) {
	f := latestFixture()
	f.addMarket(0, 2, "0x11", "1", false)
	f.addMarket(1, 2, "0x11", "1", false)
	f.addMarket(2, 2, "0x11", "1", false)
	r := NewSuiProtocolReader()
	ctx := context.Background()
	ids, err := r.discoverNaviMarkets(ctx, f)
	if err != nil || len(ids) != 3 || f.transactionReads != 1 {
		t.Fatalf("initial: %v %v reads=%d", ids, err, f.transactionReads)
	}
	ids[0] = "mutated caller copy"
	ids, err = r.discoverNaviMarkets(ctx, f)
	if err != nil || len(ids) != 3 || f.transactionReads != 1 || slices.Contains(ids, "mutated caller copy") {
		t.Fatalf("cached: %v %v", ids, err)
	}
	// A later creation must be discovered even though an older inventory is cached.
	old := f.tables[naviMainStorage][0]
	f.versions[old.Version] = old
	f.addMarket(3, 3, "0x11", "1", false)
	current := latestObject(old.ID, old.ObjectType, "OBJECT", naviMainStorage, map[string]any{"value": map[string]any{"market_id": 0, "last_market_id": 3, "is_main_market": true}})
	current.Version, current.PreviousTransaction = 3, suiMainnetGenesisDigest
	f.tables[naviMainStorage][0] = current
	f.transactions[current.PreviousTransaction] = []SuiObjectChange{
		{ID: current.ID, InputVersion: old.Version, OutputVersion: current.Version},
		{ID: naviAddress("0x103"), OwnerKind: "SHARED", Created: true, OutputVersion: 3}, // Type enrichment is optional.
	}
	ids, err = r.discoverNaviMarkets(ctx, f)
	if err != nil || len(ids) != 4 || f.transactionReads != 2 || !slices.Contains(ids, naviAddress("0x103")) {
		t.Fatalf("new market: %v %v reads=%d", ids, err, f.transactionReads)
	}
}

func TestNaviDiscoveryRejectsBrokenLineageAndDoesNotCacheFailure(t *testing.T) {
	for _, broken := range []string{"missing version", "counter unchanged", "wrong output version", "missing creation", "duplicate creation"} {
		t.Run(broken, func(t *testing.T) {
			f := latestFixture()
			f.addMarket(0, 1, "0x11", "1", false)
			f.addMarket(1, 1, "0x11", "1", false)
			switch broken {
			case "missing version":
				delete(f.versions, 1)
			case "counter unchanged":
				o := f.tables[naviMainStorage][0]
				o.Version = 1
				f.versions[1] = o
			case "wrong output version":
				f.transactions[suiTestDigest][0].OutputVersion++
			case "missing creation":
				f.transactions[suiTestDigest] = f.transactions[suiTestDigest][:1]
			case "duplicate creation":
				f.transactions[suiTestDigest] = append(f.transactions[suiTestDigest], f.transactions[suiTestDigest][1])
			}
			r := NewSuiProtocolReader()
			if _, err := r.discoverNaviMarkets(context.Background(), f); err == nil || len(r.markets.ids) != 0 {
				t.Fatalf("accepted/cached %s: %v", broken, err)
			}
		})
	}
}

func TestVoloOracleDiscoveredFromPublicationAndCached(t *testing.T) {
	f := latestFixture()
	f.objects[voloVaultPackage] = SuiObject{ID: voloVaultPackage, PreviousTransaction: suiTestDigest}
	oracle := latestObject("0x44", voloVaultPackage+"::vault_oracle::OracleConfig", "SHARED", "", map[string]any{})
	f.objects[oracle.ID] = oracle
	f.transactions[suiTestDigest] = []SuiObjectChange{{ID: oracle.ID, OwnerKind: "SHARED", Created: true}}
	r := NewSuiProtocolReader()
	for range 2 {
		ids, err := r.discoverVoloOracle(context.Background(), f)
		if err != nil || len(ids) != 1 || ids[0] != oracle.ID {
			t.Fatalf("%v %v", ids, err)
		}
	}
	if f.transactionReads != 1 {
		t.Fatal("publication reread after caching")
	}
}

func TestSuiDiscoveryWaitCanBeCanceled(t *testing.T) {
	r := NewSuiProtocolReader()
	r.markets.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := latestFixture()
	f.addMarket(0, 0, "0x11", "1", false)
	if _, err := r.discoverNaviMarkets(ctx, f); err != context.Canceled {
		t.Fatalf("%v", err)
	}
}

func TestNaviDiscoveryCoalescesConcurrentColdScans(t *testing.T) {
	f := latestFixture()
	f.addMarket(0, 1, "0x11", "1", false)
	f.addMarket(1, 1, "0x11", "1", false)
	r := NewSuiProtocolReader()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ids, err := r.discoverNaviMarkets(context.Background(), f)
			if err != nil || len(ids) != 2 {
				t.Errorf("%v %v", ids, err)
			}
		})
	}
	wg.Wait()
	if f.transactionReads != 1 {
		t.Fatalf("duplicate discovery: %d transaction reads", f.transactionReads)
	}
}
