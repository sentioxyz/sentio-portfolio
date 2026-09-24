package portfolio

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The defining package anchors capability, obligation and market types.
// Indexed owner capabilities enumerate obligations, including isolated markets.
const suilendPackage = "0xf95b06141ed4a174f239417323bde3f209b972f5930d8521ea38a52aff3a6ddf"
const suilendCapType = suilendPackage + "::lending_market::ObligationOwnerCap"

type suilendObligation struct {
	object             SuiObject
	fields             suiFields
	market, marketType string
	caps               []string
}

type suilendState struct {
	obligations []suilendObligation
	markets     map[string][]suilendReserve
}

// The loader only receives processor-backed object inventories in query paths.
// Coin metadata and checkpoint selection are independently index-backed.
type suilendObjectSource interface {
	OwnedObjects(context.Context, SuiAddress, string) ([]SuiObject, error)
	Objects(context.Context, []string) (map[string]SuiObject, error)
}

func suilendTypeArgument(objectType, base string) (string, error) {
	prefix := suiType(base) + "<"
	if !strings.HasPrefix(objectType, prefix) || !strings.HasSuffix(objectType, ">") {
		return "", fmt.Errorf("invalid Suilend object type")
	}
	// Market witnesses are generic TypeTags; do not restrict them to structs.
	parser := moveTypeParser{input: objectType[len(prefix) : len(objectType)-1]}
	argument, err := parser.parseType(0)
	parser.skipBlanks()
	if err != nil || parser.position != len(parser.input) || len(argument) > moveTypeMaxLength {
		return "", fmt.Errorf("invalid Suilend market type argument")
	}
	return argument, nil
}

func suilendStructVector(fields suiFields, key string) ([]suiFields, error) {
	values, ok := fields[key].([]any)
	if !ok || len(values) > suiObjectLimit {
		return nil, fmt.Errorf("invalid Suilend vector %s", key)
	}
	result := make([]suiFields, len(values))
	for i, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Suilend vector %s element", key)
		}
		result[i] = suiFields(object)
	}
	return result, nil
}

func suilendCoinType(fields suiFields) (string, error) {
	// gRPC's Move JSON renders TypeName as a string; JSON-RPC wraps name.
	if raw, ok := fields["coin_type"].(string); ok {
		return suiCoinType(raw)
	}
	typ, err := fields.object("coin_type")
	if err != nil {
		return "", err
	}
	raw, err := typ.text("name")
	if err != nil {
		return "", err
	}
	return suiCoinType(raw)
}

func loadSuilend(ctx context.Context, owner SuiAddress, reader suilendObjectSource) (suilendState, error) {
	state := suilendState{markets: map[string][]suilendReserve{}}
	caps, err := reader.OwnedObjects(ctx, owner, suilendCapType)
	if err != nil {
		return state, err
	}
	if len(caps) == 0 {
		return state, nil
	}
	byID := map[string]*suilendObligation{}
	capIDs := map[string]bool{}
	for _, cap := range caps {
		if (cap.OwnerKind != "ADDRESS" && cap.OwnerKind != "CONSENSUS_ADDRESS") || cap.Owner != owner.Hex() || capIDs[cap.ID] {
			return state, fmt.Errorf("invalid Suilend capability ownership")
		}
		capIDs[cap.ID] = true
		marketType, err := suilendTypeArgument(cap.ObjectType, suilendCapType)
		if err != nil {
			return state, err
		}
		fields, err := suiObjectFields(cap.Content)
		if err != nil {
			return state, err
		}
		id, err := fields.address("id")
		if err != nil || id != cap.ID {
			return state, fmt.Errorf("Suilend capability ID mismatch")
		}
		obligation, err := fields.address("obligation_id")
		if err != nil {
			return state, err
		}
		if existing := byID[obligation]; existing != nil {
			if existing.marketType != marketType {
				return state, fmt.Errorf("Suilend capabilities disagree on market type")
			}
			existing.caps = append(existing.caps, cap.ID)
		} else {
			byID[obligation] = &suilendObligation{marketType: marketType, caps: []string{cap.ID}}
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	objects, err := reader.Objects(ctx, ids)
	if err != nil {
		return state, err
	}
	marketIDs := map[string]bool{}
	for _, id := range ids {
		o, exists := objects[id]
		p := byID[id]
		if !exists || o.ID != id || o.OwnerKind != "OBJECT" || o.ObjectType != suiType(suilendPackage+"::obligation::Obligation<"+p.marketType+">") {
			return state, fmt.Errorf("owned Suilend obligation is missing or has invalid identity")
		}
		fields, err := suiObjectFields(o.Content)
		if err != nil {
			return state, err
		}
		fieldID, err := fields.address("id")
		if err != nil || fieldID != id {
			return state, fmt.Errorf("Suilend obligation ID mismatch")
		}
		market, err := fields.address("lending_market_id")
		if err != nil {
			return state, err
		}
		p.object, p.fields, p.market = o, fields, market
		marketIDs[market] = true
		sort.Strings(p.caps)
		state.obligations = append(state.obligations, *p)
	}
	readIDs := make([]string, 0, len(marketIDs))
	for id := range marketIDs {
		readIDs = append(readIDs, id)
	}
	sort.Strings(readIDs)
	roots, err := reader.Objects(ctx, readIDs)
	if err != nil {
		return state, err
	}
	for _, p := range state.obligations {
		market, exists := roots[p.market]
		if !exists || market.ID != p.market || market.OwnerKind != "SHARED" || market.ObjectType != suiType(suilendPackage+"::lending_market::LendingMarket<"+p.marketType+">") {
			return state, fmt.Errorf("invalid or missing Suilend lending market")
		}
		var parsed suilendParsedMarket
		if source, ok := reader.(suilendMarketSource); ok {
			parsed, err = source.suilendMarket(market)
		} else {
			parsed, err = parseSuilendMarket(market)
		}
		if err != nil {
			return state, err
		}
		parentID, err := suilendObligationParent(parsed.table, p.object.ID)
		if err != nil || p.object.Owner != parentID {
			return state, fmt.Errorf("Suilend obligation belongs to another market table")
		}
		state.markets[market.ID] = parsed.reserves
	}
	return state, nil
}

// suilendParsedMarket is one decoded LendingMarket: its obligation table and its
// reserve vector. Both are read-only after parsing; interest accrual copies a
// reserve and allocates new integers, so one parse may serve every account.
type suilendParsedMarket struct {
	table    string
	reserves []suilendReserve
}

func parseSuilendMarket(market SuiObject) (suilendParsedMarket, error) {
	fields, err := suiObjectFields(market.Content)
	if err != nil {
		return suilendParsedMarket{}, err
	}
	id, err := fields.address("id")
	if err != nil || id != market.ID {
		return suilendParsedMarket{}, fmt.Errorf("Suilend lending market ID mismatch")
	}
	table, err := suiTableID(fields, "obligations")
	if err != nil {
		return suilendParsedMarket{}, err
	}
	reserves, err := suilendStructVector(fields, "reserves")
	if err != nil {
		return suilendParsedMarket{}, err
	}
	result := make([]suilendReserve, 0, len(reserves))
	seenCoins, seenIDs := map[string]bool{}, map[string]bool{}
	for index, fields := range reserves {
		reserve, err := parseSuilendReserve(fields, market.ID, index)
		if err != nil {
			return suilendParsedMarket{}, err
		}
		if seenCoins[reserve.coinType] || seenIDs[reserve.id] {
			return suilendParsedMarket{}, fmt.Errorf("duplicate Suilend reserve")
		}
		seenCoins[reserve.coinType], seenIDs[reserve.id] = true, true
		result = append(result, reserve)
	}
	return suilendParsedMarket{table: table, reserves: result}, nil
}

// A source may hand back a market it already decoded for the exact same object
// version. Snapshot calculators decode each lending market once per snapshot
// instead of parsing its full reserve vector again for every account.
type suilendMarketSource interface {
	suilendMarket(SuiObject) (suilendParsedMarket, error)
}

// suilendMarketMemo keys decoded markets by object ID, version and digest, so a
// different market state can never be served from the cache.
type suilendMarketMemo struct {
	mu      sync.Mutex
	markets map[string]suilendParsedMarket
}

func newSuilendMarketMemo() *suilendMarketMemo {
	return &suilendMarketMemo{markets: map[string]suilendParsedMarket{}}
}

func (m *suilendMarketMemo) market(market SuiObject) (suilendParsedMarket, error) {
	cacheable := m != nil && market.Version != 0 && market.Digest != ""
	key := market.ID + ":" + strconv.FormatUint(market.Version, 10) + ":" + market.Digest
	if cacheable {
		m.mu.Lock()
		parsed, ok := m.markets[key]
		m.mu.Unlock()
		if ok {
			return parsed, nil
		}
	}
	parsed, err := parseSuilendMarket(market)
	if err != nil {
		return suilendParsedMarket{}, err
	}
	if cacheable {
		m.mu.Lock()
		m.markets[key] = parsed
		m.mu.Unlock()
	}
	return parsed, nil
}

func suilendLending(ctx context.Context, owner SuiAddress, pin SuiCheckpoint, reader suiCoinMetadataReader, state suilendState) ([]SuiProtocolGroup, error) {
	groups := []SuiProtocolGroup{}
	tokens := map[string]bool{}
	for _, p := range state.obligations {
		group := SuiProtocolGroup{ID: "suilend:" + p.market + ":" + p.object.ID, MarketID: p.market, Label: "Lending", Metadata: map[string]any{"product": "lending", "account": owner.Hex(), "obligation": p.object.ID, "ownerCapabilities": p.caps, "marketType": p.marketType}}
		for _, side := range []string{"deposits", "borrows"} {
			positions, err := suilendStructVector(p.fields, side)
			if err != nil {
				return nil, err
			}
			seen := map[string]bool{}
			for _, position := range positions {
				coin, err := suilendCoinType(position)
				if err != nil {
					return nil, err
				}
				index, err := position.uint("reserve_array_index")
				reserves := state.markets[p.market]
				if err != nil || !index.IsUint64() || index.Uint64() >= uint64(len(reserves)) || seen[coin] {
					return nil, fmt.Errorf("invalid Suilend position reserve index")
				}
				seen[coin] = true
				reserve := reserves[index.Uint64()]
				if reserve.coinType != coin {
					return nil, fmt.Errorf("Suilend position coin disagrees with reserve")
				}
				if pin.Timestamp.Unix() < 0 {
					return nil, fmt.Errorf("invalid Suilend checkpoint time")
				}
				// Indexed state is pinned to P: a reserve from after P is invalid.
				reserve, err = reserve.at(uint64(pin.Timestamp.Unix()))
				if err != nil {
					return nil, err
				}
				kind := "asset"
				amountKey := "deposited_ctoken_amount"
				amount, err := position.uint(amountKey)
				if side == "borrows" {
					kind, amountKey = "debt", "borrowed_amount"
					amount, err = suilendDecimal(position, amountKey)
				}
				if err != nil {
					return nil, err
				}
				principal := amount.String()
				if kind == "asset" {
					amount, err = reserve.depositAmount(amount)
				} else {
					snapshot, readErr := suilendDecimal(position, "cumulative_borrow_rate")
					if readErr != nil {
						return nil, readErr
					}
					amount, err = reserve.debtAmount(amount, snapshot)
				}
				if err != nil {
					return nil, err
				}
				if amount.Sign() == 0 {
					continue
				}
				tokens[coin] = true
				group.Components = append(group.Components, SuiProtocolComponent{Kind: kind, Coin: SuiCoinMetadata{CoinType: coin, Decimals: reserve.decimals}, AmountRaw: amount.String(), Metadata: map[string]any{"reserve": reserve.id, "reserveIndex": index.String(), amountKey: principal}})
			}
		}
		if len(group.Components) > 0 {
			groups = append(groups, group)
		}
	}
	if len(tokens) == 0 {
		return groups, nil
	}
	metadata, err := suiProtocolMetadata(ctx, reader, tokens)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		for j := range groups[i].Components {
			component := &groups[i].Components[j]
			coin := metadata[component.Coin.CoinType]
			if coin.Decimals != component.Coin.Decimals {
				return nil, fmt.Errorf("Suilend reserve decimals disagree with coin metadata")
			}
			component.Coin = coin
		}
	}
	return groups, nil
}
