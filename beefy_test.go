package portfolio

import (
	"math/big"
	"testing"
)

func TestBeefyManifestAndRegistration(t *testing.T) {
	adapter := newBeefyAdapter().(*BeefyAdapter)
	if got, want := len(beefyDeployments.Vaults), 232; got != want {
		t.Fatalf("Beefy manifest vaults = %d, want %d", got, want)
	}
	for _, chainID := range SupportedChainIDs {
		if len(adapter.vaults[chainID]) == 0 {
			t.Fatalf("Beefy has no vaults on chain %d", chainID)
		}
	}
}

func TestBeefyUnderlyingAmountRoundsDown(t *testing.T) {
	shares, _ := new(big.Int).SetString("56814288341590378346", 10)
	price, _ := new(big.Int).SetString("1325242620973423608", 10)
	if got, want := beefyUnderlyingAmount(shares, price).String(), "75292716390549057509"; got != want {
		t.Fatalf("underlying = %s, want %s", got, want)
	}
}

func TestBeefyHistoricalTimestampFilter(t *testing.T) {
	vaults := []beefyManifestVault{{ID: "old", CreatedAt: 100}, {ID: "new", CreatedAt: 200}}
	active := activeBeefyVaults(vaults, 199)
	if len(active) != 1 || active[0].ID != "old" {
		t.Fatalf("active vaults = %+v, want old only", active)
	}
}
