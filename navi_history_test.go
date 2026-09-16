package portfolio

import "testing"

func TestSuiU8FieldIDMatchesOfficialSDK(t *testing.T) {
	got, err := suiU8FieldID("0x200", 0)
	if err != nil || got != "0x1b6584715a4f23f51a5941be6b253e280988aec0d0397921ff2dcf177ff36194" {
		t.Fatalf("%s %v", got, err)
	}
}
