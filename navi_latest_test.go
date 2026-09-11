package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type latestSuiFixture struct {
	naviReaderFixture
	objects     map[string]SuiObject
	tables      map[string][]SuiObject
	directory   map[string][]string
	failObjects bool
	enumerated  []string
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
func (f *latestSuiFixture) ObjectsByType(_ context.Context, typ string) ([]string, error) {
	return f.directory[typ], nil
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
	return &latestSuiFixture{naviReaderFixture: naviReaderFixture{pin: pin, metadata: map[string]SuiCoinMetadata{suiLongType: {CoinType: suiLongType, Symbol: "SUI", Decimals: 9}}}, objects: map[string]SuiObject{}, tables: map[string][]SuiObject{}, directory: map[string][]string{}}
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
	mode := latestObject(fmt.Sprintf("0x%x", 0x700+number), suiFieldType(naviEmodeKeyType, "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::Emode"), "OBJECT", storage, map[string]any{"value": map[string]any{"user_emode_id": map[string]any{"id": emodes}}})
	f.tables[storage] = []SuiObject{market, mode}
	reserve := latestObject(fmt.Sprintf("0x%x", 0x800+number), suiFieldType("u8", "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::ReserveData"), "OBJECT", reserves, map[string]any{"name": 0, "value": map[string]any{"id": 0, "coin_type": "2::sui::SUI", "current_supply_index": naviRay.String(), "current_supply_rate": "0", "last_update_timestamp": "1000000", "supply_balance": map[string]any{"user_state": map[string]any{"id": supply}}, "borrow_balance": map[string]any{"user_state": map[string]any{"id": borrow}}}})
	f.tables[reserves] = []SuiObject{reserve}
	f.directory[naviStorageType] = append(f.directory[naviStorageType], storage)
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
func TestNaviLatestDiscoversNewMarketAndDeduplicatesChildCaps(t *testing.T) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	f.addMarket(0, 1, owner.Hex(), "1000000000", false)
	f.addMarket(1, 1, "0x22", "2000000000", true)
	for _, id := range []string{"0x22", "0x23"} {
		o := latestObject(id, naviAccountType, "ADDRESS", owner.Hex(), map[string]any{"owner": naviAddress("0x22")})
		f.objects[o.ID] = o
	}
	r := NewSuiProtocolReader(f)
	got, err := r.ReadLatest(context.Background(), "navi", owner, f)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if got.Groups[1].Label != "Multiply" || got.Groups[1].Components[0].AmountRaw != "2000000000" {
		t.Fatalf("cap attribution %+v", got.Groups)
	}
	if got.HeadBeforeRead != 100 || got.Groups[0].Metadata["stateMode"] != "latest" {
		t.Fatal("missing latest window")
	}
	// The wallet holds two capabilities for one logical account. Transferring
	// both away must remove its Multiply position without changing its balances.
	for _, id := range []string{"0x22", "0x23"} {
		o := f.objects[naviAddress(id)]
		o.Owner = naviAddress("0x99")
		f.objects[o.ID] = o
	}
	got, err = r.ReadLatest(context.Background(), "navi", owner, f)
	if err != nil || len(got.Errors) > 0 || len(got.Groups) != 1 {
		t.Fatalf("transferred %+v %v", got, err)
	}
}
func TestNaviLatestRejectsIncompleteMarketDirectory(t *testing.T) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	f.addMarket(0, 1, owner.Hex(), "1", false)
	got, err := NewSuiProtocolReader(f).ReadLatest(context.Background(), "navi", owner, f)
	if err != nil || len(got.Errors) != 1 || !strings.Contains(got.Errors[0].Error(), "market inventory") || len(got.Groups) != 0 {
		t.Fatalf("incomplete %+v %v", got, err)
	}
}
func TestNaviLatestRejectsMissingReserve(t *testing.T) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	f.addMarket(0, 0, owner.Hex(), "1", false)
	f.tables[naviAddress("0x200")] = nil
	got, err := NewSuiProtocolReader(f).ReadLatest(context.Background(), "navi", owner, f)
	if err != nil || len(got.Errors) != 1 || len(got.Groups) != 0 {
		t.Fatalf("reserve gap %+v %v", got, err)
	}
}
func TestNaviLatestPointReadFailureIsNotZero(t *testing.T) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	f.addMarket(0, 0, owner.Hex(), "1", false)
	f.failObjects = true
	got, err := NewSuiProtocolReader(f).ReadLatest(context.Background(), "navi", owner, f)
	if err != nil || len(got.Errors) != 1 || len(got.Groups) != 0 {
		t.Fatalf("point read gap %+v %v", got, err)
	}
}
func TestLatestVaultReceiptFindsUnlistedVaultAndEmptyState(t *testing.T) {
	f := latestFixture()
	owner, _ := ParseSuiAddress("0x11")
	receipt, vault, parent := naviAddress("0x22"), naviAddress("0x33"), naviAddress("0x44")
	f.objects[receipt] = latestObject(receipt, naviVaultPackage+"::navi_vault::Receipt", "ADDRESS", owner.Hex(), map[string]any{"vault_address": vault})
	f.objects[vault] = latestObject(vault, naviVaultPackage+"::navi_vault::Vault<0x2::sui::SUI>", "SHARED", "", map[string]any{"total_shares": "3", "total_assets": "1000000001", "user_states": map[string]any{"id": parent}})
	r := NewSuiProtocolReader(f)
	state := suiProtocolState{}
	if err := r.loadVaults(context.Background(), "navi", owner, f, &state); err != nil {
		t.Fatal(err)
	}
	groups, err := suiVaults(context.Background(), "navi", f.pin, f, state)
	if err != nil || len(groups) != 0 {
		t.Fatalf("fresh receipt %v %v", groups, err)
	}
	id, _ := suiAddressFieldID(parent, receipt)
	f.objects[id] = latestObject(id, suiFieldType("address", naviVaultPackage+"::navi_vault::UserState"), "OBJECT", parent, map[string]any{"name": receipt, "value": map[string]any{"shares": "2"}})
	state = suiProtocolState{}
	if err := r.loadVaults(context.Background(), "navi", owner, f, &state); err != nil {
		t.Fatal(err)
	}
	groups, err = suiVaults(context.Background(), "navi", f.pin, f, state)
	if err != nil || len(groups) != 1 || groups[0].Components[0].AmountRaw != "666666667" {
		t.Fatalf("vault %v %v", groups, err)
	}
}
