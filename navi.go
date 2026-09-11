package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

// Read pins every index query to the requested checkpoint. Index progress is a
// coverage prerequisite, never permission to substitute its head for the pin.
func (r *SuiProtocolReader) Read(ctx context.Context, protocolID string, owner SuiAddress, pin SuiCheckpoint, reader SuiReader) (SuiProtocolPositions, error) {
	name := map[string]string{"navi": "NAVI", "volo-vaults": "Volo Vaults"}[protocolID]
	result := SuiProtocolPositions{ProtocolID: protocolID, ProtocolName: name, Checkpoint: pin}
	if name == "" {
		return result, fmt.Errorf("unsupported Sui protocol")
	}
	if pin.Timestamp.UnixMilli() <= 0 || pin.Digest == ([32]byte{}) {
		return result, fmt.Errorf("invalid Sui checkpoint pin")
	}
	watermark, err := r.LatestCheckpoint(ctx, protocolID)
	if err != nil {
		return result, err
	}
	if watermark.Sequence < pin.Sequence {
		return result, fmt.Errorf("%s history covers checkpoint %d; requested %d", name, watermark.Sequence, pin.Sequence)
	}
	owned, err := r.objects(ctx, protocolID, pin.Sequence, `owner: `+strconv.Quote(owner.Hex()))
	if err != nil {
		return result, err
	}
	for _, object := range owned {
		if object.Owner != owner.Hex() {
			return result, fmt.Errorf("history returned an object belonging to another owner")
		}
	}
	if protocolID == "navi" {
		groups, err := r.naviLending(ctx, owner, pin, reader, owned)
		if err != nil {
			result.Errors = append(result.Errors, err)
		} else {
			result.Groups = append(result.Groups, groups...)
		}
	}
	groups, err := r.suiVaults(ctx, protocolID, pin, reader, owned)
	if err != nil {
		result.Errors = append(result.Errors, err)
	} else {
		result.Groups = append(result.Groups, groups...)
	}
	sort.Slice(result.Groups, func(i, j int) bool { return result.Groups[i].ID < result.Groups[j].ID })
	return result, nil
}

func (r *SuiProtocolReader) naviLending(ctx context.Context, owner SuiAddress, pin SuiCheckpoint, reader SuiReader, owned []suiHistoryObject) ([]SuiProtocolGroup, error) {
	accounts := map[string]bool{owner.Hex(): true}
	for _, object := range owned {
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
	names := make([]string, 0, len(accounts))
	for account := range accounts {
		names = append(names, account)
	}
	sort.Strings(names)
	principals, err := r.objects(ctx, "navi", pin.Sequence, `kind: "principal", owner_in: `+quotedStrings(names))
	if err != nil {
		return nil, err
	}
	type position struct {
		account, market, asset, side, parent string
		principal                            *big.Int
	}
	positions := []position{}
	seen := map[string]bool{}
	for _, row := range principals {
		if row.Kind != "principal" || !accounts[row.Owner] {
			return nil, fmt.Errorf("unexpected lending principal owner")
		}
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		account, err := fields.address("name")
		if err != nil || account != row.Owner {
			return nil, fmt.Errorf("invalid lending principal attribution")
		}
		amount, err := fields.uint("value")
		if err != nil {
			return nil, err
		}
		parts := strings.Split(row.Key, ":")
		if len(parts) != 3 || (parts[2] != "supply" && parts[2] != "borrow") {
			return nil, fmt.Errorf("invalid principal reserve key")
		}
		for _, id := range parts[:2] {
			if n, e := strconv.ParseUint(id, 10, 64); e != nil || strconv.FormatUint(n, 10) != id {
				return nil, fmt.Errorf("invalid principal market or asset")
			}
		}
		identity := row.Owner + ":" + row.Key
		if seen[identity] {
			return nil, fmt.Errorf("duplicate lending principal")
		}
		seen[identity] = true
		if amount.Sign() == 0 {
			continue
		}
		positions = append(positions, position{row.Owner, parts[0], parts[1], parts[2], row.Parent, amount})
	}
	if len(positions) == 0 {
		return nil, nil
	}
	reserves, err := r.objects(ctx, "navi", pin.Sequence, `kind: "reserve"`)
	if err != nil {
		return nil, err
	}
	byKey := map[string]suiFields{}
	coinTypes := map[string]string{}
	for _, row := range reserves {
		if row.Kind != "reserve" || byKey[row.Key] != nil {
			return nil, fmt.Errorf("invalid or duplicate NAVI reserve")
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
		if err != nil {
			return nil, err
		}
		parts := strings.Split(row.Key, ":")
		if len(parts) != 2 || parts[1] != asset.String() {
			return nil, fmt.Errorf("invalid NAVI reserve identity")
		}
		raw, err := reserve.text("coin_type")
		if err != nil {
			return nil, err
		}
		coin, err := suiCoinType(raw)
		if err != nil {
			return nil, err
		}
		byKey[row.Key] = reserve
		coinTypes[row.Key] = coin
	}
	tokens := map[string]bool{}
	for _, p := range positions {
		key := p.market + ":" + p.asset
		reserve := byKey[key]
		if reserve == nil {
			return nil, fmt.Errorf("NAVI reserve %s is not indexed at checkpoint %d", key, pin.Sequence)
		}
		balance, err := reserve.object(p.side + "_balance")
		if err != nil {
			return nil, err
		}
		table, err := balance.object("user_state")
		if err != nil {
			return nil, err
		}
		parent, err := table.address("id")
		if err != nil || parent != p.parent {
			return nil, fmt.Errorf("principal belongs to another NAVI reserve")
		}
		tokens[coinTypes[key]] = true
	}
	metadata, err := suiProtocolMetadata(ctx, reader, tokens)
	if err != nil {
		return nil, err
	}
	emodeRows, err := r.values(ctx, "navi", pin.Sequence, `kind: "emode", account_in: `+quotedStrings(names))
	if err != nil {
		return nil, err
	}
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
		reserveKey := p.market + ":" + p.asset
		reserve := byKey[reserveKey]
		coin := metadata[coinTypes[reserveKey]]
		last, err := reserve.uint("last_update_timestamp")
		if err != nil || !last.IsUint64() {
			return nil, fmt.Errorf("invalid NAVI reserve time")
		}
		index, err := reserve.uint("current_" + p.side + "_index")
		if err != nil {
			return nil, err
		}
		rate, err := reserve.uint("current_" + p.side + "_rate")
		if err != nil {
			return nil, err
		}
		index, err = naviIndexAt(index, rate, last.Uint64(), uint64(pin.Timestamp.UnixMilli()), p.side == "borrow")
		if err != nil {
			return nil, err
		}
		amount := naviTokenAmount(p.principal, index, coin.Decimals)
		if amount.Sign() == 0 {
			continue
		}
		groupKey := p.market + ":" + p.account
		group := groups[groupKey]
		if group == nil {
			product, label := "lending", "Lending"
			if p.account != owner.Hex() && emodes[groupKey] {
				product, label = "multiply", "Multiply"
			}
			group = &SuiProtocolGroup{ID: "navi:" + groupKey, MarketID: p.market, Label: label, Metadata: map[string]any{"product": product, "account": p.account}}
			groups[groupKey] = group
		}
		kind := "asset"
		if p.side == "borrow" {
			kind = "debt"
		}
		group.Components = append(group.Components, SuiProtocolComponent{Kind: kind, Coin: coin, AmountRaw: amount.String(), Metadata: map[string]any{"reserve": p.asset, "scaledPrincipal": p.principal.String()}})
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

func (r *SuiProtocolReader) suiVaults(ctx context.Context, protocolID string, pin SuiCheckpoint, reader SuiReader, owned []suiHistoryObject) ([]SuiProtocolGroup, error) {
	receipts := make(map[string]suiHistoryObject)
	vaultSet := make(map[string]bool)
	ids := []string{}
	for _, object := range owned {
		if object.Kind == "receipt" {
			fields, err := suiObjectFields(object.Content)
			if err != nil {
				return nil, err
			}
			vault, err := fields.address("vault_id")
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
	vaults, err := r.objects(ctx, protocolID, pin.Sequence, `kind: "vault", id_in: `+quotedStrings(vaultIDs))
	if err != nil {
		return nil, err
	}
	states, err := r.objects(ctx, protocolID, pin.Sequence, `kind: "receiptState", key_in: `+quotedStrings(ids))
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("owned receipt's vault was not indexed")
	}
	metadata, err := suiProtocolMetadata(ctx, reader, tokens)
	if err != nil {
		return nil, err
	}
	var valuations map[string]voloValuation
	if protocolID == "volo-vaults" {
		valuations, err = r.voloValuations(ctx, pin, vaultIDs, byID, coins, metadata)
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

func (r *SuiProtocolReader) voloValuations(ctx context.Context, pin SuiCheckpoint, vaultIDs []string, vaults map[string]suiFields, coins map[string]string, metadata map[string]SuiCoinMetadata) (map[string]voloValuation, error) {
	rows, err := r.values(ctx, "volo-vaults", pin.Sequence, `kind: "assetValue"`)
	if err != nil {
		return nil, err
	}
	values := make(map[string]map[string]*big.Int)
	prices, priceTimes, err := r.voloOraclePrices(ctx, pin, metadata)
	if err != nil {
		return nil, err
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
		if err != nil || !timestamp.IsUint64() || timestamp.Sign() == 0 || timestamp.Uint64() > uint64(pin.Timestamp.UnixMilli()) {
			return nil, fmt.Errorf("invalid or future Volo valuation timestamp")
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
		assets, ok := vaults[id]["asset_types"].([]any)
		if !ok {
			return nil, fmt.Errorf("vault asset inventory is missing")
		}
		total := new(big.Int)
		oldest := uint64(pin.Timestamp.UnixMilli())
		seen := make(map[string]bool)
		for _, asset := range assets {
			name, ok := asset.(string)
			if !ok {
				return nil, fmt.Errorf("invalid vault asset type")
			}
			name = strings.TrimPrefix(name, "0x")
			value, ok := values[id][name]
			if !ok || seen[name] {
				return nil, fmt.Errorf("Volo vault asset valuation is not indexed")
			}
			seen[name] = true
			oldest = min(oldest, timestamps[id][name])
			total.Add(total, value)
		}
		price := prices[coins[id]]
		if price == nil || price.Sign() == 0 {
			return nil, fmt.Errorf("Volo vault base-coin oracle is not indexed")
		}
		result[id] = voloValuation{Value: total, Price: price, OldestTimestampMS: min(oldest, priceTimes[coins[id]])}
	}
	return result, nil
}

// Oracle table objects predate AssetPriceUpdated. Replaying only that newer
// event loses the first vaults' price history and unchanged oracle entries.
func (r *SuiProtocolReader) voloOraclePrices(ctx context.Context, pin SuiCheckpoint, metadata map[string]SuiCoinMetadata) (map[string]*big.Int, map[string]uint64, error) {
	rows, err := r.objects(ctx, "volo-vaults", pin.Sequence, `kind_in: ["oracle", "oraclePrice"]`)
	if err != nil {
		return nil, nil, err
	}
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
		return nil, nil, fmt.Errorf("Volo oracle configuration is not indexed")
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
		if err != nil || !timestamp.IsUint64() || timestamp.Uint64() > uint64(pin.Timestamp.UnixMilli()) {
			return nil, nil, fmt.Errorf("invalid or future Volo oracle timestamp")
		}
		if prices[coin] != nil {
			return nil, nil, fmt.Errorf("duplicate Volo oracle coin")
		}
		prices[coin], times[coin] = price, timestamp.Uint64()
	}
	return prices, times, nil
}
