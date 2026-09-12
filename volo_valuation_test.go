package portfolio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The base-coin quote changes after the last NAV update. Position shares and
// NAV are unchanged, so the settled base-coin claim must stay unchanged too.
func voloValuationFixture() (*latestSuiFixture, SuiAddress) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	receipt, vault, receipts := naviAddress("0x22"), naviAddress("0x33"), naviAddress("0x44")
	oracle, quotes := naviAddress("0x55"), naviAddress("0x66")
	values, times := naviAddress("0x77"), naviAddress("0x88")
	f.objects[receipt] = latestObject(receipt, voloVaultPackage+"::receipt::Receipt", "ADDRESS", owner.Hex(), map[string]any{"vault_id": vault})
	f.objects[vault] = latestObject(vault, voloVaultPackage+"::vault::Vault<0x2::sui::SUI>", "SHARED", "", map[string]any{
		"total_shares": "1000000000", "asset_types": []string{"2::sui::SUI"},
		"receipts": map[string]any{"id": receipts}, "assets_value": map[string]any{"id": values}, "assets_value_updated": map[string]any{"id": times},
	})
	stateID, _ := suiAddressFieldID(receipts, receipt)
	f.objects[stateID] = latestObject(stateID, suiFieldType("address", voloVaultPackage+"::vault_receipt_info::VaultReceiptInfo"), "OBJECT", receipts, map[string]any{
		"name": receipt, "value": map[string]any{"shares": "500000000", "pending_withdraw_shares": "0", "pending_deposit_balance": "0", "claimable_principal": "0"},
	})
	f.objects[oracle] = latestObject(oracle, voloVaultPackage+"::vault_oracle::OracleConfig", "SHARED", "", map[string]any{"aggregators": map[string]any{"id": quotes}})
	f.objects[voloVaultPackage] = SuiObject{PreviousTransaction: suiTestDigest}
	quote := latestObject("0x99", suiFieldType("0x1::ascii::String", voloVaultPackage+"::vault_oracle::PriceInfo"), "OBJECT", quotes, map[string]any{
		"name": "2::sui::SUI", "value": map[string]any{"price": "1000000000000000000", "decimals": "9", "last_updated": "1000000"},
	})
	quote.Version = 7
	quote.PreviousTransaction = suiTestDigest
	f.versions[7] = quote
	newQuote := quote
	newQuote.Version = 9
	newQuote.PreviousTransaction = strings.Repeat("1", 32)
	newQuote.Content = strings.ReplaceAll(strings.ReplaceAll(quote.Content, `"1000000000000000000"`, `"500000000000000000"`), `"1000000"`, `"2000000"`)
	f.tables[quotes] = []SuiObject{newQuote}
	value := latestObject("0xaa", suiFieldType("0x1::ascii::String", "u256"), "OBJECT", values, map[string]any{"name": "2::sui::SUI", "value": "2000000000"})
	stamp := latestObject("0xbb", suiFieldType("0x1::ascii::String", "u64"), "OBJECT", times, map[string]any{"name": "2::sui::SUI", "value": "1000000"})
	value.Version, stamp.Version = 7, 7
	value.PreviousTransaction, stamp.PreviousTransaction = suiTestDigest, suiTestDigest
	f.tables[values], f.tables[times] = []SuiObject{value}, []SuiObject{stamp}
	f.transactions[suiTestDigest] = []SuiObjectChange{
		{ID: oracle, ObjectType: f.objects[oracle].ObjectType, OwnerKind: "SHARED", OutputVersion: 1, Created: true},
		{ID: quote.ID, InputVersion: 6, OutputVersion: 7},
		{ID: stamp.ID, InputVersion: 6, OutputVersion: 7},
	}
	return f, owner
}

func TestVoloLatestUsesSettlementQuote(t *testing.T) {
	f, owner := voloValuationFixture()
	got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
	if err != nil || len(got.Errors) != 0 || len(got.Groups) != 1 {
		t.Fatalf("positions %+v, %v", got, err)
	}
	g := got.Groups[0]
	if g.Components[0].AmountRaw != "1000000000" {
		t.Fatalf("new quote changed the settled claim: %+v", g)
	}
	if g.Metadata["valuation"] != "settled_nav" || g.Metadata["navOldestTimestampMs"] != "1000000" || g.Metadata["valuationPriceTimestampMs"] != "1000000" {
		t.Fatalf("missing settlement provenance: %+v", g.Metadata)
	}
}

func TestVoloLatestDoesNotFallbackFromMissingSettlementQuote(t *testing.T) {
	f, owner := voloValuationFixture()
	delete(f.versions, 7)
	got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
	if err == nil && len(got.Errors) == 0 {
		t.Fatalf("missing settlement quote silently used latest price: %+v", got)
	}
	if len(got.Groups) != 0 {
		t.Fatalf("unverified valuation emitted a claim: %+v", got.Groups)
	}
}

func TestVoloSettlementRejectsInconsistentProvenance(t *testing.T) {
	for _, scenario := range []string{"wrong timestamp", "wrong transaction", "wrong parent", "wrong version", "missing quote change", "missing NAV change"} {
		t.Run(scenario, func(t *testing.T) {
			f, owner := voloValuationFixture()
			quote := f.versions[7]
			switch scenario {
			case "wrong timestamp":
				quote.Content = strings.ReplaceAll(quote.Content, `"1000000"`, `"999999"`)
			case "wrong transaction":
				quote.PreviousTransaction = strings.Repeat("1", 32)
			case "wrong parent":
				quote.Owner = naviAddress("0x123")
			case "wrong version":
				quote.Version = 8
			case "missing quote change":
				f.transactions[suiTestDigest] = append(f.transactions[suiTestDigest][:1], f.transactions[suiTestDigest][2])
			case "missing NAV change":
				f.transactions[suiTestDigest] = f.transactions[suiTestDigest][:2]
			}
			f.versions[7] = quote
			got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
			if err == nil && len(got.Errors) == 0 || len(got.Groups) != 0 {
				t.Fatalf("unverified settlement emitted a claim: %+v, %v", got, err)
			}
		})
	}
}

func TestVoloVaultsWithSameCoinUseTheirOwnSettlementQuotes(t *testing.T) {
	f, owner := voloValuationFixture()
	receipt, vault, receipts := naviAddress("0x122"), naviAddress("0x133"), naviAddress("0x144")
	values, times := naviAddress("0x177"), naviAddress("0x188")
	f.objects[receipt] = latestObject(receipt, voloVaultPackage+"::receipt::Receipt", "ADDRESS", owner.Hex(), map[string]any{"vault_id": vault})
	f.objects[vault] = latestObject(vault, voloVaultPackage+"::vault::Vault<0x2::sui::SUI>", "SHARED", "", map[string]any{
		"total_shares": "1000000000", "asset_types": []string{"2::sui::SUI"},
		"receipts": map[string]any{"id": receipts}, "assets_value": map[string]any{"id": values}, "assets_value_updated": map[string]any{"id": times},
	})
	stateID, _ := suiAddressFieldID(receipts, receipt)
	f.objects[stateID] = latestObject(stateID, suiFieldType("address", voloVaultPackage+"::vault_receipt_info::VaultReceiptInfo"), "OBJECT", receipts, map[string]any{
		"name": receipt, "value": map[string]any{"shares": "500000000", "pending_withdraw_shares": "0", "pending_deposit_balance": "0", "claimable_principal": "0"},
	})
	f.tables[values] = []SuiObject{latestObject("0x1aa", suiFieldType("0x1::ascii::String", "u256"), "OBJECT", values, map[string]any{"name": "2::sui::SUI", "value": "2000000000"})}
	stamp := latestObject("0x1bb", suiFieldType("0x1::ascii::String", "u64"), "OBJECT", times, map[string]any{"name": "2::sui::SUI", "value": "2000000"})
	stamp.Version, stamp.PreviousTransaction = 9, strings.Repeat("1", 32)
	f.tables[times] = []SuiObject{stamp}
	got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
	if err != nil || len(got.Errors) != 0 || len(got.Groups) != 2 {
		t.Fatalf("positions %+v, %v", got, err)
	}
	want := map[string]string{naviAddress("0x33"): "1000000000", vault: "2000000000"}
	for _, group := range got.Groups {
		if group.Components[0].AmountRaw != want[group.MarketID] {
			t.Fatalf("quote from another vault used: %+v", group)
		}
	}
}

func TestVoloZeroAssetDoesNotRequireAnUnrelatedOldQuote(t *testing.T) {
	f, owner := voloValuationFixture()
	vault := f.objects[naviAddress("0x33")]
	fields, _ := suiObjectFields(vault.Content)
	fields["asset_types"] = []string{"2::sui::SUI", "2::other::OTHER"}
	raw, _ := json.Marshal(fields)
	vault.Content = string(raw)
	f.objects[vault.ID] = vault
	values, times := naviAddress("0x77"), naviAddress("0x88")
	f.tables[values] = append(f.tables[values], latestObject("0xcc", suiFieldType("0x1::ascii::String", "u256"), "OBJECT", values, map[string]any{"name": "2::other::OTHER", "value": "0"}))
	f.tables[times] = append(f.tables[times], latestObject("0xdd", suiFieldType("0x1::ascii::String", "u64"), "OBJECT", times, map[string]any{"name": "2::other::OTHER", "value": "1"}))
	got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
	if err != nil || len(got.Errors) != 0 || len(got.Groups) != 1 || got.Groups[0].Components[0].AmountRaw != "1000000000" {
		t.Fatalf("zero asset changed valuation: %+v, %v", got, err)
	}
	// A nonzero value from that other period must not be combined into this NAV.
	other := &f.tables[values][1]
	other.Content = strings.ReplaceAll(other.Content, `"value":"0"`, `"value":"1"`)
	got, err = NewSuiProtocolReader().ReadLatest(context.Background(), "volo-vaults", owner, f)
	if err == nil && len(got.Errors) == 0 || len(got.Groups) != 0 {
		t.Fatalf("mixed NAV periods accepted: %+v, %v", got, err)
	}
}
