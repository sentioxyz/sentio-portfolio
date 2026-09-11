package portfolio

import "testing"

func naviTopologyFixture() []suiHistoryObject {
	return []suiHistoryObject{
		historyObjectFixture("0x10", "storage", "", "", "", "", map[string]any{"reserves": map[string]any{"id": naviAddress("0x11")}}),
		historyObjectFixture("0x12", "reserve", "", naviAddress("0x11"), "201", "", map[string]any{"name": 201, "value": map[string]any{
			"id": 201, "coin_type": "2::sui::SUI",
			"supply_balance": map[string]any{"user_state": map[string]any{"id": naviAddress("0x13")}},
			"borrow_balance": map[string]any{"user_state": map[string]any{"id": naviAddress("0x14")}},
		}}),
	}
}

func naviMarketFixture(storage, market string, main bool) suiHistoryObject {
	return historyObjectFixture("0x20", "market", "", naviAddress(storage), market, "", map[string]any{"value": map[string]any{"market_id": market, "is_main_market": main}})
}

func TestNaviTopologyDiscoversUnlistedMarketAndReserve(t *testing.T) {
	rows := append(naviTopologyFixture(), naviMarketFixture("0x10", "987654321", false))
	tables, err := naviBalanceTables(rows)
	if err != nil || len(tables) != 2 {
		t.Fatalf("discovery: %v %v", tables, err)
	}
	for parent, side := range map[string]string{"0x13": "supply", "0x14": "borrow"} {
		got := tables[naviAddress(parent)]
		if got.Side != side || got.Reserve.Market != "987654321" || got.Reserve.Asset != "201" || got.Reserve.CoinType != suiLongType {
			t.Fatalf("wrong on-chain relationship: %+v %+v", got, got.Reserve)
		}
	}
}

func TestNaviTopologyLegacyMarketBoundary(t *testing.T) {
	// Before MarketInfo existed the sole Storage represented market 0.
	legacy := naviTopologyFixture()
	before, err := naviBalanceTables(legacy)
	if err != nil || before[naviAddress("0x13")].Reserve.Market != "0" {
		t.Fatalf("legacy: %v %v", before, err)
	}
	after, err := naviBalanceTables(append(legacy, naviMarketFixture("0x10", "0", true)))
	if err != nil || after[naviAddress("0x13")].Reserve.Market != "0" {
		t.Fatalf("explicit main market: %v %v", after, err)
	}
	newStorage := historyObjectFixture("0x30", "storage", "", "", "", "", map[string]any{"reserves": map[string]any{"id": naviAddress("0x31")}})
	if _, err := naviBalanceTables(append(naviTopologyFixture(), newStorage)); err == nil {
		t.Fatal("multiple storages without metadata were defaulted to market 0")
	}
	partial := append(naviTopologyFixture(), naviMarketFixture("0x10", "0", true), newStorage)
	if _, err := naviBalanceTables(partial); err == nil {
		t.Fatal("new Storage without market metadata was silently omitted")
	}
	// Once its metadata exists, an empty new market is discovered without a list change.
	market := naviMarketFixture("0x30", "999", false)
	market.ID = naviAddress("0x32")
	if _, err := naviBalanceTables(append(partial, market)); err != nil {
		t.Fatal(err)
	}
}

func TestNaviTopologyRejectsAmbiguousRelationships(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func([]suiHistoryObject) []suiHistoryObject
	}{
		{"orphan reserve", func(r []suiHistoryObject) []suiHistoryObject { return r[1:] }},
		{"orphan market", func(r []suiHistoryObject) []suiHistoryObject { return append(r, naviMarketFixture("0x99", "0", true)) }},
		{"incorrect main flag", func(r []suiHistoryObject) []suiHistoryObject { return append(r, naviMarketFixture("0x10", "99", true)) }},
		{"reserve identity", func(r []suiHistoryObject) []suiHistoryObject { r[1].Key = "0"; return r }},
		{"duplicate reserve", func(r []suiHistoryObject) []suiHistoryObject {
			duplicate := r[1]
			duplicate.ID = naviAddress("0x99")
			return append(r, duplicate)
		}},
		{"shared principal table", func(r []suiHistoryObject) []suiHistoryObject {
			r[1].Content = `{"name":201,"value":{"id":201,"coin_type":"2::sui::SUI","supply_balance":{"user_state":{"id":"0x13"}},"borrow_balance":{"user_state":{"id":"0x13"}}}}`
			return r
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := naviBalanceTables(test.edit(naviTopologyFixture())); err == nil {
				t.Fatal("ambiguous market topology accepted")
			}
		})
	}
}
