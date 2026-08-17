package portfolio

import (
	"fmt"
	"math/big"
)

const uniswapMaxTick = 887_272

var (
	uniswapQ96      = new(big.Int).Lsh(big.NewInt(1), 96)
	uniswapQ128     = new(big.Int).Lsh(big.NewInt(1), 128)
	uniswapUint256  = new(big.Int).Lsh(big.NewInt(1), 256)
	uniswapMaxUint  = new(big.Int).Sub(new(big.Int).Set(uniswapUint256), big.NewInt(1))
	tickMultipliers = []*big.Int{
		mustBigHex("fffcb933bd6fad37aa2d162d1a594001"),
		mustBigHex("fff97272373d413259a46990580e213a"),
		mustBigHex("fff2e50f5f656932ef12357cf3c7fdcc"),
		mustBigHex("ffe5caca7e10e4e61c3624eaa0941cd0"),
		mustBigHex("ffcb9843d60f6159c9db58835c926644"),
		mustBigHex("ff973b41fa98c081472e6896dfb254c0"),
		mustBigHex("ff2ea16466c96a3843ec78b326b52861"),
		mustBigHex("fe5dee046a99a2a811c461f1969c3053"),
		mustBigHex("fcbe86c7900a88aedcffc83b479aa3a4"),
		mustBigHex("f987a7253ac413176f2b074cf7815e54"),
		mustBigHex("f3392b0822b70005940c7a398e4b70f3"),
		mustBigHex("e7159475a2c29b7443b29c7fa6e889d9"),
		mustBigHex("d097f3bdfd2022b8845ad8f792aa5825"),
		mustBigHex("a9f746462d870fdf8a65dc1f90e061e5"),
		mustBigHex("70d869a156d2a1b890bb3df62baf32f7"),
		mustBigHex("31be135f97d08fd981231505542fcfa6"),
		mustBigHex("9aa508b5b7a84e1c677de54f3e99bc9"),
		mustBigHex("5d6af8dedb81196699c329225ee604"),
		mustBigHex("2216e584f5fa1ea926041bedfe98"),
		mustBigHex("48a170391f7dc42444e8fa2"),
	}
)

func mustBigHex(value string) *big.Int {
	result, ok := new(big.Int).SetString(value, 16)
	if !ok {
		panic("invalid integer constant " + value)
	}
	return result
}

func uniswapSqrtRatioAtTick(tick int32) (*big.Int, error) {
	absTick := int64(tick)
	if absTick < 0 {
		absTick = -absTick
	}
	if absTick > uniswapMaxTick {
		return nil, fmt.Errorf("tick %d is outside Uniswap TickMath bounds", tick)
	}
	var ratio *big.Int
	if absTick&1 != 0 {
		ratio = new(big.Int).Set(tickMultipliers[0])
	} else {
		ratio = new(big.Int).Lsh(big.NewInt(1), 128)
	}
	for bit := 1; bit < len(tickMultipliers); bit++ {
		if absTick&(1<<bit) != 0 {
			ratio.Mul(ratio, tickMultipliers[bit])
			ratio.Rsh(ratio, 128)
		}
	}
	if tick > 0 {
		ratio.Quo(uniswapMaxUint, ratio)
	}
	remainderMask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 32), big.NewInt(1))
	remainder := new(big.Int).And(new(big.Int).Set(ratio), remainderMask)
	ratio.Rsh(ratio, 32)
	if remainder.Sign() != 0 {
		ratio.Add(ratio, big.NewInt(1))
	}
	return ratio, nil
}

func uniswapAmount0ForLiquidity(sqrtA, sqrtB, liquidity *big.Int) *big.Int {
	lower := new(big.Int).Set(sqrtA)
	upper := new(big.Int).Set(sqrtB)
	if lower.Cmp(upper) > 0 {
		lower, upper = upper, lower
	}
	result := new(big.Int).Lsh(new(big.Int).Set(liquidity), 96)
	result.Mul(result, new(big.Int).Sub(upper, lower))
	result.Quo(result, upper)
	result.Quo(result, lower)
	return result
}

func uniswapAmount1ForLiquidity(sqrtA, sqrtB, liquidity *big.Int) *big.Int {
	lower := new(big.Int).Set(sqrtA)
	upper := new(big.Int).Set(sqrtB)
	if lower.Cmp(upper) > 0 {
		lower, upper = upper, lower
	}
	result := new(big.Int).Mul(liquidity, new(big.Int).Sub(upper, lower))
	return result.Quo(result, uniswapQ96)
}

func uniswapAmountsForLiquidity(
	sqrtPriceX96 *big.Int,
	tickLower int32,
	tickUpper int32,
	liquidity *big.Int,
) (*big.Int, *big.Int, error) {
	sqrtLower, err := uniswapSqrtRatioAtTick(tickLower)
	if err != nil {
		return nil, nil, err
	}
	sqrtUpper, err := uniswapSqrtRatioAtTick(tickUpper)
	if err != nil {
		return nil, nil, err
	}
	if sqrtPriceX96.Cmp(sqrtLower) <= 0 {
		return uniswapAmount0ForLiquidity(sqrtLower, sqrtUpper, liquidity), new(big.Int), nil
	}
	if sqrtPriceX96.Cmp(sqrtUpper) < 0 {
		return uniswapAmount0ForLiquidity(sqrtPriceX96, sqrtUpper, liquidity),
			uniswapAmount1ForLiquidity(sqrtLower, sqrtPriceX96, liquidity), nil
	}
	return new(big.Int), uniswapAmount1ForLiquidity(sqrtLower, sqrtUpper, liquidity), nil
}

func uniswapFeesFromGrowth(liquidity, currentGrowth, lastGrowth *big.Int) *big.Int {
	delta := new(big.Int).Sub(currentGrowth, lastGrowth)
	delta.Mod(delta, uniswapUint256)
	return new(big.Int).Quo(new(big.Int).Mul(liquidity, delta), uniswapQ128)
}

func uniswapDecodePackedInt24(value *big.Int, offset uint) int32 {
	raw := new(big.Int).Rsh(new(big.Int).Set(value), offset)
	raw.And(raw, big.NewInt(0xff_ffff))
	decoded := raw.Int64()
	if decoded >= 0x80_0000 {
		decoded -= 0x100_0000
	}
	return int32(decoded)
}
