package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// SuiPortfolioInput is a complete protocol inventory at one checkpoint. Objects
// are exact historical versions, including positions untouched since before the
// reporting window. Quotes retain Volo's settlement transaction provenance.
type SuiPortfolioInput struct {
	ProtocolID  string               `json:"protocolId"`
	Checkpoint  string               `json:"checkpoint"`
	TimestampMs string               `json:"timestampMs"`
	Digest      string               `json:"digest"`
	Objects     []SuiPortfolioObject `json:"objects"`
	Quotes      []SuiPortfolioObject `json:"quotes"`
	Metadata    []SuiCoinMetadata    `json:"metadata"`
}

type SuiPortfolioObject = suiSQLObject

// SuiPortfolioAsset is a chain/address selector usable by a quote provider.
// Amounts are quantities, never USD values; a denominator preserves vault math.
type SuiPortfolioAsset struct {
	SentioChainID string `json:"sentioChainId"`
	Address       string `json:"address"`
}

type SuiPortfolioAmount struct {
	Kind                 string            `json:"kind"`
	Asset                SuiPortfolioAsset `json:"asset"`
	Decimals             uint8             `json:"decimals"`
	Symbol               string            `json:"symbol"`
	Name                 string            `json:"name"`
	AmountRaw            string            `json:"amountRaw"`
	AmountDenominatorRaw string            `json:"amountDenominatorRaw"`
	Metadata             map[string]any    `json:"metadata,omitempty"`
}

type SuiPortfolioPosition struct {
	ID         string               `json:"id"`
	MarketID   string               `json:"marketId"`
	Label      string               `json:"label"`
	Components []SuiPortfolioAmount `json:"components"`
	Metadata   map[string]any       `json:"metadata,omitempty"`
}

// SuiPortfolioEvent is the shared daily event's portfolio JSON contract.
type SuiPortfolioEvent struct {
	SchemaVersion int                    `json:"schemaVersion"`
	ProtocolID    string                 `json:"protocolId"`
	Account       string                 `json:"account"`
	Checkpoint    string                 `json:"checkpoint"`
	TimestampMs   string                 `json:"timestampMs"`
	Digest        string                 `json:"digest"`
	Positions     []SuiPortfolioPosition `json:"positions"`
	Errors        []string               `json:"errors"`
}

// SuiPortfolioCalculator indexes one snapshot once, then calculates accounts
// from bounded dependency subsets without any network or price-provider calls.
type SuiPortfolioCalculator struct {
	input            SuiPortfolioInput
	reader           *suiHistoryIndex
	data             *suiSQLData
	kinds            suiSQLKinds
	byKind           map[string][]suiSQLObject
	byOwner          map[string][]suiSQLObject
	byParent         map[string][]suiSQLObject
	byParentKey      map[string][]suiSQLObject
	byKey            map[string][]suiSQLObject
	links            map[string]map[string]json.RawMessage
	accounts         []string
	principalParents map[string]naviBalanceTable
}

func NewSuiPortfolioCalculator(input SuiPortfolioInput) (*SuiPortfolioCalculator, error) {
	kinds, err := suiSQLProtocolKinds(input.ProtocolID)
	if err != nil {
		return nil, err
	}
	pin, err := indexedSuiCheckpoint(input.Checkpoint, input.TimestampMs, input.Digest, 0)
	if err != nil {
		return nil, err
	}
	names := map[string]string{"navi": "NAVI", "volo-vaults": "Volo Vaults", "suilend": "Suilend", "cetus": "Cetus", "bluefin": "Bluefin"}
	c := &SuiPortfolioCalculator{input: input, kinds: kinds,
		reader: &suiHistoryIndex{protocolID: input.ProtocolID, name: names[input.ProtocolID]},
		data:   &suiSQLData{pin: pin, objects: map[string]suiSQLObject{}, metadata: map[string]SuiCoinMetadata{}, quotes: input.Quotes},
		byKind: map[string][]suiSQLObject{}, byOwner: map[string][]suiSQLObject{}, byParent: map[string][]suiSQLObject{},
		byKey: map[string][]suiSQLObject{}, byParentKey: map[string][]suiSQLObject{}, links: map[string]map[string]json.RawMessage{}}
	for _, row := range input.Objects {
		id, e := ParseSuiAddress(row.ObjectID)
		cp, e2 := historyUint(row.Checkpoint)
		if e != nil || id.Hex() != row.ObjectID || e2 != nil || cp > pin.Sequence || row.State != "live" || c.data.objects[row.ObjectID].ObjectID != "" {
			return nil, fmt.Errorf("invalid or duplicate daily inventory object")
		}
		if _, e := row.object(); e != nil {
			return nil, e
		}
		links := map[string]json.RawMessage{}
		if row.Links != "" {
			if e := json.Unmarshal([]byte(row.Links), &links); e != nil {
				return nil, fmt.Errorf("invalid daily object links")
			}
		}
		c.links[row.ObjectID] = links
		c.data.objects[row.ObjectID] = row
		c.byKind[row.Kind] = append(c.byKind[row.Kind], row)
		if row.OwnerKind == "ADDRESS" || row.OwnerKind == "CONSENSUS_ADDRESS" {
			c.byOwner[row.Owner] = append(c.byOwner[row.Owner], row)
		}
		c.byParent[row.ParentID] = append(c.byParent[row.ParentID], row)
		c.byParentKey[row.ParentID+":"+row.Key] = append(c.byParentKey[row.ParentID+":"+row.Key], row)
		c.byKey[row.Key] = append(c.byKey[row.Key], row)
	}
	for _, meta := range input.Metadata {
		normalized, e := NormalizeMoveType(meta.CoinType)
		if e != nil || normalized != meta.CoinType {
			return nil, fmt.Errorf("invalid daily coin identity")
		}
		if c.data.metadata[meta.CoinType].CoinType != "" {
			return nil, fmt.Errorf("duplicate daily coin metadata")
		}
		c.data.metadata[meta.CoinType] = meta
	}
	// These protocols are initialized before the reporting window. Missing their
	// global state is an incomplete inventory, not proof that every wallet is empty.
	required := map[string][]string{"navi": {"storage", "reserve"}, "suilend": {"market"},
		"cetus": {"pool"}, "bluefin": {"pool"}, "volo-vaults": {"vault", "oracle"}}
	for _, kind := range required[input.ProtocolID] {
		if len(c.byKind[kind]) == 0 {
			return nil, fmt.Errorf("daily inventory is missing protocol %s state", kind)
		}
	}
	accounts, children := map[string]bool{}, map[string]bool{}
	for owner, rows := range c.byOwner {
		for _, row := range rows {
			if containsSuiKind(kinds.roots, row.Kind) {
				accounts[owner] = true
			}
			if input.ProtocolID == "navi" && row.Kind == "account" {
				children[c.link(row, "accountAddress")] = true
			}
		}
	}
	if input.ProtocolID == "navi" {
		state := suiProtocolState{}
		for _, kind := range kinds.topology {
			for _, row := range c.byKind[kind] {
				obj, _ := row.object()
				state.Topology = append(state.Topology, protocolObject(obj, kind, row.Key))
			}
		}
		c.principalParents, err = naviBalanceTables(state.Topology)
		if err != nil {
			return nil, err
		}
		for _, row := range c.byKind["principal"] {
			if _, ok := c.principalParents[row.ParentID]; ok && !children[row.Key] {
				accounts[row.Key] = true
			}
		}
	}
	for account := range accounts {
		owner, e := ParseSuiAddress(account)
		if e != nil || owner.Hex() != account {
			return nil, fmt.Errorf("invalid daily account identity")
		}
		c.accounts = append(c.accounts, account)
	}
	sort.Strings(c.accounts)
	// Retain indexed maps, not another large copy of the input slice headers.
	c.input.Objects = nil
	c.input.Metadata = nil
	c.input.Quotes = nil
	return c, nil
}

func containsSuiKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}
func (c *SuiPortfolioCalculator) link(row suiSQLObject, key string) string {
	var value string
	_ = json.Unmarshal(c.links[row.ObjectID][key], &value)
	return value
}
func (c *SuiPortfolioCalculator) Accounts() []string { return append([]string{}, c.accounts...) }

func (c *SuiPortfolioCalculator) accountData(owner SuiAddress) *suiSQLData {
	d := &suiSQLData{pin: c.data.pin, owner: owner, objects: map[string]suiSQLObject{}, metadata: c.data.metadata}
	add := func(row suiSQLObject) {
		if row.ObjectID != "" {
			d.objects[row.ObjectID] = row
		}
	}
	var roots, direct []suiSQLObject
	for _, row := range c.byOwner[owner.Hex()] {
		if containsSuiKind(c.kinds.roots, row.Kind) {
			add(row)
			roots = append(roots, row)
		}
	}
	for _, row := range roots {
		if dep, ok := c.data.objects[row.RelatedID]; ok && containsSuiKind(c.kinds.direct, dep.Kind) {
			add(dep)
			direct = append(direct, dep)
		}
	}
	for _, row := range direct {
		if dep, ok := c.data.objects[row.RelatedID]; ok && containsSuiKind(c.kinds.second, dep.Kind) {
			add(dep)
		}
	}
	for _, kind := range append(append(append([]string{}, c.kinds.topology...), c.kinds.oracle...), c.kinds.prices...) {
		for _, row := range c.byKind[kind] {
			add(row)
		}
	}
	addChildren := func(parent, kind string, keys map[string]bool) {
		if keys == nil {
			for _, row := range c.byParent[parent] {
				if row.Kind == kind {
					add(row)
				}
			}
		} else {
			for key := range keys {
				for _, row := range c.byParentKey[parent+":"+key] {
					if row.Kind == kind {
						add(row)
					}
				}
			}
		}
	}
	switch c.input.ProtocolID {
	case "navi":
		accounts := map[string]bool{owner.Hex(): true}
		receipts := map[string]bool{}
		for _, root := range roots {
			if root.Kind == "account" {
				accounts[c.link(root, "accountAddress")] = true
			} else {
				receipts[root.ObjectID] = true
			}
		}
		for account := range accounts {
			for _, row := range c.byKey[account] {
				if _, ok := c.principalParents[row.ParentID]; ok && row.Kind == "principal" {
					add(row)
				}
			}
		}
		for _, vault := range direct {
			addChildren(c.link(vault, "usersTableId"), "receiptState", receipts)
		}
	case "cetus", "bluefin":
		positions, ticks := map[string]bool{}, map[string]bool{}
		for _, root := range roots {
			positions[root.ObjectID] = true
			ticks[c.link(root, "lowerTick")] = true
			ticks[c.link(root, "upperTick")] = true
		}
		for _, pool := range direct {
			addChildren(c.link(pool, "positionTableId"), "accounting", positions)
			addChildren(c.link(pool, "tickTableId"), "tick", ticks)
		}
	case "volo-vaults":
		receipts := map[string]bool{}
		for _, root := range roots {
			receipts[root.ObjectID] = true
		}
		for _, vault := range direct {
			addChildren(c.link(vault, "usersTableId"), "receiptState", receipts)
			addChildren(c.link(vault, "navValueTableId"), "navValue", nil)
			addChildren(c.link(vault, "navTimestampTableId"), "navTimestamp", nil)
		}
		d.quotes = c.data.quotes
	}
	return d
}

func (c *SuiPortfolioCalculator) Calculate(account string) (SuiPortfolioEvent, error) {
	event := SuiPortfolioEvent{SchemaVersion: 3, ProtocolID: c.input.ProtocolID, Account: account, Checkpoint: c.input.Checkpoint,
		TimestampMs: c.input.TimestampMs, Digest: c.input.Digest, Positions: []SuiPortfolioPosition{}, Errors: []string{}}
	owner, err := ParseSuiAddress(account)
	if err != nil || owner.Hex() != account {
		return event, fmt.Errorf("invalid daily portfolio account")
	}
	result, err := c.reader.calculatePortfolio(context.Background(), owner, c.accountData(owner))
	if err != nil {
		return event, err
	}
	for _, group := range result.Groups {
		position := SuiPortfolioPosition{ID: group.ID, MarketID: group.MarketID, Label: group.Label, Metadata: group.Metadata, Components: []SuiPortfolioAmount{}}
		for _, component := range group.Components {
			denominator := component.AmountDenominatorRaw
			if denominator == "" {
				denominator = "1"
			}
			position.Components = append(position.Components, SuiPortfolioAmount{Kind: component.Kind,
				Asset: SuiPortfolioAsset{SentioChainID: "sui_mainnet", Address: component.Coin.CoinType}, Decimals: component.Coin.Decimals,
				Symbol: component.Coin.Symbol, Name: component.Coin.Name, AmountRaw: component.AmountRaw, AmountDenominatorRaw: denominator, Metadata: component.Metadata})
		}
		sort.SliceStable(position.Components, func(i, j int) bool {
			a, b := position.Components[i], position.Components[j]
			return a.Kind+":"+a.Asset.Address < b.Kind+":"+b.Asset.Address
		})
		event.Positions = append(event.Positions, position)
	}
	return event, nil
}
