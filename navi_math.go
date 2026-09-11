package portfolio

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

var naviRay = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)
var naviHalfRay = new(big.Int).Div(new(big.Int).Set(naviRay), big.NewInt(2))

// NAVI uses half-up ray multiplication, including the intermediate powers in
// its third-order borrow-interest approximation. Replacing this with floats or
// exponentiation does not reproduce the Move contract's rounding.
func naviRayMul(a, b *big.Int) *big.Int {
	n := new(big.Int).Mul(a, b)
	n.Add(n, naviHalfRay)
	return n.Div(n, naviRay)
}

func naviIndexAt(index, rate *big.Int, lastMS, targetMS uint64, borrow bool) (*big.Int, error) {
	if index.Sign() <= 0 || rate.Sign() < 0 || targetMS < lastMS {
		return nil, fmt.Errorf("invalid NAVI interest inputs")
	}
	seconds := new(big.Int).SetUint64((targetMS - lastMS) / 1000)
	interest := new(big.Int).Set(naviRay)
	if !borrow {
		term := new(big.Int).Mul(rate, seconds)
		term.Div(term, big.NewInt(365*24*60*60))
		interest.Add(interest, term)
	} else if seconds.Sign() > 0 {
		perSecond := new(big.Int).Div(new(big.Int).Set(rate), big.NewInt(365*24*60*60))
		square := naviRayMul(perSecond, perSecond)
		cube := naviRayMul(square, perSecond)
		minusOne := new(big.Int).Sub(seconds, big.NewInt(1))
		minusTwo := new(big.Int)
		if seconds.Cmp(big.NewInt(2)) > 0 {
			minusTwo.Sub(seconds, big.NewInt(2))
		}
		term2 := new(big.Int).Mul(seconds, minusOne)
		term2.Mul(term2, square).Div(term2, big.NewInt(2))
		term3 := new(big.Int).Mul(seconds, minusOne)
		term3.Mul(term3, minusTwo).Mul(term3, cube).Div(term3, big.NewInt(6))
		interest.Add(interest, new(big.Int).Mul(perSecond, seconds)).Add(interest, term2).Add(interest, term3)
	}
	return naviRayMul(index, interest), nil
}

// All lending principals use nine decimals, even USDC, BTC and other coins
// whose on-chain precision differs. Convert only after applying the index.
func naviTokenAmount(principal, index *big.Int, decimals uint8) *big.Int {
	amount := naviRayMul(principal, index)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return amount.Mul(amount, scale).Div(amount, big.NewInt(1_000_000_000))
}

type suiFields map[string]any

func unwrapSui(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if inner, ok := value["fields"].(map[string]any); ok {
			value = inner
		}
		result := make(map[string]any, len(value))
		for key, field := range value {
			result[key] = unwrapSui(field)
		}
		return result
	case []any:
		for i, field := range value {
			value[i] = unwrapSui(field)
		}
		return value
	default:
		return value
	}
}

func suiObjectFields(raw string) (suiFields, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, fmt.Errorf("invalid Move object JSON")
	}
	return suiFields(unwrapSui(value).(map[string]any)), nil
}

func (f suiFields) object(key string) (suiFields, error) {
	value, ok := f[key].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Move field %s is not an object", key)
	}
	return suiFields(value), nil
}

func (f suiFields) text(key string) (string, error) {
	switch value := f[key].(type) {
	case string:
		return value, nil
	case json.Number:
		return string(value), nil
	default:
		return "", fmt.Errorf("Move field %s is not a string or integer", key)
	}
}

func (f suiFields) uint(key string) (*big.Int, error) {
	raw, err := f.text(key)
	if err != nil {
		return nil, err
	}
	if raw == "" || (len(raw) > 1 && raw[0] == '0') {
		return nil, fmt.Errorf("invalid Move integer %s", key)
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("invalid Move integer %s", key)
		}
	}
	value, ok := new(big.Int).SetString(raw, 10)
	if !ok || value.BitLen() > 256 {
		return nil, fmt.Errorf("Move integer %s exceeds u256", key)
	}
	return value, nil
}

func (f suiFields) address(key string) (string, error) {
	value := f[key]
	for {
		object, ok := value.(map[string]any)
		if !ok {
			break
		}
		value = object["id"]
	}
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("invalid Move address field %s", key)
	}
	parsed, err := ParseSuiAddress(raw)
	if err != nil {
		return "", err
	}
	return parsed.Hex(), nil
}

func suiCoinTypeFromVault(objectType string) (string, error) {
	index := strings.IndexByte(objectType, '<')
	if index < 0 || !strings.HasSuffix(objectType, ">") {
		return "", fmt.Errorf("vault type has no base coin")
	}
	return NormalizeMoveType(objectType[index+1 : len(objectType)-1])
}

func suiCoinType(raw string) (string, error) {
	if !strings.HasPrefix(raw, "0x") {
		raw = "0x" + raw
	}
	return NormalizeMoveType(raw)
}
