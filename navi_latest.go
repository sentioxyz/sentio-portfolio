package portfolio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/blake2b"
)

// Defining types and the main Storage are protocol trust anchors. Markets,
// reserve handles, capabilities and vault addresses are discovered at runtime.
const (
	naviStorageType   = "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::Storage"
	naviMainStorage   = "0xbb4e2f4b6205c2e2a2db47aeb4f830796ec7c005f88537ee775986639bc442fe"
	naviAccountType   = "0x66c91a8560cd64d73d93dd1ec7b61f3f21ad2f66553dd3d7038ca69255479bb7::account::AccountCap"
	naviVaultPackage  = "0x51cecaacaed0bd436f04ebbd8ba0ca1627c9c4d0e54ad28eff095ca78591518c"
	voloVaultPackage  = "0xcd86f77503a755c48fe6c87e1b8e9a137ec0c1bf37aac8878b6083262b27fefa"
	naviMarketKeyType = "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::MARKET_KEY"
	naviEmodeKeyType  = "0x1e4a13a0494d5facdbe8473e74127b838c2d446ecec0ce262e2eddafa77259cb::storage::EMODE_KEY"
)

func suiAddressFieldID(parent, account string) (string, error) {
	p, err := ParseSuiAddress(parent)
	if err != nil {
		return "", err
	}
	a, err := ParseSuiAddress(account)
	if err != nil {
		return "", err
	}
	// Sui hash_type_and_key: domain 0xf0, parent, BCS u64 key length,
	// BCS key, BCS TypeTag::Address (variant 4).
	data := []byte{0xf0}
	data = append(data, p[:]...)
	data = binary.LittleEndian.AppendUint64(data, 32)
	data = append(data, a[:]...)
	data = append(data, 4)
	hash := blake2b.Sum256(data)
	return SuiAddress(hash).Hex(), nil
}

func protocolObject(object SuiObject, kind, key string) suiProtocolObject {
	return suiProtocolObject{ID: object.ID, Kind: kind, Owner: object.Owner, Parent: object.Owner, Key: key, ObjectType: object.ObjectType, Content: object.Content, Version: strconv.FormatUint(object.Version, 10)}
}

func suiTableID(fields suiFields, name string) (string, error) {
	table, err := fields.object(name)
	if err != nil {
		return "", err
	}
	return table.address("id")
}

func suiType(raw string) string { typ, _ := NormalizeMoveType(raw); return typ }

func suiFieldType(key, value string) string {
	return suiType("0x2::dynamic_field::Field<" + key + "," + value + ">")
}

func suiFieldHasKey(object SuiObject, key string) bool {
	return strings.HasPrefix(object.ObjectType, suiType("0x2::dynamic_field::Field")+"<"+suiType(key)+",")
}

// smallSuiTables bounds parallel enumeration of reserve/NAV inventories. It is
// never used for per-user tables, whose size grows with the protocol's users.
func smallSuiTables(ctx context.Context, reader SuiObjectReader, ids []string) (map[string][]SuiObject, error) {
	unique := map[string]bool{}
	for _, id := range ids {
		unique[id] = true
	}
	keys := make([]string, 0, len(unique))
	for id := range unique {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	if len(keys) > suiObjectLimit {
		return nil, fmt.Errorf("too many protocol tables")
	}
	results := make([][]SuiObject, len(keys))
	errs := make([]error, len(keys))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < min(8, len(keys)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i], errs[i] = reader.DynamicFields(ctx, keys[i])
			}
		}()
	}
	for i := range keys {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	output := make(map[string][]SuiObject, len(keys))
	for i, id := range keys {
		if errs[i] != nil {
			return nil, errs[i]
		}
		output[id] = results[i]
	}
	return output, nil
}

func (r *SuiProtocolReader) loadLatest(ctx context.Context, protocolID string, owner SuiAddress, reader SuiObjectReader) (suiProtocolState, error, error) {
	state := suiProtocolState{}
	var lendingErr error
	if protocolID == "navi" {
		caps, err := reader.OwnedObjects(ctx, owner, naviAccountType)
		lendingErr = err
		if err == nil {
			for _, cap := range caps {
				state.Owned = append(state.Owned, protocolObject(cap, "account", ""))
			}
			lendingErr = r.loadNavi(ctx, owner, reader, &state)
		}
	}
	vaultErr := r.loadVaults(ctx, protocolID, owner, reader, &state)
	return state, lendingErr, vaultErr
}

func (r *SuiProtocolReader) loadNavi(ctx context.Context, owner SuiAddress, reader SuiObjectReader, state *suiProtocolState) error {
	ids, err := r.discoverNaviMarkets(ctx, reader)
	if err != nil {
		return err
	}
	storages, err := reader.Objects(ctx, ids)
	if err != nil {
		return err
	}
	if len(storages) != len(ids) || storages[naviMainStorage].ID == "" {
		return fmt.Errorf("NAVI market inventory is incomplete")
	}
	children, err := smallSuiTables(ctx, reader, ids)
	if err != nil {
		return err
	}
	reserveParents := []string{}
	reserveCounts := map[string]uint64{}
	marketIDs := map[uint64]bool{}
	emodeTables := map[string]string{}
	var lastMarket *uint64
	for _, id := range ids {
		storage := storages[id]
		if storage.ObjectType != suiType(naviStorageType) || storage.OwnerKind != "SHARED" {
			return fmt.Errorf("invalid NAVI Storage candidate")
		}
		fields, err := suiObjectFields(storage.Content)
		if err != nil {
			return err
		}
		parent, err := suiTableID(fields, "reserves")
		if err != nil {
			return err
		}
		count, err := fields.uint("reserves_count")
		if err != nil || !count.IsUint64() || count.Uint64() > 256 {
			return fmt.Errorf("invalid NAVI reserve count")
		}
		if _, exists := reserveCounts[parent]; exists {
			return fmt.Errorf("NAVI markets share reserves")
		}
		reserveCounts[parent] = count.Uint64()
		reserveParents = append(reserveParents, parent)
		state.Topology = append(state.Topology, protocolObject(storage, "storage", ""))
		market := ""
		emodeTable := ""
		for _, child := range children[id] {
			if !suiFieldHasKey(child, naviMarketKeyType) && !suiFieldHasKey(child, naviEmodeKeyType) {
				continue
			}
			f, err := suiObjectFields(child.Content)
			if err != nil {
				return err
			}
			value, err := f.object("value")
			if err != nil {
				return err
			}
			if suiFieldHasKey(child, naviMarketKeyType) {
				number, err := value.uint("market_id")
				if err != nil || !number.IsUint64() || market != "" || marketIDs[number.Uint64()] {
					return fmt.Errorf("ambiguous NAVI market identity")
				}
				market = number.String()
				marketIDs[number.Uint64()] = true
				main, ok := value["is_main_market"].(bool)
				if !ok || main != (id == naviMainStorage) || main != (number.Sign() == 0) {
					return fmt.Errorf("NAVI main market identity mismatch")
				}
				if main {
					last, err := value.uint("last_market_id")
					if err != nil || !last.IsUint64() {
						return fmt.Errorf("invalid NAVI market inventory size")
					}
					n := last.Uint64()
					lastMarket = &n
				}
				state.Topology = append(state.Topology, protocolObject(child, "market", market))
			} else {
				if emodeTable != "" {
					return fmt.Errorf("ambiguous NAVI e-mode table")
				}
				emodeTable, err = suiTableID(value, "user_emode_id")
				if err != nil {
					return err
				}
			}
		}
		if market == "" {
			return fmt.Errorf("NAVI Storage has no market metadata")
		}
		if emodeTable != "" {
			emodeTables[emodeTable] = market
		}
	}
	if lastMarket == nil || *lastMarket >= suiObjectLimit || uint64(len(marketIDs)) != *lastMarket+1 {
		return fmt.Errorf("NAVI directory does not cover the on-chain market inventory")
	}
	for id := uint64(0); id <= *lastMarket; id++ {
		if !marketIDs[id] {
			return fmt.Errorf("NAVI directory omitted market %d", id)
		}
	}
	reserves, err := smallSuiTables(ctx, reader, reserveParents)
	if err != nil {
		return err
	}
	for _, parent := range reserveParents {
		if uint64(len(reserves[parent])) != reserveCounts[parent] {
			return fmt.Errorf("NAVI reserve inventory changed or is incomplete")
		}
		for _, object := range reserves[parent] {
			if object.ObjectType != suiFieldType("u8", "0xd899cf7d2b5db716bd2cf55599fb0d5ee38a3061e7b6bb6eebf73fa5bc4c81ca::storage::ReserveData") {
				return fmt.Errorf("invalid NAVI reserve field type")
			}
			fields, err := suiObjectFields(object.Content)
			if err != nil {
				return err
			}
			key, err := fields.uint("name")
			if err != nil || !key.IsUint64() || key.Uint64() >= reserveCounts[parent] {
				return fmt.Errorf("invalid NAVI reserve inventory key")
			}
			state.Topology = append(state.Topology, protocolObject(object, "reserve", key.String()))
		}
	}
	tables, err := naviBalanceTables(state.Topology)
	if err != nil {
		return err
	}
	accounts := map[string]bool{owner.Hex(): true}
	for _, object := range state.Owned {
		if object.Kind != "account" {
			continue
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return err
		}
		account, err := fields.address("owner")
		if err != nil {
			return err
		}
		accounts[account] = true
	}
	type target struct {
		parent, account, market string
		emode                   bool
	}
	targets := map[string]target{}
	for account := range accounts {
		for parent := range tables {
			id, err := suiAddressFieldID(parent, account)
			if err != nil {
				return err
			}
			targets[id] = target{parent: parent, account: account}
		}
		for parent, market := range emodeTables {
			id, err := suiAddressFieldID(parent, account)
			if err != nil {
				return err
			}
			targets[id] = target{parent: parent, account: account, market: market, emode: true}
		}
	}
	pointIDs := make([]string, 0, len(targets))
	for id := range targets {
		pointIDs = append(pointIDs, id)
	}
	sort.Strings(pointIDs)
	objects, err := reader.Objects(ctx, pointIDs)
	if err != nil {
		return err
	}
	for _, id := range pointIDs {
		object, exists := objects[id]
		if !exists {
			continue
		}
		target := targets[id]
		if object.OwnerKind != "OBJECT" || object.Owner != target.parent {
			return fmt.Errorf("NAVI balance belongs to another table")
		}
		valueType := "u256"
		if target.emode {
			valueType = "u64"
		}
		if object.ObjectType != suiFieldType("address", valueType) {
			return fmt.Errorf("invalid NAVI balance field type")
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return err
		}
		key, err := fields.address("name")
		if err != nil || key != target.account {
			return fmt.Errorf("NAVI balance key mismatch")
		}
		if _, err := fields.uint("value"); err != nil {
			return err
		}
		if target.emode {
			state.Emodes = append(state.Emodes, suiProtocolValue{ID: "emode:" + target.market + ":" + key, Kind: "emode", Account: key, Market: target.market, Content: `{"entered":true}`})
		} else {
			row := protocolObject(object, "principal", key)
			row.Owner = key
			state.Principals = append(state.Principals, row)
		}
	}
	return nil
}

func (r *SuiProtocolReader) loadVaults(ctx context.Context, protocolID string, owner SuiAddress, reader SuiObjectReader, state *suiProtocolState) error {
	packageID, module, tableName, stateType := naviVaultPackage, "navi_vault", "user_states", naviVaultPackage+"::navi_vault::UserState"
	receiptType := packageID + "::navi_vault::Receipt"
	if protocolID == "volo-vaults" {
		packageID, module, tableName, stateType = voloVaultPackage, "vault", "receipts", voloVaultPackage+"::vault_receipt_info::VaultReceiptInfo"
		receiptType = packageID + "::receipt::Receipt"
	}
	receipts, err := reader.OwnedObjects(ctx, owner, receiptType)
	if err != nil {
		return err
	}
	if len(receipts) == 0 {
		return nil
	}
	vaultIDs := []string{}
	byVault := map[string][]string{}
	for _, receipt := range receipts {
		fields, err := suiObjectFields(receipt.Content)
		if err != nil {
			return err
		}
		id, err := suiReceiptVault(fields, protocolID)
		if err != nil {
			return err
		}
		if _, ok := byVault[id]; !ok {
			vaultIDs = append(vaultIDs, id)
		}
		byVault[id] = append(byVault[id], receipt.ID)
		state.Owned = append(state.Owned, protocolObject(receipt, "receipt", id))
	}
	sort.Strings(vaultIDs)
	vaults, err := reader.Objects(ctx, vaultIDs)
	if err != nil {
		return err
	}
	if len(vaults) != len(vaultIDs) {
		return fmt.Errorf("owned receipt's vault is unavailable")
	}
	type target struct{ parent, key string }
	targets := map[string]target{}
	for _, id := range vaultIDs {
		vault := vaults[id]
		if vault.OwnerKind != "SHARED" || !suiObjectTypeMatches(vault.ObjectType, suiType(packageID+"::"+module+"::Vault")) {
			return fmt.Errorf("invalid vault object type or ownership")
		}
		fields, err := suiObjectFields(vault.Content)
		if err != nil {
			return err
		}
		parent, err := suiTableID(fields, tableName)
		if err != nil {
			return err
		}
		for _, receipt := range byVault[id] {
			fieldID, err := suiAddressFieldID(parent, receipt)
			if err != nil {
				return err
			}
			targets[fieldID] = target{parent, receipt}
		}
		state.Vaults = append(state.Vaults, protocolObject(vault, "vault", ""))
	}
	ids := []string{}
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	states, err := reader.Objects(ctx, ids)
	if err != nil {
		return err
	}
	// Both vault implementations can leave a fresh or emptied receipt without
	// a state entry. Only the transport's explicit NotFound is treated as empty.
	for _, id := range ids {
		object, exists := states[id]
		if !exists {
			continue
		}
		target := targets[id]
		if object.OwnerKind != "OBJECT" || object.Owner != target.parent || object.ObjectType != suiFieldType("address", stateType) {
			return fmt.Errorf("invalid vault receipt state object")
		}
		state.ReceiptStates = append(state.ReceiptStates, protocolObject(object, "receiptState", target.key))
	}
	if protocolID == "volo-vaults" {
		return r.loadVoloValuation(ctx, reader, state)
	}
	return nil
}

func (r *SuiProtocolReader) loadVoloValuation(ctx context.Context, reader SuiObjectReader, state *suiProtocolState) error {
	active, err := suiActiveShareVaults(*state)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return nil
	}
	ids, err := r.discoverVoloOracle(ctx, reader)
	if err != nil {
		return err
	}
	if len(ids) != 1 {
		return fmt.Errorf("ambiguous or missing Volo oracle configuration")
	}
	oracles, err := reader.Objects(ctx, ids)
	if err != nil {
		return err
	}
	oracle, ok := oracles[ids[0]]
	if !ok || oracle.OwnerKind != "SHARED" || oracle.ObjectType != suiType(voloVaultPackage+"::vault_oracle::OracleConfig") {
		return fmt.Errorf("invalid Volo oracle configuration")
	}
	fields, err := suiObjectFields(oracle.Content)
	if err != nil {
		return err
	}
	oracleParent, err := suiTableID(fields, "aggregators")
	if err != nil {
		return err
	}
	tables := []string{oracleParent}
	type navTables struct{ vault, values, times string }
	nav := []navTables{}
	for _, vault := range state.Vaults {
		if !active[vault.ID] {
			continue
		}
		fields, err := suiObjectFields(vault.Content)
		if err != nil {
			return err
		}
		values, err := suiTableID(fields, "assets_value")
		if err != nil {
			return err
		}
		times, err := suiTableID(fields, "assets_value_updated")
		if err != nil {
			return err
		}
		tables = append(tables, values, times)
		nav = append(nav, navTables{vault.ID, values, times})
	}
	entries, err := smallSuiTables(ctx, reader, tables)
	if err != nil {
		return err
	}
	state.OracleObjects = append(state.OracleObjects, protocolObject(oracle, "oracle", ""))
	for _, object := range entries[oracleParent] {
		if object.ObjectType != suiFieldType("0x1::ascii::String", voloVaultPackage+"::vault_oracle::PriceInfo") {
			return fmt.Errorf("invalid Volo oracle price type")
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return err
		}
		key, err := fields.text("name")
		if err != nil {
			return err
		}
		state.OracleObjects = append(state.OracleObjects, protocolObject(object, "oraclePrice", key))
	}
	for _, n := range nav {
		values, err := suiStringUintTable(entries[n.values], "u256")
		if err != nil {
			return err
		}
		times, err := suiStringUintTable(entries[n.times], "u64")
		if err != nil {
			return err
		}
		if len(values) != len(times) {
			return fmt.Errorf("Volo NAV and timestamp inventories disagree")
		}
		for asset, value := range values {
			timestamp, ok := times[asset]
			if !ok {
				return fmt.Errorf("Volo NAV timestamp is missing")
			}
			content, _ := json.Marshal(map[string]string{"amount": value, "timestamp": timestamp, "asset": asset})
			state.AssetValues = append(state.AssetValues, suiProtocolValue{ID: "assetValue:" + n.vault + ":" + asset, Kind: "assetValue", Account: n.vault, Content: string(content)})
		}
	}
	return nil
}

func suiStringUintTable(objects []SuiObject, valueType string) (map[string]string, error) {
	result := map[string]string{}
	for _, object := range objects {
		if object.ObjectType != suiFieldType("0x1::ascii::String", valueType) {
			return nil, fmt.Errorf("invalid Volo NAV table field type")
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return nil, err
		}
		key, err := fields.text("name")
		if err != nil {
			return nil, err
		}
		value, err := fields.uint("value")
		if err != nil {
			return nil, err
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate Volo NAV table key")
		}
		result[key] = value.String()
	}
	return result, nil
}

func suiReceiptVault(fields suiFields, protocolID string) (string, error) {
	field := "vault_id"
	if protocolID == "navi" {
		field = "vault_address"
	}
	return fields.address(field)
}

// Pending deposits/claimable principal do not depend on the share NAV.
func suiActiveShareVaults(state suiProtocolState) (map[string]bool, error) {
	receipts := map[string]string{}
	for _, row := range state.Owned {
		if row.Kind == "receipt" {
			receipts[row.ID] = row.Key
		}
	}
	active := map[string]bool{}
	for _, row := range state.ReceiptStates {
		fields, err := suiObjectFields(row.Content)
		if err != nil {
			return nil, err
		}
		value, err := fields.object("value")
		if err != nil {
			return nil, err
		}
		shares, err := value.uint("shares")
		if err != nil {
			return nil, err
		}
		if shares.Sign() > 0 {
			vault, ok := receipts[row.Key]
			if !ok {
				return nil, fmt.Errorf("unattributed vault receipt state")
			}
			active[vault] = true
		}
	}
	return active, nil
}
