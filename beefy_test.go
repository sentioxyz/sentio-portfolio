package portfolio

import (
	"math/big"
	"testing"
)

func TestBeefyManifestAndRegistration(t *testing.T) {
	adapter := newBeefyAdapter().(*BeefyAdapter)
	if got, want := len(beefyDeployments.Vaults), 377; got != want {
		t.Fatalf("Beefy manifest vaults = %d, want %d", got, want)
	}
	wantByChain := map[ChainID]int{
		Ethereum: 44, BSC: 95, Base: 53, Arbitrum: 40,
		Polygon: 32, Monad: 34, Plasma: 7, Avalanche: 37, Optimism: 35,
	}
	for _, chainID := range SupportedChainIDs {
		if got, want := len(adapter.vaults[chainID]), wantByChain[chainID]; got != want {
			t.Fatalf("Beefy chain %d vaults = %d, want %d", chainID, got, want)
		}
	}
	for _, vault := range beefyDeployments.Vaults {
		if vault.ActivationBlock == 0 {
			t.Fatalf("Beefy vault %q has no activation block", vault.ID)
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

func TestBeefyDeploymentBlockFilterBoundaries(t *testing.T) {
	vaults := []beefyManifestVault{
		{ID: "old", ActivationBlock: 100},
		{ID: "closed", ActivationBlock: 150, DeactivationBlock: 199},
		{ID: "new", ActivationBlock: 200},
	}
	tests := []struct {
		block uint64
		want  []string
	}{
		{block: 99, want: nil},
		{block: 100, want: []string{"old"}},
		{block: 149, want: []string{"old"}},
		{block: 150, want: []string{"old", "closed"}},
		{block: 199, want: []string{"old", "closed"}},
		{block: 200, want: []string{"old", "new"}},
	}
	for _, test := range tests {
		active := activeBeefyVaults(vaults, test.block)
		if len(active) != len(test.want) {
			t.Fatalf("block %d active vaults = %+v, want %v", test.block, active, test.want)
		}
		for index, id := range test.want {
			if active[index].ID != id {
				t.Fatalf("block %d active vault %d = %q, want %q", test.block, index, active[index].ID, id)
			}
		}
	}
}

func TestBeefyAuditedChainMinima(t *testing.T) {
	want := map[ChainID]struct {
		id    string
		block uint64
	}{
		Ethereum:  {id: "aura-aurabal", block: 15_982_782},
		BSC:       {id: "fortube-busd", block: 1_174_856},
		Base:      {id: "moonwell-base-usdbc", block: 2_572_135},
		Arbitrum:  {id: "arbi-bifi-maxi", block: 3_005_534},
		Polygon:   {id: "aave-usdc-eol", block: 14_272_076},
		Monad:     {id: "morpho-monad-steakhouse-usdc", block: 38_165_449},
		Plasma:    {id: "aavev3-plasma-usdt0", block: 2_013_105},
		Avalanche: {id: "avax-bifi-maxi", block: 3_052_900},
		Optimism:  {id: "aavev3-op-dai", block: 17_722_021},
	}
	for chainID, expected := range want {
		var minimum beefyManifestVault
		for _, vault := range beefyDeployments.Vaults {
			if vault.ChainID != chainID ||
				(minimum.ActivationBlock != 0 && minimum.ActivationBlock <= vault.ActivationBlock) {
				continue
			}
			minimum = vault
		}
		if minimum.ID != expected.id || minimum.ActivationBlock != expected.block {
			t.Fatalf(
				"Beefy chain %d minimum = %q at %d, want %q at %d",
				chainID,
				minimum.ID,
				minimum.ActivationBlock,
				expected.id,
				expected.block,
			)
		}
	}
}

func TestBeefyClosedWindowEvidence(t *testing.T) {
	for _, vault := range beefyDeployments.Vaults {
		if vault.DeactivationBlock == 0 {
			continue
		}
		if vault.ChainID != Polygon || vault.ID != "giddy-giddy" ||
			vault.ActivationBlock != 31_487_912 || vault.DeactivationBlock != 86_225_827 {
			t.Fatalf("unexpected Beefy closed window: %+v", vault)
		}
		return
	}
	t.Fatal("Beefy giddy-giddy closed window is missing")
}

func TestEveryBeefyManifestBoundaryUsesActivationBlock(t *testing.T) {
	for _, target := range beefyDeployments.Vaults {
		chainVaults := make([]beefyManifestVault, 0)
		for _, vault := range beefyDeployments.Vaults {
			if vault.ChainID == target.ChainID {
				chainVaults = append(chainVaults, vault)
			}
		}
		for _, active := range activeBeefyVaults(chainVaults, target.ActivationBlock-1) {
			if active.Vault == target.Vault {
				t.Fatalf("Beefy vault %q active before block %d", target.ID, target.ActivationBlock)
			}
		}
		found := false
		for _, active := range activeBeefyVaults(chainVaults, target.ActivationBlock) {
			if active.Vault == target.Vault {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Beefy vault %q inactive at block %d", target.ID, target.ActivationBlock)
		}
		if target.DeactivationBlock == 0 {
			continue
		}
		found = false
		for _, active := range activeBeefyVaults(chainVaults, target.DeactivationBlock) {
			if active.Vault == target.Vault {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Beefy vault %q inactive at final block %d", target.ID, target.DeactivationBlock)
		}
		for _, active := range activeBeefyVaults(chainVaults, target.DeactivationBlock+1) {
			if active.Vault == target.Vault {
				t.Fatalf("Beefy vault %q active after block %d", target.ID, target.DeactivationBlock)
			}
		}
	}
}
