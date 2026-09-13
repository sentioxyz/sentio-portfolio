package portfolio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
)

func clmmQ(n int64) string                { return new(big.Int).Mul(big.NewInt(n), suiCLMMQ64).String() }
func clmmTickBits(n int32) map[string]any { return map[string]any{"bits": uint32(n)} }
func clmmFixture(protocol string) (*latestSuiFixture, SuiAddress) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	pkg := cetusCLMMPackage
	if protocol == "bluefin" {
		pkg = bluefinCLMMPackage
	}
	coinB := suiType("0x3::coin::COIN")
	f.metadata[coinB] = SuiCoinMetadata{CoinType: coinB, Symbol: "COIN", Decimals: 6}
	pool, pos, ticks, states := naviAddress("0x21"), naviAddress("0x22"), naviAddress("0x23"), naviAddress("0x24")
	nft := map[string]any{"coin_type_a": suiLongType, "coin_type_b": coinB, "liquidity": "100"}
	pf := map[string]any{"current_sqrt_price": clmmQ(1), "current_tick_index": clmmTickBits(0), "liquidity": "100"}
	accrual := map[string]any{}
	if protocol == "cetus" {
		nft["pool"], nft["tick_lower_index"], nft["tick_upper_index"] = pool, clmmTickBits(-100), clmmTickBits(100)
		pf["fee_growth_global_a"], pf["fee_growth_global_b"] = clmmQ(10), clmmQ(12)
		pf["tick_manager"] = map[string]any{"ticks": map[string]any{"id": ticks}}
		pf["position_manager"] = map[string]any{"positions": map[string]any{"id": states}}
		pf["rewarder_manager"] = map[string]any{"last_updated_time": "990", "rewarders": []any{map[string]any{"reward_coin": map[string]any{"name": suiLongType}, "growth_global": clmmQ(20), "emissions_per_second": clmmQ(100)}}}
		accrual = map[string]any{"position_id": pos, "liquidity": "100", "tick_lower_index": clmmTickBits(-100), "tick_upper_index": clmmTickBits(100), "fee_owned_a": "5", "fee_owned_b": "9", "fee_growth_inside_a": clmmQ(3), "fee_growth_inside_b": clmmQ(2), "rewards": []any{map[string]any{"amount_owned": "7", "growth_inside": clmmQ(4)}}}
		address, _ := ParseSuiAddress(pos)
		id, _ := suiCLMMFieldID(states, "0x2::object::ID", address[:])
		f.objects[id] = latestObject(id, suiFieldType("0x2::object::ID", cetusTablePackage+"::linked_table::Node<0x2::object::ID,"+pkg+"::position::PositionInfo>"), "OBJECT", states, map[string]any{"name": pos, "value": map[string]any{"value": accrual}})
	} else {
		nft["pool_id"], nft["lower_tick"], nft["upper_tick"] = pool, clmmTickBits(-100), clmmTickBits(100)
		nft["fee_growth_coin_a"], nft["fee_growth_coin_b"], nft["token_a_fee"], nft["token_b_fee"] = clmmQ(3), clmmQ(2), "5", "9"
		nft["reward_infos"] = []any{map[string]any{"coins_owed_reward": "7", "reward_growth_inside_last": clmmQ(4)}}
		pf["fee_growth_global_coin_a"], pf["fee_growth_global_coin_b"] = clmmQ(10), clmmQ(12)
		pf["ticks_manager"] = map[string]any{"ticks": map[string]any{"id": ticks}}
		pf["reward_infos"] = []any{map[string]any{"last_update_time": "990", "ended_at_seconds": "2000", "reward_coin_type": suiLongType, "reward_growth_global": clmmQ(20), "reward_per_seconds": clmmQ(100)}}
	}
	f.objects[pos] = latestObject(pos, pkg+"::position::Position", "ADDRESS", owner.Hex(), nft)
	f.objects[pool] = latestObject(pool, pkg+"::pool::Pool<"+suiLongType+","+coinB+">", "SHARED", "", pf)
	for i, tick := range []int32{-100, 100} {
		id, key, value, _ := suiCLMMTickKey(ticks, tick, pkg)
		price := new(big.Int).Div(new(big.Int).Set(suiCLMMQ64), big.NewInt(2)).String()
		a, b, r := clmmQ(2), clmmQ(1), clmmQ(2)
		if i == 1 {
			price = clmmQ(2)
			a, b, r = clmmQ(1), clmmQ(2), clmmQ(1)
		}
		tf := map[string]any{"index": clmmTickBits(tick), "sqrt_price": price, "fee_growth_outside_a": a, "fee_growth_outside_b": b}
		var name any = clmmTickBits(tick)
		var valueFields any = tf
		if protocol == "cetus" {
			tf["rewards_growth_outside"] = []any{r}
			score := fmt.Sprint(int64(tick) + 443636)
			name = score
			valueFields = map[string]any{"score": score, "value": tf}
		} else {
			tf["reward_growths_outside"] = []any{r}
		}
		f.objects[id] = latestObject(id, suiFieldType(key, value), "OBJECT", ticks, map[string]any{"name": name, "value": valueFields})
	}
	return f, owner
}

func TestSuiCLMMLatestPrincipalFeesAndRewards(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			f, owner := clmmFixture(protocol)
			r := NewSuiProtocolReader()
			if !slices.Contains(r.ProtocolIDs(), protocol) {
				t.Fatal("not registered")
			}
			got, err := r.ReadLatest(context.Background(), protocol, owner, f)
			if err != nil || len(got.Errors) != 0 || len(got.Groups) != 1 {
				t.Fatalf("read %+v %v", got, err)
			}
			g := got.Groups[0]
			want := []string{"50", "50", "405", "709", "2307"}
			kinds := []string{"asset", "asset", "reward", "reward", "reward"}
			if len(g.Components) != len(want) {
				t.Fatalf("components %+v", g.Components)
			}
			for i, c := range g.Components {
				if c.AmountRaw != want[i] || c.Kind != kinds[i] || c.Coin.Symbol == "" {
					t.Fatalf("component %d: %+v", i, c)
				}
			}
			if g.MarketID != naviAddress("0x21") || g.ID != protocol+":"+naviAddress("0x22") || g.Metadata["stateMode"] != "latest" || g.Metadata["headBeforeRead"] != "100" || g.Metadata["headAfterRead"] != "100" {
				t.Fatalf("identity %+v", g)
			}
		})
	}
}

func TestSuiCLMMEmptyAndTransferredPositions(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			f, owner := clmmFixture(protocol)
			o := f.objects[naviAddress("0x22")]
			o.Owner = naviAddress("0x12")
			f.objects[o.ID] = o
			f.failObjects = true // Empty owners need neither pools nor dynamic fields.
			got, err := NewSuiProtocolReader().ReadLatest(context.Background(), protocol, owner, f)
			if err != nil || len(got.Errors) != 0 || len(got.Groups) != 0 {
				t.Fatalf("transferred position %+v %v", got, err)
			}
		})
	}
}

func TestSuiCLMMZeroLiquidityRetainsOwedWithoutTicks(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			f, owner := clmmFixture(protocol)
			editSuilendObject(f, naviAddress("0x22"), func(fields suiFields) { fields["liquidity"] = "0" })
			pkg := bluefinCLMMPackage
			if protocol == "cetus" {
				pkg = cetusCLMMPackage
				address, _ := ParseSuiAddress("0x22")
				id, _ := suiCLMMFieldID(naviAddress("0x24"), "0x2::object::ID", address[:])
				editSuilendObject(f, id, func(fields suiFields) { v, _ := fields.object("value"); v, _ = v.object("value"); v["liquidity"] = "0" })
			}
			for _, tick := range []int32{-100, 100} {
				id, _, _, _ := suiCLMMTickKey(naviAddress("0x23"), tick, pkg)
				delete(f.objects, id)
			}
			got, err := NewSuiProtocolReader().ReadLatest(context.Background(), protocol, owner, f)
			if err != nil || len(got.Errors) != 0 || len(got.Groups) != 1 {
				t.Fatalf("zero liquidity %+v %v", got, err)
			}
			for i, want := range []string{"5", "9", "7"} {
				if got.Groups[0].Components[i].AmountRaw != want {
					t.Fatalf("owed %+v", got.Groups[0])
				}
			}
		})
	}
}

func TestSuiCLMMRejectsIncompleteState(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		for _, failure := range []string{"missing pool", "wrong pool coins", "wrong field owner", "missing tick", "wrong tick key", "missing growth", "missing metadata", "wide integer", "bad NFT identity"} {
			t.Run(protocol+"/"+failure, func(t *testing.T) {
				f, owner := clmmFixture(protocol)
				pool, pos := naviAddress("0x21"), naviAddress("0x22")
				pkg := cetusCLMMPackage
				if protocol == "bluefin" {
					pkg = bluefinCLMMPackage
				}
				tick, _, _, _ := suiCLMMTickKey(naviAddress("0x23"), -100, pkg)
				switch failure {
				case "missing pool":
					delete(f.objects, pool)
				case "wrong pool coins":
					o := f.objects[pool]
					o.ObjectType = suiType(pkg + "::pool::Pool<0x2::sui::SUI,0x4::fake::FAKE>")
					f.objects[pool] = o
				case "wrong field owner":
					o := f.objects[tick]
					o.Owner = naviAddress("0x99")
					f.objects[tick] = o
				case "missing tick":
					delete(f.objects, tick)
				case "wrong tick key":
					editSuilendObject(f, tick, func(fields suiFields) { fields["name"] = "0" })
				case "missing growth":
					editSuilendObject(f, pool, func(fields suiFields) {
						delete(fields, "fee_growth_global_a")
						delete(fields, "fee_growth_global_coin_a")
					})
				case "missing metadata":
					delete(f.metadata, suiLongType)
				case "wide integer":
					editSuilendObject(f, pos, func(fields suiFields) { fields["liquidity"] = suiCLMMMod128.String() })
				case "bad NFT identity":
					editSuilendObject(f, pos, func(fields suiFields) { fields["id"] = naviAddress("0x33") })
				}
				got, err := NewSuiProtocolReader().ReadLatest(context.Background(), protocol, owner, f)
				if err != nil || len(got.Errors) == 0 || len(got.Groups) != 0 {
					t.Fatalf("accepted %s: %+v %v", failure, got, err)
				}
			})
		}
	}
}

func TestSuiCLMMRewardEndAndHeadSkew(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			f, owner := clmmFixture(protocol)
			before, after := f.pin, f.pin
			after.Sequence--
			after.Timestamp = time.Unix(980, 0)
			r := &latestHeadFixture{latestSuiFixture: f, heads: []SuiCheckpoint{before, after}}
			got, err := NewSuiProtocolReader().ReadLatest(context.Background(), protocol, owner, r)
			if err != nil || len(got.Errors) != 0 || got.Groups[0].Components[4].AmountRaw != "1307" || got.Checkpoint != after || got.HeadBeforeRead != before.Sequence || r.headReads != 2 {
				t.Fatalf("skew %+v %v", got, err)
			}
		})
	}
	f, owner := clmmFixture("bluefin")
	editSuilendObject(f, naviAddress("0x21"), func(fields suiFields) {
		rs := fields["reward_infos"].([]any)
		rs[0].(map[string]any)["ended_at_seconds"] = "995"
	})
	got, err := NewSuiProtocolReader().ReadLatest(context.Background(), "bluefin", owner, f)
	if err != nil || len(got.Errors) != 0 || got.Groups[0].Components[4].AmountRaw != "1807" {
		t.Fatalf("expired rewards %+v %v", got, err)
	}
}

func TestSuiCLMMMathRangesAndWrapping(t *testing.T) {
	lo := new(big.Int).Div(new(big.Int).Set(suiCLMMQ64), big.NewInt(2))
	hi := new(big.Int).Mul(suiCLMMQ64, big.NewInt(2))
	for _, tc := range []struct {
		numerator, denominator int64
		a, b                   string
	}{{1, 4, "150", "0"}, {1, 2, "150", "0"}, {1, 1, "50", "50"}, {2, 1, "0", "150"}, {4, 1, "0", "150"}} {
		price := new(big.Int).Div(new(big.Int).Mul(suiCLMMQ64, big.NewInt(tc.numerator)), big.NewInt(tc.denominator))
		a, b := suiCLMMAmounts(big.NewInt(100), price, lo, hi)
		if a.String() != tc.a || b.String() != tc.b {
			t.Fatalf("range %+v => %s/%s", tc, a, b)
		}
	}
	inside := suiCLMMInside(big.NewInt(10), big.NewInt(10), big.NewInt(7), 0, -100, 100)
	lastAtWrap := new(big.Int).Sub(suiCLMMMod128, big.NewInt(7))
	zero, zeroErr := suiCLMMOwed(new(big.Int), lastAtWrap, inside, big.NewInt(100))
	if zeroErr != nil || zero.Sign() != 0 {
		t.Fatalf("unchanged wrapped growth is not zero: %s %v", zero, zeroErr)
	}
	last := new(big.Int).Sub(suiCLMMMod128, suiCLMMQ64)
	owed, err := suiCLMMOwed(big.NewInt(5), last, suiCLMMQ64, big.NewInt(100))
	if err != nil || owed.String() != "205" {
		t.Fatalf("wrap %s %v", owed, err)
	}
	_, err = suiCLMMOwed(new(big.Int).SetUint64(^uint64(0)), new(big.Int), suiCLMMQ64, big.NewInt(1))
	if err == nil {
		t.Fatal("overflow accepted")
	}
	if suiCLMMInside(big.NewInt(10), big.NewInt(2), big.NewInt(3), -101, -100, 100).Cmp(suiCLMMSub(big.NewInt(2), big.NewInt(3))) != 0 {
		t.Fatal("below range growth")
	}
	if suiCLMMInside(big.NewInt(10), big.NewInt(2), big.NewInt(3), 100, -100, 100).String() != "1" {
		t.Fatal("upper boundary growth")
	}
}

func TestSuiCLMMDynamicFieldKeys(t *testing.T) {
	// Independently queried public chain fields, not account balance fixtures.
	id, err := suiCLMMFieldID("0x78e33a2c94b362a16085ec5ff667f952c47e092f9a9d301a8f5399eff18b81a8", "u64", binary.LittleEndian.AppendUint64(nil, 516156))
	if err != nil || id != "0x8f545090f0b01a93159a9fc9248b33369b9bb83c08b435764889154f69574f92" {
		t.Fatalf("u64 %s %v", id, err)
	}
	id, err = suiCLMMFieldID("0x71feb21d6d89c89922822c500d69241777144ab7488c4b69ee2f9339783e33f2", suiCLMMI32, binary.LittleEndian.AppendUint32(nil, 73800))
	if err != nil || id != "0xc8b34e7176a9d0fe5bbb0b5af4b11fb1e805e467b72ac90a6ccf6af514c000f3" {
		t.Fatalf("I32 %s %v", id, err)
	}
	addr, _ := ParseSuiAddress("0xd01cb2c439ac3f7448e455d33be2e1638ef8b2bb4b8b3a6a211054b8a72236de")
	id, err = suiCLMMFieldID("0x870fd94941dfe64fc8f57fbcf01e6fe9f3247100e268be35765cb4c032356d9e", "0x2::object::ID", addr[:])
	if err != nil || id != "0x410cef7c849dd890f17fe980d61012b56cc35d8532104356f3c3744598313022" {
		t.Fatalf("ID %s %v", id, err)
	}
}

func TestSuiCLMMNormalizedNestedCoins(t *testing.T) {
	f := suiFields{"coin": map[string]any{"name": "2::sui::SUI"}}
	parser := &suiCLMMParser{}
	coin := parser.coin(f, "coin")
	if parser.err != nil || coin != suiLongType {
		t.Fatalf("TypeName %s %v", coin, parser.err)
	}
	raw := strings.ReplaceAll(`{"coin":{"type":"0x1::type_name::TypeName","fields":{"name":"2::sui::SUI"}}}`, " ", "")
	fields, err := suiObjectFields(raw)
	if err != nil || parser.coin(fields, "coin") != suiLongType {
		t.Fatal("wrapped TypeName")
	}
}

func TestSuiCLMMKeepsSeparateNFTsInOnePool(t *testing.T) {
	for _, protocol := range []string{"cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			f, owner := clmmFixture(protocol)
			oldID, newID := naviAddress("0x22"), naviAddress("0x25")
			o := f.objects[oldID]
			fields, _ := suiObjectFields(o.Content)
			f.objects[newID] = latestObject(newID, o.ObjectType, "ADDRESS", owner.Hex(), fields)
			if protocol == "cetus" {
				oldAddress, _ := ParseSuiAddress(oldID)
				newAddress, _ := ParseSuiAddress(newID)
				oldField, _ := suiCLMMFieldID(naviAddress("0x24"), "0x2::object::ID", oldAddress[:])
				newField, _ := suiCLMMFieldID(naviAddress("0x24"), "0x2::object::ID", newAddress[:])
				o = f.objects[oldField]
				fields, _ = suiObjectFields(o.Content)
				fields["name"] = newID
				value, _ := fields.object("value")
				value, _ = value.object("value")
				value["position_id"] = newID
				f.objects[newField] = latestObject(newField, o.ObjectType, "OBJECT", o.Owner, fields)
			}
			got, err := NewSuiProtocolReader().ReadLatest(context.Background(), protocol, owner, f)
			if err != nil || len(got.Errors) != 0 || len(got.Groups) != 2 {
				t.Fatalf("separate positions %+v %v", got, err)
			}
			for i, id := range []string{oldID, newID} {
				g := got.Groups[i]
				if g.ID != protocol+":"+id || len(g.Components) != 5 || g.Components[0].AmountRaw != "50" {
					t.Fatalf("position %d: %+v", i, g)
				}
			}
		})
	}
}
