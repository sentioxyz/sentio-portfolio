package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"
	"strings"
	"testing"
)

func suilendTestDecimal(value string) map[string]any { return map[string]any{"value": value} }
func suilendTestWad(value string) string {
	return new(big.Int).Mul(naviInt(value), suilendWad).String()
}

func suilendFixture() (*latestSuiFixture, SuiAddress) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x1")
	typ := suilendPackage + "::suilend::MAIN_POOL"
	f.objects[naviAddress("0x10")] = latestObject("0x10", suilendCapType+"<"+typ+">", "ADDRESS", owner.Hex(), map[string]any{"obligation_id": naviAddress("0x20")})
	f.objects[naviAddress("0x20")] = latestObject("0x20", suilendPackage+"::obligation::Obligation<"+typ+">", "OBJECT", "0x40", map[string]any{
		"lending_market_id": naviAddress("0x30"),
		"deposits":          []any{map[string]any{"coin_type": "2::sui::SUI", "reserve_array_index": "0", "deposited_ctoken_amount": "100000000000"}},
		"borrows":           []any{map[string]any{"coin_type": "2::sui::SUI", "reserve_array_index": "0", "borrowed_amount": suilendTestDecimal(suilendTestWad("50000000000")), "cumulative_borrow_rate": suilendTestDecimal(suilendTestWad("2"))}},
	})
	parent, _ := suilendObligationParent(naviAddress("0x50"), naviAddress("0x20"))
	obligation := f.objects[naviAddress("0x20")]
	obligation.Owner = parent
	f.objects[obligation.ID] = obligation
	f.objects[naviAddress("0x30")] = latestObject("0x30", suilendPackage+"::lending_market::LendingMarket<"+typ+">", "SHARED", "", map[string]any{
		"obligations": map[string]any{"id": naviAddress("0x50"), "size": "1"},
		"reserves": []any{map[string]any{
			"id": naviAddress("0x60"), "lending_market_id": naviAddress("0x30"), "array_index": "0", "coin_type": "2::sui::SUI", "mint_decimals": 9,
			"available_amount": "800000000000", "borrowed_amount": suilendTestDecimal(suilendTestWad("400000000000")), "ctoken_supply": "1000000000000",
			"unclaimed_spread_fees": suilendTestDecimal(suilendTestWad("100000000000")), "cumulative_borrow_rate": suilendTestDecimal(suilendTestWad("3")), "interest_last_update_timestamp_s": "1000",
			"config": map[string]any{"element": map[string]any{"interest_rate_utils": "AGQ=", "interest_rate_aprs": []any{"0", "0"}, "spread_fee_bps": "1000"}},
		}},
	})
	return f, owner
}

func editSuilendObject(f *latestSuiFixture, id string, edit func(suiFields)) {
	o := f.objects[naviAddress(id)]
	fields, err := suiObjectFields(o.Content)
	if err != nil {
		panic(err)
	}
	edit(fields)
	raw, _ := json.Marshal(fields)
	o.Content = string(raw)
	f.objects[o.ID] = o
}

func editSuilendReserve(f *latestSuiFixture, edit func(suiFields)) {
	editSuilendObject(f, "0x30", func(fields suiFields) { rs, _ := suilendStructVector(fields, "reserves"); edit(rs[0]) })
}

func TestSuilendLatestSupplyDebtAndOwnership(t *testing.T) {
	f, owner := suilendFixture()
	// Multiple caps can legally authorize one obligation; never duplicate its debt.
	cap := f.objects[naviAddress("0x10")]
	f.objects[naviAddress("0x11")] = latestObject("0x11", cap.ObjectType, "ADDRESS", owner.Hex(), map[string]any{"obligation_id": naviAddress("0x20")})
	reader := NewSuiProtocolReader()
	if !slices.Contains(reader.ProtocolIDs(), "suilend") {
		t.Fatal("Suilend is not registered")
	}
	got, err := readSuilendFixture(t, f, owner)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) != 1 {
		t.Fatalf("read: %+v %v", got, err)
	}
	g := got.Groups[0]
	if got.ProtocolName != "Suilend" || g.MarketID != naviAddress("0x30") || len(g.Components) != 2 || len(g.Metadata["ownerCapabilities"].([]string)) != 2 {
		t.Fatalf("identity %+v", g)
	}
	if g.Components[0].Kind != "asset" || g.Components[0].AmountRaw != "110000000000" || g.Components[1].Kind != "debt" || g.Components[1].AmountRaw != "75000000000" {
		t.Fatalf("amounts %+v", g.Components)
	}
	if g.Metadata["stateMode"] != "indexed" || g.Metadata["materializedAtCheckpoint"] == nil {
		t.Fatalf("window %+v", g.Metadata)
	}
	// Moving both capabilities removes this owner's position, regardless of who
	// originally deposited. The current cap owner alone controls the obligation.
	for _, id := range []string{"0x10", "0x11"} {
		o := f.objects[naviAddress(id)]
		o.Owner = naviAddress("0x99")
		f.objects[o.ID] = o
	}
	got, err = readSuilendFixture(t, f, owner)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) > 0 {
		t.Fatalf("transferred %+v %v", got, err)
	}
	next, _ := ParseSuiAddress("0x99")
	got, err = readSuilendFixture(t, f, next)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) != 1 {
		t.Fatalf("new owner %+v %v", got, err)
	}
}

func TestSuilendEmptyObligationsAndWallet(t *testing.T) {
	f, owner := suilendFixture()
	editSuilendObject(f, "0x20", func(o suiFields) { o["deposits"] = []any{}; o["borrows"] = []any{} })
	f.metadata = nil
	got, err := readSuilendFixture(t, f, owner)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) > 0 {
		t.Fatalf("empty %+v %v", got, err)
	}
	// A valid market inventory certifies empty ownership without coin metadata.
	delete(f.objects, naviAddress("0x10"))
	delete(f.objects, naviAddress("0x20"))
	f.failObjects = true
	got, err = readSuilendFixture(t, f, owner)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) > 0 {
		t.Fatalf("empty wallet %+v %v", got, err)
	}
}

func TestSuilendRejectsIncompleteOrInconsistentState(t *testing.T) {
	tests := map[string]func(*latestSuiFixture){
		"missing obligation": func(f *latestSuiFixture) { delete(f.objects, naviAddress("0x20")) },
		"missing market":     func(f *latestSuiFixture) { delete(f.objects, naviAddress("0x30")) },
		"wrong derived parent": func(f *latestSuiFixture) {
			o := f.objects[naviAddress("0x20")]
			o.Owner = naviAddress("0x99")
			f.objects[o.ID] = o
		},
		"wrong table": func(f *latestSuiFixture) {
			editSuilendObject(f, "0x30", func(o suiFields) { o["obligations"] = map[string]any{"id": naviAddress("0x99"), "size": "1"} })
		},
		"wrong obligation type": func(f *latestSuiFixture) {
			o := f.objects[naviAddress("0x20")]
			o.ObjectType = suiType("0xbeef::obligation::Obligation<0xbeef::pool::POOL>")
			f.objects[o.ID] = o
		},
		"wrong capability id": func(f *latestSuiFixture) {
			editSuilendObject(f, "0x10", func(o suiFields) { o["id"] = naviAddress("0x99") })
		},
		"wrong reserve market": func(f *latestSuiFixture) {
			editSuilendReserve(f, func(o suiFields) { o["lending_market_id"] = naviAddress("0x99") })
		},
		"wrong reserve index": func(f *latestSuiFixture) { editSuilendReserve(f, func(o suiFields) { o["array_index"] = "1" }) },
		"wrong coin": func(f *latestSuiFixture) {
			editSuilendReserve(f, func(o suiFields) { o["coin_type"] = "3::fake::COIN" })
		},
		"wrong decimals": func(f *latestSuiFixture) { editSuilendReserve(f, func(o suiFields) { o["mint_decimals"] = 6 }) },
		"zero supply":    func(f *latestSuiFixture) { editSuilendReserve(f, func(o suiFields) { o["ctoken_supply"] = "0" }) },
		"invalid curve": func(f *latestSuiFixture) {
			editSuilendReserve(f, func(o suiFields) {
				o["config"] = map[string]any{"element": map[string]any{"interest_rate_utils": "ZAA=", "interest_rate_aprs": []any{"0", "0"}, "spread_fee_bps": "0"}}
			})
		},
		"missing metadata": func(f *latestSuiFixture) { f.metadata = nil },
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			f, owner := suilendFixture()
			edit(f)
			got, err := readSuilendFixture(t, f, owner)
			if err == nil && len(got.Errors) == 0 {
				t.Fatal("accepted invalid state")
			}
			if len(got.Groups) > 0 {
				t.Fatalf("fabricated complete positions %+v", got.Groups)
			}
		})
	}
}

func TestSuilendMoveInterestAndRounding(t *testing.T) {
	f, owner := suilendFixture()
	editSuilendReserve(f, func(o suiFields) {
		o["config"] = map[string]any{"element": map[string]any{"interest_rate_utils": []any{0, 100}, "interest_rate_aprs": []any{"315360000000", "315360000000"}, "spread_fee_bps": "1000"}}
	})
	state, err := loadSuilend(context.Background(), owner, f)
	if err != nil {
		t.Fatal(err)
	}
	r := state.markets[naviAddress("0x30")][0]
	compounded, err := r.at(1003)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly 100% per second for three seconds: factor=8, new debt=2800,
	// protocol spread=280, net supply=800+3200-380=3620 SUI.
	deposit, err := compounded.depositAmount(big.NewInt(100000000000))
	if err != nil || deposit.String() != "362000000000" {
		t.Fatalf("deposit %v %v", deposit, err)
	}
	debt, err := compounded.debtAmount(naviInt(suilendTestWad("50000000000")), naviInt(suilendTestWad("2")))
	if err != nil || debt.String() != "600000000000" {
		t.Fatalf("debt %v %v", debt, err)
	}
	if r.borrowed.String() != suilendTestWad("400000000000") || r.updated != 1000 {
		t.Fatal("compounding mutated source reserve")
	}
	pow, err := suilendPow(new(big.Int).Add(suilendWad, big.NewInt(1)), 3)
	if err != nil || pow.Cmp(new(big.Int).Add(suilendWad, big.NewInt(3))) != 0 {
		t.Fatalf("WAD truncation %v %v", pow, err)
	}
	if _, err := r.debtAmount(big.NewInt(1), big.NewInt(0)); err == nil {
		t.Fatal("zero debt index accepted")
	}
	if _, err := r.debtAmount(big.NewInt(1), naviInt(suilendTestWad("4"))); err == nil {
		t.Fatal("backwards debt index accepted")
	}
	if _, err := r.at(999); err == nil {
		t.Fatal("backwards time accepted")
	}
}

func TestSuilendDiscoversIsolatedMarketAndKeepsObligationsSeparate(t *testing.T) {
	f, owner := suilendFixture()
	typ := "0xbeef::isolated::POOL"
	// A second market and obligation are reached only through the current cap,
	// not a hardcoded registry list. Empty main-market obligations stay excluded.
	original := map[string]SuiObject{}
	for k, v := range f.objects {
		original[k] = v
	}
	replacements := map[string]string{naviAddress("0x10"): naviAddress("0x110"), naviAddress("0x20"): naviAddress("0x120"), naviAddress("0x30"): naviAddress("0x130"), naviAddress("0x40"): naviAddress("0x140"), naviAddress("0x50"): naviAddress("0x150"), naviAddress("0x60"): naviAddress("0x160")}
	for _, o := range original {
		raw, _ := json.Marshal(o)
		s := string(raw)
		for a, b := range replacements {
			s = strings.ReplaceAll(s, a, b)
		}
		s = strings.ReplaceAll(s, suilendPackage+"::suilend::MAIN_POOL", suiType(typ))
		var copy SuiObject
		if err := json.Unmarshal([]byte(s), &copy); err != nil {
			t.Fatal(err)
		}
		f.objects[copy.ID] = copy
	}
	child := f.objects[naviAddress("0x120")]
	child.Owner, _ = suilendObligationParent(naviAddress("0x150"), child.ID)
	f.objects[child.ID] = child
	got, err := readSuilendFixture(t, f, owner)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) != 2 {
		t.Fatalf("isolated markets %+v %v", got, err)
	}
	if got.Groups[0].MarketID == got.Groups[1].MarketID || got.Groups[0].ID == got.Groups[1].ID {
		t.Fatal("distinct market obligations merged")
	}
}

func TestSuilendInterestCurveInterpolatesInWad(t *testing.T) {
	f, owner := suilendFixture()
	editSuilendReserve(f, func(o suiFields) {
		o["available_amount"] = "600000000000"
		o["borrowed_amount"] = suilendTestDecimal(suilendTestWad("500000000000"))
		o["config"] = map[string]any{"element": map[string]any{"interest_rate_utils": []any{0, 100}, "interest_rate_aprs": []any{"0", "20000"}, "spread_fee_bps": "1000"}}
	})
	state, err := loadSuilend(context.Background(), owner, f)
	if err != nil {
		t.Fatal(err)
	}
	r := state.markets[naviAddress("0x30")][0]
	// 50% utilization on the 0%-200% curve gives 100% APR. The contract
	// floors 1e18/31536000 to 31709791983 before multiplying the index.
	next, err := r.at(1001)
	if err != nil || next.borrowIndex.String() != "3000000095129375949" {
		t.Fatalf("interpolated index %v %v", next.borrowIndex, err)
	}
}
