package portfolio

import (
	"encoding/base64"
	"fmt"
	"math/big"
)

var suilendWad = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Suilend's Decimal truncates at each multiplication/division, including every
// step of exponentiation. Keep the Move operation order; floats lose raw units.
func suilendMul(a, b *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(a, b), suilendWad)
}

func suilendDiv(a, b *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(a, suilendWad), b)
}

func suilendPow(base *big.Int, exponent uint64) (*big.Int, error) {
	result := new(big.Int).Set(suilendWad)
	for exponent > 0 {
		if exponent&1 != 0 {
			result = suilendMul(result, base)
		}
		exponent >>= 1
		if exponent > 0 {
			base = suilendMul(base, base)
		}
		if result.BitLen() > 256 || base.BitLen() > 256 {
			return nil, fmt.Errorf("Suilend interest exceeds u256")
		}
	}
	return result, nil
}

type suilendReserve struct {
	id, coinType                                   string
	decimals                                       uint8
	available, borrowed, fees, supply, borrowIndex *big.Int
	updated                                        uint64
	utils, aprs                                    []*big.Int
	spreadBPS                                      *big.Int
}

func suilendDecimal(fields suiFields, key string) (*big.Int, error) {
	value, err := fields.object(key)
	if err != nil {
		return nil, err
	}
	return value.uint("value")
}

func suilendUintVector(fields suiFields, key string) ([]*big.Int, error) {
	// Move JSON renders vector<u8> as base64, unlike vectors of wider integers.
	if raw, ok := fields[key].(string); ok && key == "interest_rate_utils" {
		bytes, err := base64.StdEncoding.Strict().DecodeString(raw)
		if err != nil || len(bytes) > 256 {
			return nil, fmt.Errorf("invalid Suilend utilization vector")
		}
		result := make([]*big.Int, len(bytes))
		for i, b := range bytes {
			result[i] = new(big.Int).SetUint64(uint64(b))
		}
		return result, nil
	}
	values, ok := fields[key].([]any)
	if !ok || len(values) > 256 {
		return nil, fmt.Errorf("invalid Suilend vector %s", key)
	}
	result := make([]*big.Int, len(values))
	for i, value := range values {
		n, err := (suiFields{"value": value}).uint("value")
		if err != nil || !n.IsUint64() {
			return nil, fmt.Errorf("invalid Suilend vector %s element", key)
		}
		result[i] = n
	}
	return result, nil
}

func parseSuilendReserve(fields suiFields, market string, index int) (suilendReserve, error) {
	r := suilendReserve{}
	var err error
	if r.id, err = fields.address("id"); err != nil {
		return r, err
	}
	parent, err := fields.address("lending_market_id")
	if err != nil || parent != market {
		return r, fmt.Errorf("Suilend reserve market mismatch")
	}
	n, err := fields.uint("array_index")
	if err != nil || !n.IsUint64() || n.Uint64() != uint64(index) {
		return r, fmt.Errorf("Suilend reserve index mismatch")
	}
	if r.coinType, err = suilendCoinType(fields); err != nil {
		return r, err
	}
	n, err = fields.uint("mint_decimals")
	if err != nil || !n.IsUint64() || n.Uint64() > 36 {
		return r, fmt.Errorf("invalid Suilend reserve decimals")
	}
	r.decimals = uint8(n.Uint64())
	if r.available, err = fields.uint("available_amount"); err != nil {
		return r, err
	}
	if r.supply, err = fields.uint("ctoken_supply"); err != nil {
		return r, err
	}
	if !r.available.IsUint64() || !r.supply.IsUint64() {
		return r, fmt.Errorf("Suilend reserve amount exceeds u64")
	}
	if r.borrowed, err = suilendDecimal(fields, "borrowed_amount"); err != nil {
		return r, err
	}
	if r.fees, err = suilendDecimal(fields, "unclaimed_spread_fees"); err != nil {
		return r, err
	}
	if r.borrowIndex, err = suilendDecimal(fields, "cumulative_borrow_rate"); err != nil {
		return r, err
	}
	if r.borrowIndex.Sign() <= 0 {
		return r, fmt.Errorf("invalid Suilend reserve borrow index")
	}
	n, err = fields.uint("interest_last_update_timestamp_s")
	if err != nil || !n.IsUint64() {
		return r, fmt.Errorf("invalid Suilend reserve timestamp")
	}
	r.updated = n.Uint64()
	cell, err := fields.object("config")
	if err != nil {
		return r, err
	}
	config, err := cell.object("element")
	if err != nil {
		return r, err
	}
	if r.utils, err = suilendUintVector(config, "interest_rate_utils"); err != nil {
		return r, err
	}
	if r.aprs, err = suilendUintVector(config, "interest_rate_aprs"); err != nil {
		return r, err
	}
	if len(r.utils) < 2 || len(r.utils) != len(r.aprs) || r.utils[0].Sign() != 0 || r.utils[len(r.utils)-1].Cmp(big.NewInt(100)) != 0 {
		return r, fmt.Errorf("invalid Suilend interest curve")
	}
	for i := 1; i < len(r.utils); i++ {
		if r.utils[i].Cmp(r.utils[i-1]) <= 0 || r.aprs[i].Cmp(r.aprs[i-1]) < 0 {
			return r, fmt.Errorf("invalid Suilend interest curve order")
		}
	}
	if r.spreadBPS, err = config.uint("spread_fee_bps"); err != nil {
		return r, err
	}
	if r.spreadBPS.Cmp(big.NewInt(10000)) > 0 {
		return r, fmt.Errorf("invalid Suilend spread fee")
	}
	return r, nil
}

func (r suilendReserve) totalSupply() *big.Int {
	n := new(big.Int).Mul(r.available, suilendWad)
	return n.Add(n, r.borrowed).Sub(n, r.fees)
}

// At compounds reserve debt and protocol spread to the observed chain time,
// matching reserve::compound_interest and reserve_config::calculate_apr.
func (r suilendReserve) at(target uint64) (suilendReserve, error) {
	total := r.totalSupply()
	if target < r.updated || total.Sign() < 0 || new(big.Int).Mul(r.available, suilendWad).Cmp(r.fees) < 0 {
		return r, fmt.Errorf("invalid Suilend reserve time or net liquidity")
	}
	if target == r.updated {
		return r, nil
	}
	util := new(big.Int)
	if total.Sign() > 0 {
		util = suilendDiv(r.borrowed, total)
	}
	var apr *big.Int
	percent := new(big.Int).Div(new(big.Int).Set(suilendWad), big.NewInt(100))
	bps := new(big.Int).Div(new(big.Int).Set(suilendWad), big.NewInt(10000))
	for i := 1; i < len(r.utils); i++ {
		left, right := new(big.Int).Mul(r.utils[i-1], percent), new(big.Int).Mul(r.utils[i], percent)
		if util.Cmp(left) >= 0 && util.Cmp(right) <= 0 {
			weight := suilendDiv(new(big.Int).Sub(util, left), new(big.Int).Sub(right, left))
			diff := new(big.Int).Mul(new(big.Int).Sub(r.aprs[i], r.aprs[i-1]), bps)
			apr = new(big.Int).Add(new(big.Int).Mul(r.aprs[i-1], bps), suilendMul(weight, diff))
			break
		}
	}
	if apr == nil {
		return r, fmt.Errorf("Suilend utilization is outside interest curve")
	}
	perSecond := new(big.Int).Div(apr, big.NewInt(365*24*60*60))
	factor, err := suilendPow(new(big.Int).Add(suilendWad, perSecond), target-r.updated)
	if err != nil {
		return r, err
	}
	debt := suilendMul(r.borrowed, new(big.Int).Sub(factor, suilendWad))
	r.borrowed = new(big.Int).Add(r.borrowed, debt)
	r.fees = new(big.Int).Add(r.fees, suilendMul(debt, new(big.Int).Mul(r.spreadBPS, bps)))
	r.borrowIndex = suilendMul(r.borrowIndex, factor)
	r.updated = target
	if r.borrowed.BitLen() > 256 || r.fees.BitLen() > 256 || r.borrowIndex.BitLen() > 256 {
		return r, fmt.Errorf("Suilend reserve interest exceeds u256")
	}
	return r, nil
}

func (r suilendReserve) depositAmount(ctokens *big.Int) (*big.Int, error) {
	if !ctokens.IsUint64() || r.supply.Sign() == 0 || ctokens.Cmp(r.supply) > 0 {
		return nil, fmt.Errorf("invalid Suilend cToken supply")
	}
	ratio := new(big.Int).Div(r.totalSupply(), r.supply)
	return suilendMul(ctokens, ratio), nil
}

func (r suilendReserve) debtAmount(amount, snapshotIndex *big.Int) (*big.Int, error) {
	if snapshotIndex.Sign() <= 0 || snapshotIndex.Cmp(r.borrowIndex) > 0 {
		return nil, fmt.Errorf("invalid Suilend obligation borrow index")
	}
	compounded := suilendMul(amount, suilendDiv(r.borrowIndex, snapshotIndex))
	return new(big.Int).Div(compounded, suilendWad), nil
}
