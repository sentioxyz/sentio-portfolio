package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

func naviLending(ctx context.Context, owner SuiAddress, pin SuiCheckpoint, reader SuiReader, state suiProtocolState) ([]SuiProtocolGroup, error) {
	accounts := map[string]bool{owner.Hex(): true}
	for _, object := range state.Owned {
		if object.Kind != "account" {
			continue
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return nil, err
		}
		account, err := fields.address("owner")
		if err != nil {
			return nil, err
		}
		accounts[account] = true
	}
	tables, err := naviBalanceTables(state.Topology)
	if err != nil {
		return nil, err
	}
	principals := state.Principals
	type position struct {
		account   string
		table     naviBalanceTable
		principal *big.Int
	}
	positions := []position{}
	seen := make(map[string]bool)
	tokens := make(map[string]bool)
	for _, row := range principals {
		table, ok := tables[row.Parent]
		if !ok || row.Kind != "principal" || !accounts[row.Owner] {
			return nil, fmt.Errorf("unexpected lending principal attribution")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		account, err := fields.address("name")
		if err != nil || account != row.Owner || row.Key != account {
			return nil, fmt.Errorf("invalid lending principal identity")
		}
		identity := row.Parent + ":" + account
		if seen[identity] {
			return nil, fmt.Errorf("duplicate lending principal")
		}
		seen[identity] = true
		amount, err := fields.uint("value")
		if err != nil {
			return nil, err
		}
		if amount.Sign() == 0 {
			continue
		}
		positions = append(positions, position{account, table, amount})
		tokens[table.Reserve.CoinType] = true
	}
	if len(positions) == 0 {
		return nil, nil
	}
	metadata, err := suiProtocolMetadata(ctx, reader, tokens)
	if err != nil {
		return nil, err
	}
	emodeRows := state.Emodes
	emodes := map[string]bool{}
	for _, row := range emodeRows {
		if row.Kind != "emode" || !accounts[row.Account] || row.ID != "emode:"+row.Market+":"+row.Account {
			return nil, fmt.Errorf("invalid NAVI e-mode identity")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		entered, ok := fields["entered"].(bool)
		if !ok {
			return nil, fmt.Errorf("invalid NAVI e-mode state")
		}
		emodes[row.Market+":"+row.Account] = entered
	}
	groups := map[string]*SuiProtocolGroup{}
	for _, p := range positions {
		ref := p.table.Reserve
		reserve := ref.Fields
		coin := metadata[ref.CoinType]
		side := p.table.Side
		last, err := reserve.uint("last_update_timestamp")
		if err != nil || !last.IsUint64() {
			return nil, fmt.Errorf("invalid NAVI reserve time")
		}
		index, err := reserve.uint("current_" + side + "_index")
		if err != nil {
			return nil, err
		}
		rate, err := reserve.uint("current_" + side + "_rate")
		if err != nil {
			return nil, err
		}
		// Latest objects and head observations may come from different backends.
		// Keep the stored index if the reserve is ahead of the observed head.
		target := max(last.Uint64(), uint64(pin.Timestamp.UnixMilli()))
		index, err = naviIndexAt(index, rate, last.Uint64(), target, side == "borrow")
		if err != nil {
			return nil, err
		}
		amount := naviTokenAmount(p.principal, index, coin.Decimals)
		if amount.Sign() == 0 {
			continue
		}
		groupKey := ref.Market + ":" + p.account
		group := groups[groupKey]
		if group == nil {
			product, label := "lending", "Lending"
			if p.account != owner.Hex() && emodes[groupKey] {
				product, label = "multiply", "Multiply"
			}
			group = &SuiProtocolGroup{ID: "navi:" + groupKey, MarketID: ref.Market, Label: label, Metadata: map[string]any{"product": product, "account": p.account}}
			groups[groupKey] = group
		}
		kind := "asset"
		if side == "borrow" {
			kind = "debt"
		}
		group.Components = append(group.Components, SuiProtocolComponent{Kind: kind, Coin: coin, AmountRaw: amount.String(), Metadata: map[string]any{"reserve": ref.Asset, "scaledPrincipal": p.principal.String()}})
	}
	result := []SuiProtocolGroup{}
	for _, group := range groups {
		result = append(result, *group)
	}
	return result, nil
}

func suiProtocolMetadata(ctx context.Context, reader SuiReader, wanted map[string]bool) (map[string]SuiCoinMetadata, error) {
	coins := make([]string, 0, len(wanted))
	for coin := range wanted {
		coins = append(coins, coin)
	}
	sort.Strings(coins)
	metadata, unusable, err := reader.CoinMetadata(ctx, coins)
	if err != nil {
		return nil, err
	}
	if len(unusable) != 0 {
		return nil, fmt.Errorf("protocol coin metadata is unavailable for %d coin types", len(unusable))
	}
	for _, coin := range coins {
		if _, ok := metadata[coin]; !ok {
			return nil, fmt.Errorf("protocol coin metadata omitted %s", coin)
		}
	}
	return metadata, nil
}

func suiVaults(ctx context.Context, protocolID string, reader SuiReader, state suiProtocolState) ([]SuiProtocolGroup, error) {
	receipts := make(map[string]suiProtocolObject)
	vaultSet := make(map[string]bool)
	ids := []string{}
	for _, object := range state.Owned {
		if object.Kind == "receipt" {
			fields, err := suiObjectFields(object.Content)
			if err != nil {
				return nil, err
			}
			vault, err := suiReceiptVault(fields, protocolID)
			if err != nil || vault != object.Key {
				return nil, fmt.Errorf("invalid receipt vault attribution")
			}
			receipts[object.ID] = object
			vaultSet[object.Key] = true
			ids = append(ids, object.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	vaultIDs := []string{}
	for id := range vaultSet {
		parsed, err := ParseSuiAddress(id)
		if err != nil || parsed.Hex() != id {
			return nil, fmt.Errorf("receipt has invalid vault identity")
		}
		vaultIDs = append(vaultIDs, id)
	}
	vaults, states := state.Vaults, state.ReceiptStates
	byID := make(map[string]suiFields)
	coins := make(map[string]string)
	tokens := make(map[string]bool)
	for _, vault := range vaults {
		if vault.Kind != "vault" || !vaultSet[vault.ID] {
			return nil, fmt.Errorf("unexpected vault row")
		}
		fields, err := suiObjectFields(vault.Content)
		if err != nil {
			return nil, err
		}
		byID[vault.ID] = fields
		coin, err := suiCoinTypeFromVault(vault.ObjectType)
		if err != nil {
			return nil, err
		}
		coins[vault.ID] = coin
		tokens[coin] = true
	}
	if len(byID) != len(vaultSet) {
		return nil, fmt.Errorf("owned receipt's vault was unavailable")
	}
	metadata, err := suiProtocolMetadata(ctx, reader, tokens)
	if err != nil {
		return nil, err
	}
	var valuations map[string]voloValuation
	if protocolID == "volo-vaults" {
		active, activeErr := suiActiveShareVaults(state)
		if activeErr != nil {
			return nil, activeErr
		}
		activeIDs := []string{}
		for id := range active {
			activeIDs = append(activeIDs, id)
		}
		valuations, err = voloValuations(activeIDs, byID, coins, metadata, state)
		if err != nil {
			return nil, err
		}
	}
	result := []SuiProtocolGroup{}
	seen := make(map[string]bool)
	for _, object := range states {
		receipt, ok := receipts[object.Key]
		if !ok || seen[object.Key] || object.Kind != "receiptState" {
			return nil, fmt.Errorf("invalid vault receipt state")
		}
		seen[object.Key] = true
		vault := byID[receipt.Key]
		tableField := "user_states"
		if protocolID == "volo-vaults" {
			tableField = "receipts"
		}
		table, err := vault.object(tableField)
		if err != nil {
			return nil, err
		}
		parent, err := table.address("id")
		if err != nil || parent != object.Parent {
			return nil, fmt.Errorf("receipt state belongs to another vault")
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return nil, err
		}
		name, err := fields.address("name")
		if err != nil || name != object.Key {
			return nil, fmt.Errorf("invalid receipt state attribution")
		}
		state, err := fields.object("value")
		if err != nil {
			return nil, err
		}
		shares, err := state.uint("shares")
		if err != nil {
			return nil, err
		}
		coin := metadata[coins[receipt.Key]]
		group := SuiProtocolGroup{ID: protocolID + ":vault:" + receipt.ID, MarketID: receipt.Key, Label: "Vault", Metadata: map[string]any{"product": "vault", "receipt": receipt.ID, "shares": shares.String(), "valuation": "stored_nav"}}
		if shares.Sign() > 0 {
			totalShares, err := vault.uint("total_shares")
			if err != nil || totalShares.Sign() == 0 || shares.Cmp(totalShares) > 0 {
				return nil, fmt.Errorf("invalid vault share supply")
			}
			var amount *big.Int
			if protocolID == "navi" {
				assets, err := vault.uint("total_assets")
				if err != nil {
					return nil, err
				}
				amount = new(big.Int).Mul(shares, assets)
				amount.Div(amount, totalShares)
			} else {
				valuation := valuations[receipt.Key]
				amount = voloBaseAmount(shares, totalShares, valuation.Value, valuation.Price, coin.Decimals)
				if amount == nil {
					return nil, fmt.Errorf("Volo normalized base price is zero")
				}
				group.Metadata["navOldestTimestampMs"] = strconv.FormatUint(valuation.OldestTimestampMS, 10)
				group.Metadata["valuationPriceTimestampMs"] = strconv.FormatUint(valuation.PriceTimestampMS, 10)
				group.Metadata["valuation"] = "settled_nav"
			}
			group.Components = append(group.Components, SuiProtocolComponent{Kind: "asset", Coin: coin, AmountRaw: amount.String()})
		}
		if protocolID == "volo-vaults" {
			for _, field := range []string{"pending_deposit_balance", "claimable_principal"} {
				amount, err := state.uint(field)
				if err != nil {
					return nil, err
				}
				if amount.Sign() > 0 {
					group.Components = append(group.Components, SuiProtocolComponent{Kind: "asset", Coin: coin, AmountRaw: amount.String(), Metadata: map[string]any{"status": field}})
				}
			}
			pending, err := state.uint("pending_withdraw_shares")
			if err != nil || pending.Cmp(shares) > 0 {
				return nil, fmt.Errorf("invalid pending withdrawal shares")
			}
			// Pending withdrawals are still included in shares until execution.
			group.Metadata["pendingWithdrawShares"] = pending.String()
		}
		if len(group.Components) > 0 {
			result = append(result, group)
		}
	}
	return result, nil
}

type voloValuation struct {
	Value, Price      *big.Int
	OldestTimestampMS uint64
	PriceTimestampMS  uint64
}

func voloBaseAmount(shares, totalShares, value, price *big.Int, decimals uint8) *big.Int {
	// Match get_share_ratio -> mul_d -> div_with_oracle_price, preserving
	// every integer division. Fusing the fractions overstates small claims.
	scale := big.NewInt(1_000_000_000)
	ratio := new(big.Int).Div(new(big.Int).Mul(value, scale), totalShares)
	claim := new(big.Int).Div(new(big.Int).Mul(shares, ratio), scale)
	normalized := new(big.Int).Set(price)
	if decimals < 9 {
		normalized.Mul(normalized, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(9-decimals)), nil))
	} else {
		normalized.Div(normalized, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-9)), nil))
	}
	if normalized.Sign() == 0 {
		return nil
	}
	return claim.Mul(claim, big.NewInt(1_000_000_000_000_000_000)).Div(claim, normalized)
}

func voloValuations(vaultIDs []string, vaults map[string]suiFields, coins map[string]string, metadata map[string]SuiCoinMetadata, state suiProtocolState) (map[string]voloValuation, error) {
	if len(vaultIDs) == 0 {
		return map[string]voloValuation{}, nil
	}
	rows := state.AssetValues
	values := make(map[string]map[string]*big.Int)
	oracles := []suiProtocolObject{}
	for _, object := range state.OracleObjects {
		if object.Kind == "oracle" {
			oracles = append(oracles, object)
		}
	}
	timestamps := make(map[string]map[string]uint64)
	for _, row := range rows {
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		amount, err := fields.uint("amount")
		if err != nil {
			return nil, err
		}
		timestamp, err := fields.uint("timestamp")
		if err != nil || !timestamp.IsUint64() || timestamp.Sign() == 0 {
			return nil, fmt.Errorf("invalid Volo valuation timestamp")
		}
		asset, err := fields.text("asset")
		if err != nil {
			return nil, err
		}
		account, err := ParseSuiAddress(row.Account)
		if err != nil || account.Hex() != row.Account || row.Kind != "assetValue" || row.ID != "assetValue:"+row.Account+":"+asset || row.Market != "" {
			return nil, fmt.Errorf("invalid Volo asset valuation identity")
		}
		if values[row.Account] == nil {
			values[row.Account] = make(map[string]*big.Int)
			timestamps[row.Account] = make(map[string]uint64)
		}
		asset = strings.TrimPrefix(asset, "0x")
		if values[row.Account][asset] != nil {
			return nil, fmt.Errorf("duplicate Volo asset valuation")
		}
		values[row.Account][asset] = amount
		timestamps[row.Account][asset] = timestamp.Uint64()
	}
	result := make(map[string]voloValuation)
	for _, id := range vaultIDs {
		quote, ok := state.VaultPrices[id]
		if !ok {
			return nil, fmt.Errorf("Volo settlement quote is unavailable")
		}
		quoteRows := append(append([]suiProtocolObject{}, oracles...), quote)
		prices, priceTimes, err := voloOraclePrices(metadata, quoteRows)
		if err != nil {
			return nil, err
		}
		assets, ok := vaults[id]["asset_types"].([]any)
		if !ok {
			return nil, fmt.Errorf("vault asset inventory is missing")
		}
		total := new(big.Int)
		oldest := priceTimes[coins[id]]
		seen := make(map[string]bool)
		for _, asset := range assets {
			name, ok := asset.(string)
			if !ok {
				return nil, fmt.Errorf("invalid vault asset type")
			}
			name = strings.TrimPrefix(name, "0x")
			value, ok := values[id][name]
			if !ok || seen[name] {
				return nil, fmt.Errorf("Volo vault asset valuation is unavailable")
			}
			seen[name] = true
			if value.Sign() > 0 {
				if timestamps[id][name] != priceTimes[coins[id]] {
					return nil, fmt.Errorf("Volo NAV and settlement quote timestamps disagree")
				}
				oldest = min(oldest, timestamps[id][name])
			}
			total.Add(total, value)
		}
		price := prices[coins[id]]
		if price == nil || price.Sign() == 0 {
			return nil, fmt.Errorf("Volo vault base-coin oracle is unavailable")
		}
		result[id] = voloValuation{Value: total, Price: price, OldestTimestampMS: oldest, PriceTimestampMS: priceTimes[coins[id]]}
	}
	return result, nil
}

// Oracle table entries are authoritative even when no recent price event exists.
func voloOraclePrices(metadata map[string]SuiCoinMetadata, rows []suiProtocolObject) (map[string]*big.Int, map[string]uint64, error) {
	parent := ""
	for _, row := range rows {
		if row.Kind != "oracle" {
			continue
		}
		if parent != "" {
			return nil, nil, fmt.Errorf("ambiguous Volo oracle configuration")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, nil, err
		}
		table, err := fields.object("aggregators")
		if err != nil {
			return nil, nil, err
		}
		parent, err = table.address("id")
		if err != nil {
			return nil, nil, err
		}
	}
	if parent == "" {
		return nil, nil, fmt.Errorf("Volo oracle configuration is unavailable")
	}
	prices, times := make(map[string]*big.Int), make(map[string]uint64)
	for _, row := range rows {
		if row.Kind == "oracle" {
			continue
		}
		if row.Kind != "oraclePrice" || row.Parent != parent {
			return nil, nil, fmt.Errorf("invalid Volo oracle price attribution")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, nil, err
		}
		asset, err := fields.text("name")
		if err != nil || asset != row.Key {
			return nil, nil, fmt.Errorf("invalid Volo oracle asset")
		}
		coin, err := suiCoinType(asset)
		if err != nil {
			return nil, nil, err
		}
		info, err := fields.object("value")
		if err != nil {
			return nil, nil, err
		}
		decimals, err := info.uint("decimals")
		if err != nil || !decimals.IsUint64() || decimals.Uint64() > 255 {
			return nil, nil, fmt.Errorf("invalid Volo oracle decimals")
		}
		if coinInfo, wanted := metadata[coin]; wanted && uint64(coinInfo.Decimals) != decimals.Uint64() {
			return nil, nil, fmt.Errorf("Volo oracle and coin decimals disagree")
		}
		price, err := info.uint("price")
		if err != nil {
			return nil, nil, err
		}
		timestamp, err := info.uint("last_updated")
		if err != nil || !timestamp.IsUint64() {
			return nil, nil, fmt.Errorf("invalid Volo oracle timestamp")
		}
		if prices[coin] != nil {
			return nil, nil, fmt.Errorf("duplicate Volo oracle coin")
		}
		prices[coin], times[coin] = price, timestamp.Uint64()
	}
	return prices, times, nil
}
