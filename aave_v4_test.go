package portfolio

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestEngineRegistersAaveV4(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		if protocol.ID != "aave-v4" {
			continue
		}
		want := []ChainID{Ethereum, Avalanche}
		if len(protocol.Chains) != len(want) ||
			protocol.Chains[0] != want[0] || protocol.Chains[1] != want[1] {
			t.Fatalf("aave-v4 chains = %v, want %v", protocol.Chains, want)
		}
		return
	}
	t.Fatal("aave-v4 is not registered")
}

func TestAaveV4ReserveTokenUsesScannedChain(t *testing.T) {
	underlying := common.HexToAddress("0x0000000000000000000000000000000000001234")
	token := aaveV4ReserveToken(Avalanche, aaveV4ReserveData{
		Underlying: underlying,
		Decimals:   6,
	})
	if token.ChainID != Avalanche || token.Address != underlying || token.Decimals != 6 {
		t.Fatalf("Avalanche reserve token = %+v", token)
	}
}

func TestAaveV4HubDeploymentWindows(t *testing.T) {
	// Hub 0x62d631… deploys at 25,318,132; a fixed-block scan before that must not
	// eth_call it (no code yet: empty return data would fail the strict batch and drop
	// the whole Aave v4 surface for blocks 24,720,899–25,318,131).
	cases := []struct {
		block uint64
		want  int
	}{
		{24_720_886, 0},
		{24_720_887, 1},
		{24_720_899, 3},
		{25_318_131, 3},
		{25_318_132, 4},
	}
	for _, testCase := range cases {
		if got := len(activeAaveV4Hubs(aaveV4EthereumHubs, testCase.block)); got != testCase.want {
			t.Fatalf("active hubs at block %d = %d, want %d", testCase.block, got, testCase.want)
		}
	}
	late := activeAaveV4Hubs(aaveV4EthereumHubs, 25_318_131)
	for _, hub := range late {
		if hub == common.HexToAddress("0x62d63197660c080236193CA60b70E49A08E90368") {
			t.Fatal("the late hub is active before its deployment block")
		}
	}
}
