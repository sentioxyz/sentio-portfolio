package portfolio

import (
	"strings"
	"testing"
)

func TestASCIIFieldIDMatchesOfficialSDK(t *testing.T) {
	for key, want := range map[string]string{"2::sui::SUI": "0x71895decf62064a4bb597606fedc37eae17f5c377fedc45469ee725f01869c67", strings.Repeat("a", 130): "0x5e0e39a512c606f6af63ddb5417ddfbbd7de65980ab189b4ebc6d8370b06f97c"} {
		got, err := suiASCIIFieldID("0x33", key)
		if err != nil || got != want {
			t.Fatalf("field ID %s: %s %v", key, got, err)
		}
	}
}
