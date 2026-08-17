package portfolio

import "testing"

func TestEngineRegistersAaveV4(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		if protocol.ID != "aave-v4" {
			continue
		}
		if len(protocol.Chains) != 1 || protocol.Chains[0] != Ethereum {
			t.Fatalf("aave-v4 chains = %v, want [Ethereum]", protocol.Chains)
		}
		return
	}
	t.Fatal("aave-v4 is not registered")
}
