package portfolio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

func voloHistoryType(kind string) string {
	switch kind {
	case "vault":
		return suiType(voloVaultPackage + "::vault::Vault")
	case "receipt":
		return suiType(voloVaultPackage + "::receipt::Receipt")
	case "receiptState":
		return suiFieldType("address", voloVaultPackage+"::vault_receipt_info::VaultReceiptInfo")
	case "oracle":
		return suiType(voloVaultPackage + "::vault_oracle::OracleConfig")
	case "oraclePrice":
		return suiFieldType("0x1::ascii::String", voloVaultPackage+"::vault_oracle::PriceInfo")
	case "navValue":
		return suiFieldType("0x1::ascii::String", "u256")
	case "navTimestamp":
		return suiFieldType("0x1::ascii::String", "u64")
	}
	return ""
}

func suiASCIIFieldID(parent, key string) (string, error) {
	if len(key) == 0 || len(key) > 1024 {
		return "", fmt.Errorf("invalid ASCII table key")
	}
	for _, b := range []byte(key) {
		if b > 127 {
			return "", fmt.Errorf("invalid ASCII table key")
		}
	}
	// ascii::String contains a BCS vector<u8>, including its ULEB128 length.
	encoded := binary.AppendUvarint(nil, uint64(len(key)))
	return suiCLMMFieldID(parent, "0x1::ascii::String", append(encoded, []byte(key)...))
}

func sortedSuiIDs(set map[string]bool) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *suiHistoryIndex) voloCatalog(ctx context.Context, pin SuiCheckpoint, kind string) (map[string]SuiObject, error) {
	data, ok := ctx.Value(suiSQLDataKey{}).(*suiSQLData)
	if !ok {
		return nil, fmt.Errorf("Sui SQL state is missing")
	}
	ids := []string{}
	for _, row := range data.objects {
		if row.Kind == kind && row.State == "live" {
			ids = append(ids, row.ObjectID)
		}
	}
	return data.Objects(ctx, ids)
}

func (r *suiHistoryIndex) loadVolo(ctx context.Context, owner SuiAddress, pin SuiCheckpoint) (suiProtocolState, error) {
	state := suiProtocolState{}

	var candidates []suiSQLObject
	if data, ok := ctx.Value(suiSQLDataKey{}).(*suiSQLData); ok {
		for _, row := range data.objects {
			if row.Kind == "receipt" && row.State == "live" && row.Owner == owner.Hex() && (row.OwnerKind == "ADDRESS" || row.OwnerKind == "CONSENSUS_ADDRESS") {
				candidates = append(candidates, row)
			}
		}
	} else {
		return state, fmt.Errorf("Sui SQL state is missing")
	}
	if len(candidates) == 0 {
		return state, nil
	}
	// Only held receipts need the oracle dependency. The snapshot certificate
	// establishes empty ownership without checking unrelated protocol objects.
	oracles, err := r.voloCatalog(ctx, pin, "oracle")
	if err != nil {
		return state, err
	}
	if len(oracles) != 1 {
		return state, fmt.Errorf("Volo oracle inventory is incomplete or ambiguous")
	}
	var oracle SuiObject
	for _, object := range oracles {
		oracle = object
	}
	if oracle.OwnerKind != "SHARED" {
		return state, fmt.Errorf("invalid Volo oracle owner")
	}
	state.OracleObjects = append(state.OracleObjects, protocolObject(oracle, "oracle", ""))

	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Kind != "receipt" {
			return state, fmt.Errorf("invalid Volo owner candidate")
		}
		ids = append(ids, candidate.ObjectID)
	}
	owned, refs, err := r.objects(ctx, ids, pin, true)
	if err != nil {
		return state, err
	}
	vaultIDs := map[string]bool{}
	for _, candidate := range candidates {
		if refs[candidate.ObjectID].Kind != "receipt" {
			return state, fmt.Errorf("invalid Volo receipt reference")
		}
		object, ok := owned[candidate.ObjectID]
		if !ok || object.Owner != owner.Hex() || (object.OwnerKind != "ADDRESS" && object.OwnerKind != "CONSENSUS_ADDRESS") {
			continue
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return state, err
		}
		vault, err := suiReceiptVault(fields, "volo-vaults")
		if err != nil {
			return state, err
		}
		vaultIDs[vault] = true
		state.Owned = append(state.Owned, protocolObject(object, "receipt", vault))
	}
	vaults, vaultRefs, err := r.objects(ctx, sortedSuiIDs(vaultIDs), pin, true)
	if err != nil {
		return state, err
	}
	if len(vaults) != len(vaultIDs) {
		return state, fmt.Errorf("Volo receipt vault is unavailable")
	}
	type receiptTarget struct{ parent, receipt string }
	targets := map[string]receiptTarget{}
	for _, id := range sortedSuiIDs(vaultIDs) {
		vault := vaults[id]
		if vaultRefs[id].Kind != "vault" || vault.OwnerKind != "SHARED" {
			return state, fmt.Errorf("invalid Volo vault")
		}
		fields, err := suiObjectFields(vault.Content)
		if err != nil {
			return state, err
		}
		parent, err := suiTableID(fields, "receipts")
		if err != nil {
			return state, err
		}
		state.Vaults = append(state.Vaults, protocolObject(vault, "vault", ""))
		for _, receipt := range state.Owned {
			if receipt.Key != id {
				continue
			}
			field, err := suiAddressFieldID(parent, receipt.ID)
			if err != nil {
				return state, err
			}
			targets[field] = receiptTarget{parent, receipt.ID}
		}
	}
	ids = nil
	for id := range targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	receipts, receiptRefs, err := r.objects(ctx, ids, pin, false)
	if err != nil {
		return state, err
	}
	for _, id := range ids {
		object, ok := receipts[id]
		if !ok {
			continue
		} // Fresh or emptied receipts may have no state field.
		target := targets[id]
		if receiptRefs[id].Kind != "receiptState" || object.OwnerKind != "OBJECT" || object.Owner != target.parent {
			return state, fmt.Errorf("invalid Volo receipt state attribution")
		}
		state.ReceiptStates = append(state.ReceiptStates, protocolObject(object, "receiptState", target.receipt))
	}
	err = r.loadVoloNAV(ctx, pin, oracle, &state)
	return state, err
}

func (r *suiHistoryIndex) loadVoloNAV(ctx context.Context, pin SuiCheckpoint, oracle SuiObject, state *suiProtocolState) error {
	active, err := suiActiveShareVaults(*state)
	if err != nil || len(active) == 0 {
		return err
	}
	fields, err := suiObjectFields(oracle.Content)
	if err != nil {
		return err
	}
	parent, err := suiTableID(fields, "aggregators")
	if err != nil {
		return err
	}
	priceObjects, err := r.voloCatalog(ctx, pin, "oraclePrice")
	if err != nil {
		return err
	}
	prices := map[string]SuiObject{}
	for _, object := range priceObjects {
		if object.OwnerKind != "OBJECT" || object.Owner != parent {
			return fmt.Errorf("invalid Volo quote parent")
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return err
		}
		name, err := fields.text("name")
		if err != nil {
			return err
		}
		coin, err := suiCoinType(name)
		if err != nil {
			return err
		}
		if _, duplicate := prices[coin]; duplicate {
			return fmt.Errorf("duplicate Volo base-coin quote")
		}
		prices[coin] = object
	}
	state.VaultPrices = map[string]suiProtocolObject{}
	for _, vault := range state.Vaults {
		if !active[vault.ID] {
			continue
		}
		f, err := suiObjectFields(vault.Content)
		if err != nil {
			return err
		}
		valuesParent, err := suiTableID(f, "assets_value")
		if err != nil {
			return err
		}
		timesParent, err := suiTableID(f, "assets_value_updated")
		if err != nil {
			return err
		}
		assets, ok := f["asset_types"].([]any)
		if !ok || len(assets) > suiObjectLimit {
			return fmt.Errorf("invalid Volo asset inventory")
		}
		type target struct{ kind, parent, name string }
		targets := map[string]target{}
		wanted := map[string]bool{}
		for _, asset := range assets {
			name, ok := asset.(string)
			if !ok || wanted[name] {
				return fmt.Errorf("invalid Volo asset inventory")
			}
			wanted[name] = true
			// NAV inventory entries are exact ASCII table keys, including
			// position identifiers that are not valid Move coin types.
			for kind, parent := range map[string]string{"navValue": valuesParent, "navTimestamp": timesParent} {
				id, err := suiASCIIFieldID(parent, name)
				if err != nil {
					return err
				}
				targets[id] = target{kind, parent, name}
			}
		}
		ids := make([]string, 0, len(targets))
		for id := range targets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		objects, refs, err := r.objects(ctx, ids, pin, true)
		if err != nil {
			return err
		}
		if len(objects) != len(ids) {
			return fmt.Errorf("Volo NAV inventory is incomplete")
		}
		values := map[string]string{}
		times := map[string]SuiObject{}
		for _, id := range ids {
			object, target := objects[id], targets[id]
			if refs[id].Kind != target.kind || object.OwnerKind != "OBJECT" || object.Owner != target.parent {
				return fmt.Errorf("Volo NAV field attribution mismatch")
			}
			fields, err := suiObjectFields(object.Content)
			if err != nil {
				return err
			}
			name, err := fields.text("name")
			if err != nil || name != target.name {
				return fmt.Errorf("Volo NAV field key mismatch")
			}
			value, err := fields.uint("value")
			if err != nil {
				return err
			}
			if target.kind == "navValue" {
				values[name] = value.String()
			} else {
				times[name] = object
			}
		}
		for asset := range wanted {
			fields, err := suiObjectFields(times[asset].Content)
			if err != nil {
				return err
			}
			timestamp, err := fields.uint("value")
			if err != nil {
				return err
			}
			content, _ := json.Marshal(map[string]string{"amount": values[asset], "timestamp": timestamp.String(), "asset": asset})
			state.AssetValues = append(state.AssetValues, suiProtocolValue{ID: "assetValue:" + vault.ID + ":" + asset, Kind: "assetValue", Account: vault.ID, Content: string(content)})
		}
		coin, err := suiCoinTypeFromVault(vault.ObjectType)
		if err != nil {
			return err
		}
		quote, err := r.voloSettlement(ctx, pin, prices[coin], values, times)
		if err != nil {
			return err
		}
		fields, err := suiObjectFields(quote.Content)
		if err != nil {
			return err
		}
		name, err := fields.text("name")
		if err != nil {
			return err
		}
		state.VaultPrices[vault.ID] = protocolObject(quote, "oraclePrice", name)
	}
	return nil
}

func (r *suiHistoryIndex) voloSettlement(ctx context.Context, pin SuiCheckpoint, current SuiObject, values map[string]string, times map[string]SuiObject) (SuiObject, error) {
	if current.ID == "" {
		return SuiObject{}, fmt.Errorf("Volo base-coin quote is unavailable")
	}
	var anchor SuiObject
	var timestamp uint64
	for asset, object := range times {
		if values[asset] == "0" {
			continue
		}
		fields, err := suiObjectFields(object.Content)
		if err != nil {
			return SuiObject{}, err
		}
		stamp, err := fields.uint("value")
		if err != nil || !stamp.IsUint64() || stamp.Sign() == 0 {
			return SuiObject{}, fmt.Errorf("invalid Volo NAV timestamp")
		}
		if anchor.ID != "" && (timestamp != stamp.Uint64() || anchor.PreviousTransaction != object.PreviousTransaction || anchor.Version != object.Version) {
			return SuiObject{}, fmt.Errorf("Volo NAV assets have mixed settlement provenance")
		}
		anchor, timestamp = object, stamp.Uint64()
	}
	if anchor.ID == "" {
		return current, nil
	}
	var rows []suiSQLObject
	if sqlData, ok := ctx.Value(suiSQLDataKey{}).(*suiSQLData); ok {
		for _, row := range sqlData.quotes {
			if row.ObjectID == current.ID && row.TransactionDigest == anchor.PreviousTransaction {
				rows = append(rows, row)
			}
		}
	} else {
		return SuiObject{}, fmt.Errorf("Sui SQL state is missing")
	}

	if len(rows) != 1 {
		return SuiObject{}, fmt.Errorf("Volo settlement quote is missing or ambiguous")
	}
	ref := rows[0]
	// All Move objects written by one transaction share its Lamport version:
	// https://mystenlabs.github.io/sui/sui_types/effects/enum.TransactionEffects.html#method.lamport_version
	if ref.ObjectID != current.ID || ref.Kind != "oraclePrice" || ref.State != "live" || ref.TransactionDigest != anchor.PreviousTransaction || ref.Version != strconv.FormatUint(anchor.Version, 10) {
		return SuiObject{}, fmt.Errorf("Volo settlement quote lineage mismatch")
	}
	quote, err := ref.object()
	if err != nil {
		return SuiObject{}, err
	}
	if quote.OwnerKind != "OBJECT" || quote.Owner != current.Owner || quote.ObjectType != current.ObjectType {
		return SuiObject{}, fmt.Errorf("Volo settlement quote attribution mismatch")
	}
	fields, err := suiObjectFields(quote.Content)
	if err != nil {
		return SuiObject{}, err
	}
	value, err := fields.object("value")
	if err != nil {
		return SuiObject{}, err
	}
	stamp, err := value.uint("last_updated")
	if err != nil || !stamp.IsUint64() || stamp.Uint64() != timestamp {
		return SuiObject{}, fmt.Errorf("Volo NAV and settlement quote timestamps disagree")
	}
	return quote, nil
}
