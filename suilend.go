package portfolio

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Only the defining package is an anchor. Current owner capabilities enumerate
// obligations and each obligation names its market, including isolated markets.
// No global user table, event scan, indexer or explorer supplies position state.
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

func readSuilendLatest(ctx context.Context, owner SuiAddress, reader SuiReader) (SuiProtocolPositions, error) {
	result := SuiProtocolPositions{ProtocolID: "suilend", ProtocolName: "Suilend"}
	objects, ok := reader.(SuiObjectReader)
	if !ok {
		return result, fmt.Errorf("Sui object reads are unavailable")
	}
	before, err := reader.LatestCheckpoint(ctx)
	if err != nil {
		return result, err
	}
	state, readErr := loadSuilend(ctx, owner, objects)
	after, err := reader.LatestCheckpoint(ctx)
	if err != nil {
		return result, err
	}
	result.Checkpoint, result.HeadBeforeRead = after, before.Sequence
	if readErr == nil {
		result.Groups, readErr = suilendLending(ctx, owner, after, reader, state)
	}
	if readErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("lending: %w", readErr))
		return result, nil
	}
	for i := range result.Groups {
		metadata := result.Groups[i].Metadata
		metadata["stateMode"] = "latest"
		metadata["headBeforeRead"] = fmt.Sprint(before.Sequence)
		metadata["headAfterRead"] = fmt.Sprint(after.Sequence)
	}
	return result, nil
}

func suilendTypeArgument(objectType, base string) (string, error) {
	prefix := suiType(base) + "<"
	if !strings.HasPrefix(objectType, prefix) || !strings.HasSuffix(objectType, ">") {
		return "", fmt.Errorf("invalid Suilend object type")
	}
	return NormalizeMoveType(objectType[len(prefix) : len(objectType)-1])
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

func loadSuilend(ctx context.Context, owner SuiAddress, reader SuiObjectReader) (suilendState, error) {
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
	marketIDs, parentIDs := map[string]bool{}, map[string]bool{}
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
		marketIDs[market], parentIDs[o.Owner] = true, true
		sort.Strings(p.caps)
		state.obligations = append(state.obligations, *p)
	}
	readIDs := make([]string, 0, len(marketIDs)+len(parentIDs))
	for id := range marketIDs {
		readIDs = append(readIDs, id)
	}
	for id := range parentIDs {
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
		fields, err := suiObjectFields(market.Content)
		if err != nil {
			return state, err
		}
		fieldID, err := fields.address("id")
		if err != nil || fieldID != market.ID {
			return state, fmt.Errorf("Suilend lending market ID mismatch")
		}
		table, err := suiTableID(fields, "obligations")
		if err != nil {
			return state, err
		}
		parent, exists := roots[p.object.Owner]
		if !exists || parent.ID != p.object.Owner || parent.OwnerKind != "OBJECT" || parent.Owner != table || parent.ObjectType != suiFieldType("0x2::dynamic_object_field::Wrapper<0x2::object::ID>", "0x2::object::ID") {
			return state, fmt.Errorf("Suilend obligation belongs to another market table")
		}
		link, err := suiObjectFields(parent.Content)
		if err != nil {
			return state, err
		}
		name, err := link.object("name")
		if err != nil {
			return state, err
		}
		key, err := name.address("name")
		if err != nil || key != p.object.ID {
			return state, fmt.Errorf("Suilend obligation table key mismatch")
		}
		value, err := link.address("value")
		if err != nil || value != p.object.ID {
			return state, fmt.Errorf("Suilend obligation table value mismatch")
		}
		if _, exists := state.markets[market.ID]; exists {
			continue
		}
		reserves, err := suilendStructVector(fields, "reserves")
		if err != nil {
			return state, err
		}
		seenCoins, seenIDs := map[string]bool{}, map[string]bool{}
		for index, fields := range reserves {
			reserve, err := parseSuilendReserve(fields, market.ID, index)
			if err != nil {
				return state, err
			}
			if seenCoins[reserve.coinType] || seenIDs[reserve.id] {
				return state, fmt.Errorf("duplicate Suilend reserve")
			}
			seenCoins[reserve.coinType], seenIDs[reserve.id] = true, true
			state.markets[market.ID] = append(state.markets[market.ID], reserve)
		}
	}
	return state, nil
}

func suilendLending(ctx context.Context, owner SuiAddress, pin SuiCheckpoint, reader SuiReader, state suilendState) ([]SuiProtocolGroup, error) {
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
				// A newer backend may supply a reserve ahead of the observed head.
				// Retain its stored interest rather than compound backwards.
				reserve, err = reserve.at(max(reserve.updated, uint64(pin.Timestamp.Unix())))
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
