package portfolio

import (
	"math/big"
	"strings"
	"testing"
)

func TestVesperManifest(t *testing.T) {
	if vesperDeployments.SourceCommit != "7334bb44adcce9de3e318e0322218197c932fee0" {
		t.Fatalf("unexpected metadata commit %q", vesperDeployments.SourceCommit)
	}
	if len(vesperDeployments.Pools) != 60 {
		t.Fatalf("Vesper pool count = %d, want 60", len(vesperDeployments.Pools))
	}
	counts := make(map[ChainID]int)
	for _, pool := range vesperDeployments.Pools {
		counts[pool.ChainID]++
	}
	if counts[Ethereum] != 55 || counts[Base] != 5 {
		t.Fatalf("Vesper pool counts = %#v, want Ethereum=55 Base=5", counts)
	}
	if strings.Contains(strings.ToLower(string(vesperManifestJSON)), "rpc.sentio.xyz") {
		t.Fatal("Vesper manifest must not contain an RPC endpoint")
	}
}

func TestVesperUnderlyingAmount(t *testing.T) {
	amount, err := vesperUnderlyingAmount(big.NewInt(3), big.NewInt(10), big.NewInt(4))
	if err != nil {
		t.Fatal(err)
	}
	if amount.Cmp(big.NewInt(7)) != 0 {
		t.Fatalf("underlying amount = %s, want 7", amount)
	}
	if _, err := vesperUnderlyingAmount(big.NewInt(1), big.NewInt(10), new(big.Int)); err == nil {
		t.Fatal("non-zero shares with zero total supply must fail")
	}
}

func TestVesperAdapterCoverage(t *testing.T) {
	info := newVesperAdapter().Info()
	if info.ID != "vesper" || len(info.Chains) != 2 || info.Chains[0] != Ethereum || info.Chains[1] != Base {
		t.Fatalf("unexpected Vesper protocol info: %#v", info)
	}
	if vesperLockedVSPWindow.ActiveAt(vesperLockedVSPWindow.ActivationBlock - 1) {
		t.Fatal("Locked VSP activated one block early")
	}
	if !vesperLockedVSPWindow.ActiveAt(vesperLockedVSPWindow.ActivationBlock) {
		t.Fatal("Locked VSP is not active at its deployment block")
	}
}
