package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestVoloHistoricalNAVPreservesOpaqueKeys(t *testing.T) {
	const position = "3::pool::CredentialV2<0x4::lp::LP,0x4::lp::LP>"
	for _, keys := range [][]string{
		{position + "0", position + "1"},
		{position + "0", "0x" + position + "0"},
	} {
		t.Run(keys[1], func(t *testing.T) {
			f, owner := voloValuationFixture()
			vault := f.objects[naviAddress("0x33")]
			fields, err := suiObjectFields(vault.Content)
			if err != nil {
				t.Fatal(err)
			}
			fields["asset_types"] = keys
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			vault.Content = string(raw)
			f.objects[vault.ID] = vault
			for kind, parent := range map[string]string{"navValue": naviAddress("0x77"), "navTimestamp": naviAddress("0x88")} {
				original := f.tables[parent][0]
				f.tables[parent] = nil
				for i, key := range keys {
					value := "1000000"
					if kind == "navValue" {
						value = fmt.Sprint((i + 1) * 1_000_000_000)
					}
					row := original
					raw, err := json.Marshal(map[string]string{"name": key, "value": value})
					if err != nil {
						t.Fatal(err)
					}
					row.Content = string(raw)
					f.tables[parent] = append(f.tables[parent], row)
				}
			}
			fixture, reader := newSQLPortfolioFixture(t, "volo-vaults", f)
			got, err := reader.ReadAtCheckpoint(context.Background(), owner, fixture.pin.Sequence+1)
			if err != nil || len(got.Groups) != 1 || fixture.calls != 1 {
				t.Fatalf("opaque NAV keys rejected: %+v, %v, SQL calls %d", got, err, fixture.calls)
			}
			group := got.Groups[0]
			// Half the shares claim half of the two distinct stored NAV values.
			if len(group.Components) != 1 || group.Components[0].AmountRaw != "1500000000" ||
				group.Components[0].Coin.CoinType != suiLongType || group.Metadata["valuation"] != "settled_nav" ||
				group.Metadata["navOldestTimestampMs"] != "1000000" || group.Metadata["valuationPriceTimestampMs"] != "1000000" {
				t.Fatalf("opaque keys changed settlement accounting: %+v", group)
			}
		})
	}
}

func TestVoloOpaqueNAVKeysDoNotRelaxCoinValidation(t *testing.T) {
	const position = "3::pool::CredentialV2<0x4::lp::LP,0x4::lp::LP>0"
	for _, target := range []string{"vault coin", "oracle quote"} {
		t.Run(target, func(t *testing.T) {
			f, owner := voloValuationFixture()
			if target == "vault coin" {
				vault := f.objects[naviAddress("0x33")]
				vault.ObjectType = voloVaultPackage + "::vault::Vault<0x" + position + ">"
				f.objects[vault.ID] = vault
			} else {
				quote := &f.tables[naviAddress("0x66")][0]
				fields, err := suiObjectFields(quote.Content)
				if err != nil {
					t.Fatal(err)
				}
				fields["name"] = position
				raw, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				quote.Content = string(raw)
			}
			got, err := readVoloFixture(t, f, owner)
			if err == nil || len(got.Groups) != 0 {
				t.Fatalf("invalid %s emitted positions: %+v, %v", target, got, err)
			}
		})
	}
}
