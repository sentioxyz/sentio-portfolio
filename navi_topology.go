package portfolio

import (
	"fmt"
	"strconv"
)

type naviReserve struct {
	Market, Asset, CoinType string
	Fields                  suiFields
}

type naviBalanceTable struct {
	Reserve *naviReserve
	Side    string
}

// Relationships come from objects at the requested checkpoint, not a manifest
// of today's markets. The same object types cover subsequently created markets.
func naviBalanceTables(rows []suiHistoryObject) (map[string]naviBalanceTable, error) {
	storages := make(map[string]suiFields)
	markets := make(map[string]string)
	marketIDs := make(map[string]bool)
	for _, row := range rows {
		switch row.Kind {
		case "storage":
			fields, err := suiObjectFields(row.Content)
			if err != nil {
				return nil, err
			}
			storages[row.ID] = fields
		case "market":
			fields, err := suiObjectFields(row.Content)
			if err != nil {
				return nil, err
			}
			info, err := fields.object("value")
			if err != nil {
				return nil, err
			}
			id, err := info.uint("market_id")
			if err != nil || !id.IsUint64() || row.Key != id.String() {
				return nil, fmt.Errorf("invalid NAVI market identity")
			}
			main, ok := info["is_main_market"].(bool)
			if !ok || main != (id.Sign() == 0) || markets[row.Parent] != "" || marketIDs[id.String()] {
				return nil, fmt.Errorf("ambiguous NAVI market identity")
			}
			markets[row.Parent] = id.String()
			marketIDs[id.String()] = true
		case "reserve":
		default:
			return nil, fmt.Errorf("unexpected NAVI topology object")
		}
	}
	// Before MarketInfo was introduced, NAVI had one Storage and market 0.
	// Once market metadata exists, every Storage must have it; never default a
	// newly created or incompletely indexed market to the original market.
	if len(markets) == 0 && len(storages) == 1 {
		for id := range storages {
			markets[id] = "0"
		}
	}
	for storage := range markets {
		if storages[storage] == nil {
			return nil, fmt.Errorf("NAVI market's Storage is not indexed")
		}
	}
	parents := make(map[string]string)
	for id, fields := range storages {
		market, ok := markets[id]
		if !ok {
			return nil, fmt.Errorf("NAVI Storage has no market identity at this checkpoint")
		}
		table, err := fields.object("reserves")
		if err != nil {
			return nil, err
		}
		parent, err := table.address("id")
		if err != nil {
			return nil, err
		}
		if _, ok := parents[parent]; ok {
			return nil, fmt.Errorf("NAVI markets share a reserve table")
		}
		parents[parent] = market
	}
	tables := make(map[string]naviBalanceTable)
	seen := make(map[string]bool)
	for _, row := range rows {
		if row.Kind != "reserve" {
			continue
		}
		market, ok := parents[row.Parent]
		if !ok {
			return nil, fmt.Errorf("NAVI reserve's Storage is not indexed")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		reserve, err := fields.object("value")
		if err != nil {
			return nil, err
		}
		asset, err := reserve.uint("id")
		if err != nil || !asset.IsUint64() || asset.Uint64() > 255 {
			return nil, fmt.Errorf("invalid NAVI asset ID")
		}
		name, err := fields.uint("name")
		if err != nil || name.Cmp(asset) != 0 || row.Key != asset.String() {
			return nil, fmt.Errorf("invalid NAVI reserve identity")
		}
		key := market + ":" + asset.String()
		if seen[key] {
			return nil, fmt.Errorf("duplicate NAVI reserve")
		}
		seen[key] = true
		raw, err := reserve.text("coin_type")
		if err != nil {
			return nil, err
		}
		coin, err := suiCoinType(raw)
		if err != nil {
			return nil, err
		}
		ref := &naviReserve{Market: market, Asset: strconv.FormatUint(asset.Uint64(), 10), CoinType: coin, Fields: reserve}
		for _, side := range []string{"supply", "borrow"} {
			balance, err := reserve.object(side + "_balance")
			if err != nil {
				return nil, err
			}
			table, err := balance.object("user_state")
			if err != nil {
				return nil, err
			}
			parent, err := table.address("id")
			if err != nil {
				return nil, err
			}
			if _, ok := tables[parent]; ok {
				return nil, fmt.Errorf("NAVI reserves share a principal table")
			}
			tables[parent] = naviBalanceTable{Reserve: ref, Side: side}
		}
	}
	return tables, nil
}
