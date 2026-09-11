package portfolio

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

// Only root identities are cached. Every scan reads the current market counter
// and all state used in position calculations. A gate coalesces cold discovery
// while allowing waiting requests to cancel.
type suiRootCache struct {
	gate    chan struct{}
	version uint64
	digest  string
	ids     []string
}

func (c *suiRootCache) lock(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func naviMarketCounter(field SuiObject) (uint64, error) {
	if field.Owner != naviMainStorage || field.OwnerKind != "OBJECT" || !suiFieldHasKey(field, naviMarketKeyType) {
		return 0, fmt.Errorf("invalid NAVI market inventory field")
	}
	f, err := suiObjectFields(field.Content)
	if err != nil {
		return 0, err
	}
	value, err := f.object("value")
	if err != nil {
		return 0, err
	}
	main, ok := value["is_main_market"].(bool)
	market, err := value.uint("market_id")
	if err != nil || !ok || !main || market.Sign() != 0 {
		return 0, fmt.Errorf("invalid NAVI main market identity")
	}
	last, err := value.uint("last_market_id")
	if err != nil || !last.IsUint64() || last.Uint64() >= suiObjectLimit {
		return 0, fmt.Errorf("invalid NAVI market inventory count")
	}
	return last.Uint64(), nil
}

func createdSuiRoots(ctx context.Context, reader SuiObjectReader, changes []SuiObjectChange, typ string) ([]string, error) {
	typ = suiType(typ)
	candidates := []string{}
	for _, change := range changes {
		if change.Created && change.OwnerKind == "SHARED" && (change.ObjectType == "" || change.ObjectType == typ) {
			candidates = append(candidates, change.ID)
		}
	}
	objects, err := reader.Objects(ctx, candidates)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, id := range candidates {
		if object, ok := objects[id]; ok && object.OwnerKind == "SHARED" && object.ObjectType == typ {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (r *SuiProtocolReader) discoverNaviMarkets(ctx context.Context, reader SuiObjectReader) ([]string, error) {
	lineage, ok := reader.(SuiObjectLineageReader)
	if !ok {
		return nil, fmt.Errorf("Sui object lineage reads are unavailable")
	}
	fields, err := reader.DynamicFields(ctx, naviMainStorage)
	if err != nil {
		return nil, err
	}
	var current SuiObject
	for _, field := range fields {
		if suiFieldHasKey(field, naviMarketKeyType) {
			if current.ID != "" {
				return nil, fmt.Errorf("ambiguous NAVI market inventory field")
			}
			current = field
		}
	}
	count, err := naviMarketCounter(current)
	if err != nil {
		return nil, err
	}
	// Read the current counter outside the cache gate so warm scans do not
	// serialize network requests across wallets.
	if err := r.markets.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-r.markets.gate }()
	head := current
	remaining := count
	ids := []string{}
	for {
		if current.Version == r.markets.version && current.PreviousTransaction == r.markets.digest && len(r.markets.ids) > 0 {
			ids = append(ids, r.markets.ids...)
			break
		}
		if remaining == 0 {
			ids = append(ids, naviMainStorage)
			break
		}
		if err := validateSuiTransactionDigest(current.PreviousTransaction); err != nil {
			return nil, err
		}
		changes, err := lineage.TransactionObjectChanges(ctx, current.PreviousTransaction)
		if err != nil {
			return nil, fmt.Errorf("NAVI market inventory transaction: %w", err)
		}
		var previousVersion uint64
		for _, change := range changes {
			if change.ID == current.ID && change.OutputVersion == current.Version && !change.Created {
				previousVersion = change.InputVersion
			}
		}
		if previousVersion == 0 || previousVersion >= current.Version {
			return nil, fmt.Errorf("broken NAVI market inventory lineage")
		}
		previous, err := lineage.ObjectAtVersion(ctx, current.ID, previousVersion)
		if err != nil {
			return nil, fmt.Errorf("NAVI market inventory version: %w", err)
		}
		if previous.ID != current.ID || previous.Version != previousVersion {
			return nil, fmt.Errorf("NAVI market inventory version mismatch")
		}
		before, err := naviMarketCounter(previous)
		if err != nil {
			return nil, err
		}
		if before >= remaining {
			return nil, fmt.Errorf("NAVI market inventory counter did not decrease")
		}
		created, err := createdSuiRoots(ctx, reader, changes, naviStorageType)
		if err != nil {
			return nil, err
		}
		if uint64(len(created)) != remaining-before {
			return nil, fmt.Errorf("NAVI market inventory creation count mismatch")
		}
		ids = append(ids, created...)
		current, remaining = previous, before
	}
	sort.Strings(ids)
	if uint64(len(ids)) != count+1 || len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return nil, fmt.Errorf("NAVI market inventory is incomplete or duplicated")
	}
	r.markets.version, r.markets.digest, r.markets.ids = head.Version, head.PreviousTransaction, slices.Clone(ids)
	return ids, nil
}

func (r *SuiProtocolReader) discoverVoloOracle(ctx context.Context, reader SuiObjectReader) ([]string, error) {
	lineage, ok := reader.(SuiObjectLineageReader)
	if !ok {
		return nil, fmt.Errorf("Sui object lineage reads are unavailable")
	}
	if err := r.oracle.lock(ctx); err != nil {
		return nil, err
	}
	defer func() { <-r.oracle.gate }()
	if len(r.oracle.ids) != 0 {
		return slices.Clone(r.oracle.ids), nil
	}
	digest, err := lineage.PreviousTransaction(ctx, voloVaultPackage)
	if err != nil {
		return nil, err
	}
	changes, err := lineage.TransactionObjectChanges(ctx, digest)
	if err != nil {
		return nil, err
	}
	ids, err := createdSuiRoots(ctx, reader, changes, voloVaultPackage+"::vault_oracle::OracleConfig")
	if err != nil {
		return nil, err
	}
	if len(ids) != 1 {
		return nil, fmt.Errorf("ambiguous or missing Volo oracle publication")
	}
	r.oracle.ids = slices.Clone(ids)
	return ids, nil
}
