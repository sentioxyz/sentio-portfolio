package portfolio

import "testing"

func TestSuiHistoryRequiresCanonicalVersionPin(t *testing.T) {
	for _, version := range []string{"", "0", "00", "01", "+1", "-1", "18446744073709551616"} {
		if _, err := NewNaviHistoryReader(SentioIndexerConfig{SQLURL: "https://example.invalid/sql", ProcessorVersion: version}); err == nil {
			t.Fatalf("accepted ambiguous processor version %q", version)
		}
	}
}

func TestSuiU8FieldIDMatchesOfficialSDK(t *testing.T) {
	got, err := suiU8FieldID("0x200", 0)
	if err != nil || got != "0x1b6584715a4f23f51a5941be6b253e280988aec0d0397921ff2dcf177ff36194" {
		t.Fatalf("%s %v", got, err)
	}
}
