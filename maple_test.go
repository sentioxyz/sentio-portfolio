package portfolio

import "testing"

func TestEngineRegistersCompleteMapleManifest(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		if protocol.ID != "maple" {
			continue
		}
		adapter := newMapleAdapter().(*MapleAdapter)
		if got := len(adapter.vaults[Ethereum]); got != 21 {
			t.Fatalf("Maple vault count = %d, want 21", got)
		}
		if got := len(adapter.queues); got != 11 {
			t.Fatalf("Maple queue count = %d, want 11", got)
		}
		return
	}
	t.Fatal("maple is not registered")
}
