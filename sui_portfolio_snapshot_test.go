package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

// Reuse independently constructed protocol fixtures, with the normalized query
// links emitted by processors. The expected quantities still use the existing
// full-inventory calculation path, not the new account/dependency selection.
func dailyFixture(t *testing.T, protocol string) (SuiPortfolioInput, SuiAddress, SuiProtocolPositions) {
	t.Helper()
	var base *latestSuiFixture
	var owner SuiAddress
	switch protocol {
	case "suilend":
		base, owner = suilendFixture()
	case "cetus", "bluefin":
		base, owner = clmmFixture(protocol)
	case "volo-vaults":
		base, owner = voloValuationFixture()
	default:
		base = latestFixture()
		owner, _ = ParseSuiAddress("0x11")
		base.addMarket(0, 0, owner.Hex(), "900719925474099312345", false)
	}
	f, r := newSQLPortfolioFixture(t, protocol, base)
	d, err := r.readSQL(context.Background(), owner, suiSQLSelection{})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := r.calculatePortfolio(context.Background(), owner, d)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]map[string]string{}
	if protocol == "cetus" || protocol == "bluefin" {
		pkg := cetusCLMMPackage
		if protocol == "bluefin" {
			pkg = bluefinCLMMPackage
		}
		positions, pools, err := loadSuiCLMM(context.Background(), owner, d, pkg)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range positions {
			row := d.objects[p.object.ID]
			row.RelatedID = p.pool
			d.objects[p.object.ID] = row
			links[p.object.ID] = map[string]string{"lowerTick": fmt.Sprint(p.lower), "upperTick": fmt.Sprint(p.upper)}
			pool := pools[p.pool]
			links[p.pool] = map[string]string{"tickTableId": pool.tickTable, "positionTableId": pool.positionTable}
			if p.stateID != "" {
				row := d.objects[p.stateID]
				row.Key = p.object.ID
				d.objects[p.stateID] = row
			}
			for _, tick := range []int32{p.lower, p.upper} {
				id, _, _, err := suiCLMMTickKey(pool.tickTable, tick, pkg)
				if err != nil {
					t.Fatal(err)
				}
				row := d.objects[id]
				row.Key = fmt.Sprint(tick)
				d.objects[id] = row
			}
		}
	}
	input := SuiPortfolioInput{ProtocolID: protocol, Checkpoint: fmt.Sprint(f.pin.Sequence), TimestampMs: fmt.Sprint(f.pin.Timestamp.UnixMilli()), Digest: suiTestDigest, Quotes: d.quotes}
	for _, row := range d.objects {
		fields, _ := suiObjectFields(row.Content)
		if protocol == "suilend" {
			if row.Kind == "cap" {
				row.RelatedID, _ = fields.address("obligation_id")
			}
			if row.Kind == "obligation" {
				row.RelatedID, _ = fields.address("lending_market_id")
			}
		}
		if protocol == "volo-vaults" && row.Kind == "vault" {
			links[row.ObjectID] = map[string]string{}
			for from, to := range map[string]string{"receipts": "usersTableId", "assets_value": "navValueTableId", "assets_value_updated": "navTimestampTableId"} {
				obj, _ := fields.object(from)
				links[row.ObjectID][to], _ = obj.address("id")
			}
		}
		if link, ok := links[row.ObjectID]; ok {
			raw, _ := json.Marshal(link)
			row.Links = string(raw)
		}
		input.Objects = append(input.Objects, row)
	}
	for _, meta := range d.metadata {
		input.Metadata = append(input.Metadata, meta)
	}
	return input, owner, expected
}

func TestSuiDailyCalculatorFiveProtocols(t *testing.T) {
	for _, protocol := range []string{"navi", "volo-vaults", "suilend", "cetus", "bluefin"} {
		t.Run(protocol, func(t *testing.T) {
			input, owner, expected := dailyFixture(t, protocol)
			calculator, err := NewSuiPortfolioCalculator(input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calculator.Accounts(), []string{owner.Hex()}) {
				t.Fatal(calculator.Accounts())
			}
			event, err := calculator.Calculate(owner.Hex())
			if err != nil {
				t.Fatal(err)
			}
			if event.SchemaVersion != 3 || event.Checkpoint != input.Checkpoint || event.TimestampMs != input.TimestampMs || event.Account != owner.Hex() || len(event.Errors) != 0 {
				t.Fatalf("invalid event %+v", event)
			}
			var got, want []string
			for _, g := range event.Positions {
				for _, c := range g.Components {
					if c.Asset.SentioChainID != "sui_mainnet" {
						t.Fatal(c.Asset)
					}
					got = append(got, fmt.Sprint(g.ID, c.Kind, c.Asset.Address, c.Decimals, c.AmountRaw, "/", c.AmountDenominatorRaw))
				}
			}
			for _, g := range expected.Groups {
				for _, c := range g.Components {
					d := c.AmountDenominatorRaw
					if d == "" {
						d = "1"
					}
					want = append(want, fmt.Sprint(g.ID, c.Kind, c.Coin.CoinType, c.Coin.Decimals, c.AmountRaw, "/", d))
				}
			}
			sort.Strings(got)
			sort.Strings(want)
			if len(got) == 0 || !reflect.DeepEqual(got, want) {
				t.Fatalf("quantities %v != %v", got, want)
			}
		})
	}
}

func TestSuiDailyOwnershipAndMissingDependencies(t *testing.T) {
	input, owner, _ := dailyFixture(t, "suilend")
	other, _ := ParseSuiAddress("0x123")
	for i := range input.Objects {
		if input.Objects[i].Kind == "cap" {
			input.Objects[i].Owner = other.Hex()
		}
	}
	c, err := NewSuiPortfolioCalculator(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Accounts(), []string{other.Hex()}) {
		t.Fatal(c.Accounts())
	}
	old, err := c.Calculate(owner.Hex())
	if err != nil || len(old.Positions) != 0 {
		t.Fatal(old, err)
	}
	for i, row := range input.Objects {
		if row.Kind == "market" {
			input.Objects = append(input.Objects[:i], input.Objects[i+1:]...)
			break
		}
	}
	c, err = NewSuiPortfolioCalculator(input)
	if err == nil {
		_, err = c.Calculate(other.Hex())
	}
	if err == nil {
		t.Fatal("missing market became zero balance")
	}
}
