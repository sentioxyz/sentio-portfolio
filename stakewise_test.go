package portfolio

import "testing"

func TestStakeWiseManifestAndRegistration(t *testing.T) {
	adapter := newStakeWiseAdapter().(*StakeWiseAdapter)
	if got, want := len(adapter.vaults), 54; got != want {
		t.Fatalf("StakeWise verified vaults = %d, want %d", got, want)
	}
	if got := adapter.Info(); got.ID != "stakewise" || len(got.Chains) != 1 || got.Chains[0] != Ethereum {
		t.Fatalf("StakeWise protocol info = %+v", got)
	}
	for _, vault := range adapter.vaults {
		if vault.ActivationBlock == 0 {
			t.Fatalf("StakeWise vault %s has no activation block", vault.Address)
		}
	}
}
