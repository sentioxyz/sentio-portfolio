package portfolio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"
)

// Defining package IDs identify authentic positions and pools across upgrades.
const (
	cetusCLMMPackage   = "0x1eabed72c53feb3805120a081dc15963c204dc8d091542592abaf7a35689b2fb"
	bluefinCLMMPackage = "0x3492c874c1e3b3e2984e8c41b589e642d4d0a5d6459e5a9cfc2d52fd7c89c267"
	suiCLMMI32         = "0x714a63a0dba6da4f017b42d5d0fb78867f18bcde904868e51d951a5a6f5b7f57::i32::I32"
	cetusTablePackage  = "0xbe21a06129308e0495431d12286127897aff07a8ade3970495a4404d97f9eaaa"
)

type suiCLMMReward struct {
	coin         string
	global, rate *big.Int
	last, end    uint64
}
type suiCLMMPool struct {
	object                   SuiObject
	price, liquidity         *big.Int
	current                  int32
	fees                     [2]*big.Int
	rewards                  []suiCLMMReward
	tickTable, positionTable string
}
type suiCLMMPosition struct {
	object               SuiObject
	pool                 string
	coins                [2]string
	lower, upper         int32
	liquidity            *big.Int
	fees, lastFees       [2]*big.Int
	rewards, lastRewards []*big.Int
	stateID              string
	stateVersion         uint64
	ticks                [2]suiCLMMTick
}
type suiCLMMTick struct {
	price   *big.Int
	fees    [2]*big.Int
	rewards []*big.Int
}

func loadSuiCLMM(ctx context.Context, owner SuiAddress, reader SuiObjectReader, pkg string) ([]suiCLMMPosition, map[string]suiCLMMPool, error) {
	owned, err := reader.OwnedObjects(ctx, owner, pkg+"::position::Position")
	if err != nil {
		return nil, nil, err
	}
	positions := make([]suiCLMMPosition, 0, len(owned))
	pools := map[string]suiCLMMPool{}
	if len(owned) == 0 {
		return positions, pools, nil
	}
	parser := &suiCLMMParser{}
	seen, poolSet := map[string]bool{}, map[string]bool{}
	poolKey, loKey, hiKey := "pool", "tick_lower_index", "tick_upper_index"
	if pkg == bluefinCLMMPackage {
		poolKey, loKey, hiKey = "pool_id", "lower_tick", "upper_tick"
	}
	for _, o := range owned {
		if o.ObjectType != suiType(pkg+"::position::Position") || o.Owner != owner.Hex() || (o.OwnerKind != "ADDRESS" && o.OwnerKind != "CONSENSUS_ADDRESS") || seen[o.ID] {
			return nil, nil, fmt.Errorf("invalid CLMM position ownership or type")
		}
		seen[o.ID] = true
		f, e := suiObjectFields(o.Content)
		if e != nil {
			return nil, nil, e
		}
		if parser.address(f, "id") != o.ID {
			return nil, nil, fmt.Errorf("CLMM position ID mismatch")
		}
		p := suiCLMMPosition{object: o, pool: parser.address(f, poolKey), coins: [2]string{parser.coin(f, "coin_type_a"), parser.coin(f, "coin_type_b")}, lower: parser.tick(f, loKey), upper: parser.tick(f, hiKey), liquidity: parser.uint(f, "liquidity", 128)}
		if p.lower >= p.upper {
			return nil, nil, fmt.Errorf("invalid CLMM tick range")
		}
		if pkg == bluefinCLMMPackage {
			parseSuiCLMMAccrual(parser, f, &p, pkg)
		}
		if parser.err != nil {
			return nil, nil, parser.err
		}
		positions = append(positions, p)
		poolSet[p.pool] = true
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].object.ID < positions[j].object.ID })
	ids := make([]string, 0, len(poolSet))
	for id := range poolSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	objects, err := reader.Objects(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, id := range ids {
		o, exists := objects[id]
		if !exists || o.ID != id || o.OwnerKind != "SHARED" {
			return nil, nil, fmt.Errorf("CLMM pool is missing or not shared")
		}
		f, e := suiObjectFields(o.Content)
		if e != nil {
			return nil, nil, e
		}
		pool := parseSuiCLMMPool(parser, o, f, pkg)
		if parser.err != nil {
			return nil, nil, parser.err
		}
		pools[id] = pool
	}
	// Read only the owned positions' accounting records. Cetus NFTs hold display
	// liquidity that can remain stale after apply_liquidity_cut; PositionInfo is
	// authoritative for calculations and determines which boundaries are needed.
	ids = nil
	for i := range positions {
		p := &positions[i]
		pool := pools[p.pool]
		if pool.object.ObjectType != suiType(pkg+"::pool::Pool<"+p.coins[0]+","+p.coins[1]+">") {
			return nil, nil, fmt.Errorf("CLMM pool coin types disagree with position")
		}
		if pkg == cetusCLMMPackage {
			address, _ := ParseSuiAddress(p.object.ID)
			p.stateID, err = suiCLMMFieldID(pool.positionTable, "0x2::object::ID", address[:])
			if err != nil {
				return nil, nil, err
			}
			ids = append(ids, p.stateID)
		}
	}
	if pkg == cetusCLMMPackage {
		objects, err = reader.Objects(ctx, ids)
		if err != nil {
			return nil, nil, err
		}
		for i := range positions {
			p := &positions[i]
			pool := pools[p.pool]
			valueType := cetusTablePackage + "::linked_table::Node<0x2::object::ID," + pkg + "::position::PositionInfo>"
			field, e := suiCLMMField(objects, p.stateID, pool.positionTable, "0x2::object::ID", valueType)
			if e != nil {
				return nil, nil, e
			}
			if parser.address(field, "name") != p.object.ID {
				return nil, nil, fmt.Errorf("CLMM position table key mismatch")
			}
			f := parser.object(parser.object(field, "value"), "value")
			if parser.address(f, "position_id") != p.object.ID || parser.tick(f, "tick_lower_index") != p.lower || parser.tick(f, "tick_upper_index") != p.upper {
				return nil, nil, fmt.Errorf("CLMM position accounting disagrees with NFT")
			}
			p.liquidity = parser.uint(f, "liquidity", 128)
			p.stateVersion = objects[p.stateID].Version
			parseSuiCLMMAccrual(parser, f, p, pkg)
			if parser.err != nil {
				return nil, nil, parser.err
			}
		}
	}
	// Zero-liquidity positions retain stored fees/rewards without tick reads:
	// their boundaries may already have been removed from the pool.
	ids = nil
	for _, p := range positions {
		if p.liquidity.Sign() > 0 {
			for _, tick := range []int32{p.lower, p.upper} {
				id, _, _, e := suiCLMMTickKey(pools[p.pool].tickTable, tick, pkg)
				if e != nil {
					return nil, nil, e
				}
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > 0 {
		objects, err = reader.Objects(ctx, ids)
		if err != nil {
			return nil, nil, err
		}
	}
	for i := range positions {
		p := &positions[i]
		pool := pools[p.pool]
		if len(p.rewards) > len(pool.rewards) {
			return nil, nil, fmt.Errorf("CLMM position has unknown rewards")
		}
		for len(p.rewards) < len(pool.rewards) {
			p.rewards = append(p.rewards, new(big.Int))
			p.lastRewards = append(p.lastRewards, new(big.Int))
		}
		if p.liquidity.Sign() > 0 {
			for j, tick := range []int32{p.lower, p.upper} {
				id, key, valueType, e := suiCLMMTickKey(pool.tickTable, tick, pkg)
				if e != nil {
					return nil, nil, e
				}
				f, e := suiCLMMField(objects, id, pool.tickTable, key, valueType)
				if e != nil {
					return nil, nil, e
				}
				if pkg == cetusCLMMPackage {
					if parser.uint(f, "name", 64).Uint64() != uint64(int64(tick)+443636) {
						return nil, nil, fmt.Errorf("CLMM tick table key mismatch")
					}
					node := parser.object(f, "value")
					if parser.uint(node, "score", 64).Uint64() != uint64(int64(tick)+443636) {
						return nil, nil, fmt.Errorf("CLMM tick score mismatch")
					}
					f = parser.object(node, "value")
				} else {
					if parser.tick(f, "name") != tick {
						return nil, nil, fmt.Errorf("CLMM tick table key mismatch")
					}
					f = parser.object(f, "value")
				}
				if parser.tick(f, "index") != tick {
					return nil, nil, fmt.Errorf("CLMM tick index mismatch")
				}
				rewardKey := "rewards_growth_outside"
				if pkg == bluefinCLMMPackage {
					rewardKey = "reward_growths_outside"
				}
				t := suiCLMMTick{price: parser.uint(f, "sqrt_price", 128), fees: [2]*big.Int{parser.uint(f, "fee_growth_outside_a", 128), parser.uint(f, "fee_growth_outside_b", 128)}, rewards: parser.growths(f, rewardKey)}
				if t.price.Sign() == 0 || len(t.rewards) > len(pool.rewards) {
					return nil, nil, fmt.Errorf("invalid CLMM tick state")
				}
				// New reward streams start with zero growth on older ticks.
				for len(t.rewards) < len(pool.rewards) {
					t.rewards = append(t.rewards, new(big.Int))
				}
				p.ticks[j] = t
			}
			if p.ticks[0].price.Cmp(p.ticks[1].price) >= 0 {
				return nil, nil, fmt.Errorf("invalid CLMM boundary prices")
			}
		}
		if parser.err != nil {
			return nil, nil, parser.err
		}
	}
	return positions, pools, nil
}

func suiCLMMField(objects map[string]SuiObject, id, parent, key, value string) (suiFields, error) {
	o, ok := objects[id]
	if !ok || o.ID != id || o.OwnerKind != "OBJECT" || o.Owner != parent || o.ObjectType != suiFieldType(key, value) {
		return nil, fmt.Errorf("CLMM accounting field is missing or has invalid identity")
	}
	f, e := suiObjectFields(o.Content)
	if e != nil {
		return nil, e
	}
	fieldID, e := f.address("id")
	if e != nil || fieldID != id {
		return nil, fmt.Errorf("CLMM accounting field ID mismatch")
	}
	return f, nil
}
func suiCLMMTickKey(parent string, tick int32, pkg string) (id, key, value string, err error) {
	var data []byte
	if pkg == cetusCLMMPackage {
		key = "u64"
		value = cetusTablePackage + "::skip_list::Node<" + pkg + "::tick::Tick>"
		data = binary.LittleEndian.AppendUint64(nil, uint64(int64(tick)+443636))
	} else {
		key = suiCLMMI32
		value = pkg + "::tick::TickInfo"
		data = binary.LittleEndian.AppendUint32(nil, uint32(tick))
	}
	id, err = suiCLMMFieldID(parent, key, data)
	return
}

func parseSuiCLMMAccrual(parser *suiCLMMParser, f suiFields, p *suiCLMMPosition, pkg string) {
	feeA, feeB, lastA, lastB, vector, owed, last := "fee_owned_a", "fee_owned_b", "fee_growth_inside_a", "fee_growth_inside_b", "rewards", "amount_owned", "growth_inside"
	if pkg == bluefinCLMMPackage {
		feeA, feeB, lastA, lastB, vector, owed, last = "token_a_fee", "token_b_fee", "fee_growth_coin_a", "fee_growth_coin_b", "reward_infos", "coins_owed_reward", "reward_growth_inside_last"
	}
	p.fees = [2]*big.Int{parser.uint(f, feeA, 64), parser.uint(f, feeB, 64)}
	p.lastFees = [2]*big.Int{parser.uint(f, lastA, 128), parser.uint(f, lastB, 128)}
	for _, r := range parser.vector(f, vector) {
		p.rewards = append(p.rewards, parser.uint(r, owed, 64))
		p.lastRewards = append(p.lastRewards, parser.uint(r, last, 128))
	}
}
func parseSuiCLMMPool(parser *suiCLMMParser, o SuiObject, f suiFields, pkg string) suiCLMMPool {
	p := suiCLMMPool{object: o, price: parser.uint(f, "current_sqrt_price", 128), liquidity: parser.uint(f, "liquidity", 128), current: parser.tick(f, "current_tick_index")}
	if parser.address(f, "id") != o.ID || p.price.Sign() == 0 {
		parser.fail(fmt.Errorf("invalid CLMM pool identity or price"))
	}
	var rewards []suiFields
	if pkg == cetusCLMMPackage {
		p.fees = [2]*big.Int{parser.uint(f, "fee_growth_global_a", 128), parser.uint(f, "fee_growth_global_b", 128)}
		p.tickTable = parser.address(parser.object(parser.object(f, "tick_manager"), "ticks"), "id")
		p.positionTable = parser.address(parser.object(parser.object(f, "position_manager"), "positions"), "id")
		manager := parser.object(f, "rewarder_manager")
		last := parser.uint(manager, "last_updated_time", 64).Uint64()
		rewards = parser.vector(manager, "rewarders")
		for _, r := range rewards {
			p.rewards = append(p.rewards, suiCLMMReward{coin: parser.coin(r, "reward_coin"), global: parser.uint(r, "growth_global", 128), rate: parser.uint(r, "emissions_per_second", 128), last: last, end: ^uint64(0)})
		}
	} else {
		p.fees = [2]*big.Int{parser.uint(f, "fee_growth_global_coin_a", 128), parser.uint(f, "fee_growth_global_coin_b", 128)}
		p.tickTable = parser.address(parser.object(parser.object(f, "ticks_manager"), "ticks"), "id")
		rewards = parser.vector(f, "reward_infos")
		for _, r := range rewards {
			p.rewards = append(p.rewards, suiCLMMReward{coin: parser.coin(r, "reward_coin_type"), global: parser.uint(r, "reward_growth_global", 128), rate: parser.uint(r, "reward_per_seconds", 128), last: parser.uint(r, "last_update_time", 64).Uint64(), end: parser.uint(r, "ended_at_seconds", 64).Uint64()})
		}
	}
	return p
}

func suiCLMMGroups(ctx context.Context, reader SuiReader, protocol string, positions []suiCLMMPosition, pools map[string]suiCLMMPool, head SuiCheckpoint) ([]SuiProtocolGroup, error) {
	groups := []SuiProtocolGroup{}
	coins := map[string]bool{}
	for _, p := range positions {
		pool := pools[p.pool]
		g := SuiProtocolGroup{ID: protocol + ":" + p.object.ID, MarketID: p.pool, Label: "Liquidity", Metadata: map[string]any{"positionId": p.object.ID, "positionVersion": fmt.Sprint(p.object.Version), "poolVersion": fmt.Sprint(pool.object.Version), "liquidity": p.liquidity.String(), "tickLower": p.lower, "tickUpper": p.upper, "currentTick": pool.current, "sqrtPriceX64": pool.price.String(), "ownership": "direct"}}
		if p.stateID != "" {
			g.Metadata["positionInfoId"] = p.stateID
			g.Metadata["positionInfoVersion"] = fmt.Sprint(p.stateVersion)
		}
		add := func(coin, kind, role string, amount *big.Int) {
			if amount.Sign() == 0 {
				return
			}
			coins[coin] = true
			g.Components = append(g.Components, SuiProtocolComponent{Kind: kind, Coin: SuiCoinMetadata{CoinType: coin}, AmountRaw: amount.String(), Metadata: map[string]any{"positionId": p.object.ID, "role": role}})
		}
		if p.liquidity.Sign() > 0 {
			a, b := suiCLMMAmounts(p.liquidity, pool.price, p.ticks[0].price, p.ticks[1].price)
			if !a.IsUint64() || !b.IsUint64() {
				return nil, fmt.Errorf("CLMM principal exceeds u64")
			}
			add(p.coins[0], "asset", "principal", a)
			add(p.coins[1], "asset", "principal", b)
		}
		for i, owed := range p.fees {
			if p.liquidity.Sign() > 0 {
				inside := suiCLMMInside(pool.fees[i], p.ticks[0].fees[i], p.ticks[1].fees[i], pool.current, p.lower, p.upper)
				var err error
				owed, err = suiCLMMOwed(owed, p.lastFees[i], inside, p.liquidity)
				if err != nil {
					return nil, err
				}
			}
			add(p.coins[i], "reward", "uncollected-fee", owed)
		}
		for i, r := range pool.rewards {
			owed := p.rewards[i]
			if p.liquidity.Sign() > 0 {
				global := new(big.Int).Set(r.global)
				at := min(uint64(head.Timestamp.Unix()), r.end)
				// Object state may be newer than the observed head on pooled backends.
				// Keep stored growth in that case; never extrapolate backwards.
				if at > r.last && pool.liquidity.Sign() > 0 {
					delta := new(big.Int).Mul(new(big.Int).SetUint64(at-r.last), r.rate)
					delta.Div(delta, pool.liquidity)
					if delta.BitLen() > 128 {
						return nil, fmt.Errorf("CLMM reward growth exceeds u128")
					}
					global.Add(global, delta).Mod(global, suiCLMMMod128)
				}
				inside := suiCLMMInside(global, p.ticks[0].rewards[i], p.ticks[1].rewards[i], pool.current, p.lower, p.upper)
				var err error
				owed, err = suiCLMMOwed(owed, p.lastRewards[i], inside, p.liquidity)
				if err != nil {
					return nil, err
				}
			}
			add(r.coin, "reward", "reward", owed)
		}
		if len(g.Components) > 0 {
			groups = append(groups, g)
		}
	}
	if len(coins) == 0 {
		return groups, nil
	}
	metadata, err := suiProtocolMetadata(ctx, reader, coins)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		for j := range groups[i].Components {
			c := &groups[i].Components[j]
			c.Coin = metadata[c.Coin.CoinType]
		}
	}
	return groups, nil
}
