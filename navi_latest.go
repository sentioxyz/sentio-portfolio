package portfolio

import (
	"encoding/binary"
	"fmt"
	"strconv"

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
