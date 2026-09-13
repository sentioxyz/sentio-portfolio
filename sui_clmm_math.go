package portfolio

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/blake2b"
)

var suiCLMMQ64 = new(big.Int).Lsh(big.NewInt(1), 64)
var suiCLMMMod128 = new(big.Int).Lsh(big.NewInt(1), 128)

// Both protocols settle withdrawals by flooring each Q64 token delta. Tick
// objects supply the exact boundary square-root prices used by the contracts.
func suiCLMMAmounts(liquidity, price, lower, upper *big.Int) (a, b *big.Int) {
	p := new(big.Int).Set(price)
	if p.Cmp(lower) < 0 {
		p.Set(lower)
	}
	if p.Cmp(upper) > 0 {
		p.Set(upper)
	}
	a = new(big.Int).Sub(upper, p)
	a.Mul(a, liquidity).Mul(a, suiCLMMQ64).Div(a, new(big.Int).Mul(p, upper))
	b = new(big.Int).Sub(p, lower)
	b.Mul(b, liquidity).Div(b, suiCLMMQ64)
	return
}

func suiCLMMSub(a, b *big.Int) *big.Int {
	return new(big.Int).Mod(new(big.Int).Sub(a, b), suiCLMMMod128)
}

func suiCLMMInside(global, lower, upper *big.Int, current, lo, hi int32) *big.Int {
	below, above := lower, upper
	if current < lo {
		below = suiCLMMSub(global, lower)
	}
	if current >= hi {
		above = suiCLMMSub(global, upper)
	}
	return suiCLMMSub(suiCLMMSub(global, below), above)
}

func suiCLMMOwed(owed, last, inside, liquidity *big.Int) (*big.Int, error) {
	delta := suiCLMMSub(inside, last)
	delta.Mul(delta, liquidity).Div(delta, suiCLMMQ64).Add(delta, owed)
	if !delta.IsUint64() {
		return nil, fmt.Errorf("CLMM accrued amount exceeds u64")
	}
	return delta, nil
}

// Only the key layouts needed by these point reads are admitted: u64 and a
// non-generic struct (object::ID or i32::I32). This is Sui hash_type_and_key,
// including the BCS TypeTag; it never enumerates a pool's position/tick table.
func suiCLMMFieldID(parent, keyType string, key []byte) (string, error) {
	p, err := ParseSuiAddress(parent)
	if err != nil {
		return "", err
	}
	var tag []byte
	if keyType == "u64" {
		tag = []byte{2}
	} else {
		parts := strings.Split(keyType, "::")
		if len(parts) != 3 || len(parts[1]) > 127 || len(parts[2]) > 127 || strings.ContainsAny(keyType, "<>") {
			return "", fmt.Errorf("invalid CLMM dynamic field key type")
		}
		address, err := ParseSuiAddress(parts[0])
		if err != nil {
			return "", err
		}
		tag = append([]byte{7}, address[:]...)
		tag = append(tag, byte(len(parts[1])))
		tag = append(tag, parts[1]...)
		tag = append(tag, byte(len(parts[2])))
		tag = append(tag, parts[2]...)
		tag = append(tag, 0)
	}
	data := append([]byte{0xf0}, p[:]...)
	data = binary.LittleEndian.AppendUint64(data, uint64(len(key)))
	data = append(data, key...)
	data = append(data, tag...)
	hash := blake2b.Sum256(data)
	return SuiAddress(hash).Hex(), nil
}

// Parsing retains the first coverage error; missing fields never become zero
// balances. The helpers accept both fullnode gRPC JSON and Move fields wrappers.
type suiCLMMParser struct{ err error }

func (p *suiCLMMParser) fail(err error) {
	if p.err == nil {
		p.err = err
	}
}
func (p *suiCLMMParser) object(f suiFields, k string) suiFields {
	v, e := f.object(k)
	p.fail(e)
	return v
}
func (p *suiCLMMParser) uint(f suiFields, k string, bits int) *big.Int {
	v, e := f.uint(k)
	p.fail(e)
	if e != nil {
		return new(big.Int)
	}
	if v.BitLen() > bits {
		p.fail(fmt.Errorf("CLMM %s exceeds u%d", k, bits))
	}
	return v
}
func (p *suiCLMMParser) address(f suiFields, k string) string {
	v, e := f.address(k)
	p.fail(e)
	return v
}
func (p *suiCLMMParser) coin(f suiFields, k string) string {
	v, ok := f[k].(string)
	if !ok {
		v, _ = p.object(f, k)["name"].(string)
	}
	coin, e := suiCoinType(v)
	p.fail(e)
	return coin
}
func (p *suiCLMMParser) tick(f suiFields, k string) int32 {
	bits := p.uint(p.object(f, k), "bits", 32)
	tick := int32(uint32(bits.Uint64()))
	if tick < -443636 || tick > 443636 {
		p.fail(fmt.Errorf("CLMM tick is outside supported range"))
	}
	return tick
}
func (p *suiCLMMParser) vector(f suiFields, k string) []suiFields {
	raw, ok := f[k].([]any)
	if !ok || len(raw) > suiObjectLimit {
		p.fail(fmt.Errorf("invalid CLMM vector %s", k))
		return nil
	}
	result := make([]suiFields, len(raw))
	for i, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			p.fail(fmt.Errorf("invalid CLMM vector element %s", k))
		}
		result[i] = suiFields(m)
	}
	return result
}
func (p *suiCLMMParser) growths(f suiFields, k string) []*big.Int {
	raw, ok := f[k].([]any)
	if !ok || len(raw) > suiObjectLimit {
		p.fail(fmt.Errorf("invalid CLMM growth vector %s", k))
		return nil
	}
	result := make([]*big.Int, len(raw))
	for i, v := range raw {
		result[i] = p.uint(suiFields{"growth": v}, "growth", 128)
	}
	return result
}
