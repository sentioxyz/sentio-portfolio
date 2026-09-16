package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

type SuiObjectChange struct {
	ID, ObjectType, OwnerKind   string
	InputVersion, OutputVersion uint64
	Created                     bool
}

type latestSuiFixture struct {
	naviReaderFixture
	objects          map[string]SuiObject
	tables           map[string][]SuiObject
	transactions     map[string][]SuiObjectChange
	versions         map[uint64]SuiObject
	transactionReads int
	failObjects      bool
	enumerated       []string
}

func (f *latestSuiFixture) Objects(_ context.Context, ids []string) (map[string]SuiObject, error) {
	if f.failObjects {
		return nil, errors.New("unavailable")
	}
	result := map[string]SuiObject{}
	for _, id := range ids {
		if o, ok := f.objects[id]; ok {
			result[id] = o
		}
	}
	return result, nil
}
func (f *latestSuiFixture) OwnedObjects(_ context.Context, owner SuiAddress, typ string) ([]SuiObject, error) {
	result := []SuiObject{}
	for _, o := range f.objects {
		if o.Owner == owner.Hex() && o.OwnerKind == "ADDRESS" && suiObjectTypeMatches(o.ObjectType, suiType(typ)) {
			result = append(result, o)
		}
	}
	return result, nil
}
func (f *latestSuiFixture) DynamicFields(_ context.Context, id string) ([]SuiObject, error) {
	// Only explicitly seeded topology tables may be enumerated. In particular,
	// balance tables must be accessed by field ID, even for many markets.
	rows, ok := f.tables[id]
	if !ok {
		return nil, fmt.Errorf("unexpected full-table enumeration %s", id)
	}
	return rows, nil
}
func latestObject(id, typ, kind, owner string, fields map[string]any) SuiObject {
	id = naviAddress(id)
	fields["id"] = id
	raw, _ := json.Marshal(fields)
	if owner != "" {
		owner = naviAddress(owner)
	}
	return SuiObject{ID: id, ObjectType: suiType(typ), OwnerKind: kind, Owner: owner, Content: string(raw), Version: 1}
}
func latestFixture() *latestSuiFixture {
	pin, _ := newSuiCheckpoint(100, suiTestDigest, time.Unix(1000, 0))
	return &latestSuiFixture{naviReaderFixture: naviReaderFixture{pin: pin, metadata: map[string]SuiCoinMetadata{suiLongType: {CoinType: suiLongType, Symbol: "SUI", Decimals: 9}}}, objects: map[string]SuiObject{}, tables: map[string][]SuiObject{}, transactions: map[string][]SuiObjectChange{}, versions: map[uint64]SuiObject{}}
}

type latestHeadFixture struct {
	*latestSuiFixture
	heads     []SuiCheckpoint
	headReads int
}

func (f *latestHeadFixture) LatestCheckpoint(context.Context) (SuiCheckpoint, error) {
	if f.headReads >= len(f.heads) {
		return SuiCheckpoint{}, errors.New("unexpected additional head read")
	}
	head := f.heads[f.headReads]
	f.headReads++
	return head, nil
}

func TestSuilendIndexedLatestIgnoresNodeHeads(t *testing.T) {
	f, owner := suilendFixture()
	fixture, index := newSuilendIndexFixture(t, f)
	node := &latestHeadFixture{latestSuiFixture: f}
	got, err := NewSuiProtocolReader().WithSuilendHistory(index).ReadLatest(context.Background(), "suilend", owner, node)
	if err != nil || len(got.Groups) != 1 || node.headReads != 0 || got.Checkpoint != fixture.pin {
		t.Fatalf("index-only latest %+v %v, node reads %d", got, err, node.headReads)
	}
}

func (f *latestSuiFixture) addMarket(number, last int, account string, amount string, emode bool) {
	storage := naviAddress(fmt.Sprintf("0x%x", 0x100+number))
	if number == 0 {
		storage = naviMainStorage
	}
	reserves := naviAddress(fmt.Sprintf("0x%x", 0x200+number))
	supply := naviAddress(fmt.Sprintf("0x%x", 0x300+number))
	borrow := naviAddress(fmt.Sprintf("0x%x", 0x400+number))
	emodes := naviAddress(fmt.Sprintf("0x%x", 0x500+number))
	f.objects[storage] = latestObject(storage, naviStorageType, "SHARED", "", map[string]any{"reserves_count": 1, "reserves": map[string]any{"id": reserves}})
	market := latestObject(fmt.Sprintf("0x%x", 0x600+number), suiFieldType(naviMarketKeyType, "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::MarketInfo"), "OBJECT", storage, map[string]any{"value": map[string]any{"market_id": number, "last_market_id": last, "is_main_market": number == 0}})
	if number == 0 && last > 0 {
		initial := latestObject(market.ID, market.ObjectType, "OBJECT", storage, map[string]any{"value": map[string]any{"market_id": 0, "last_market_id": 0, "is_main_market": true}})
		f.versions[1] = initial
		market.Version = 2
		market.PreviousTransaction = suiTestDigest
		f.transactions[suiTestDigest] = []SuiObjectChange{{ID: market.ID, InputVersion: 1, OutputVersion: 2}}
	}
	if number > 0 {
		f.transactions[suiTestDigest] = append(f.transactions[suiTestDigest], SuiObjectChange{ID: storage, ObjectType: suiType(naviStorageType), OwnerKind: "SHARED", Created: true, OutputVersion: 2})
	}
	mode := latestObject(fmt.Sprintf("0x%x", 0x700+number), suiFieldType(naviEmodeKeyType, "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::Emode"), "OBJECT", storage, map[string]any{"value": map[string]any{"user_emode_id": map[string]any{"id": emodes}}})
	f.tables[storage] = []SuiObject{market, mode}
	reserve := latestObject(fmt.Sprintf("0x%x", 0x800+number), suiFieldType("u8", "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::ReserveData"), "OBJECT", reserves, map[string]any{"name": 0, "value": map[string]any{"id": 0, "coin_type": "2::sui::SUI", "current_supply_index": naviRay.String(), "current_supply_rate": "0", "last_update_timestamp": "1000000", "supply_balance": map[string]any{"user_state": map[string]any{"id": supply}}, "borrow_balance": map[string]any{"user_state": map[string]any{"id": borrow}}}})
	f.tables[reserves] = []SuiObject{reserve}
	if amount != "" {
		id, _ := suiAddressFieldID(supply, account)
		f.objects[id] = latestObject(id, suiFieldType("address", "u256"), "OBJECT", supply, map[string]any{"name": naviAddress(account), "value": amount})
	}
	if emode {
		id, _ := suiAddressFieldID(emodes, account)
		f.objects[id] = latestObject(id, suiFieldType("address", "u64"), "OBJECT", emodes, map[string]any{"name": naviAddress(account), "value": "9"})
	}
}
func TestSuiDynamicFieldIDMatchesOfficialSDK(t *testing.T) {
	got, err := suiAddressFieldID("0x33", "0x22")
	if err != nil || got != "0x1a44968106da4aac6383aec32dd998bea05e318813b3fe15a0017e50f86c122b" {
		t.Fatalf("%s %v", got, err)
	}
}
