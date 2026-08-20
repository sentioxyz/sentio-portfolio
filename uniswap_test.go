package portfolio

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

func requireBigInt(t *testing.T, value string) *big.Int {
	t.Helper()
	result, ok := new(big.Int).SetString(value, 10)
	if !ok {
		t.Fatalf("invalid test integer %q", value)
	}
	return result
}

func TestUniswapTickMathBoundaryVectors(t *testing.T) {
	for _, test := range []struct {
		tick int32
		want string
	}{
		{tick: -887_272, want: "4295128739"},
		{tick: 0, want: "79228162514264337593543950336"},
		{tick: 887_272, want: "1461446703485210103287273052203988822378723970342"},
	} {
		got, err := uniswapSqrtRatioAtTick(test.tick)
		if err != nil {
			t.Fatal(err)
		}
		if got.String() != test.want {
			t.Fatalf("sqrt ratio at tick %d = %s, want %s", test.tick, got, test.want)
		}
	}
}

func TestUniswapLiquiditySides(t *testing.T) {
	liquidity := requireBigInt(t, "1000000000000000000")
	below, err := uniswapSqrtRatioAtTick(-120)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := uniswapSqrtRatioAtTick(0)
	if err != nil {
		t.Fatal(err)
	}
	above, err := uniswapSqrtRatioAtTick(120)
	if err != nil {
		t.Fatal(err)
	}
	below0, below1, err := uniswapAmountsForLiquidity(below, -60, 60, liquidity)
	if err != nil {
		t.Fatal(err)
	}
	inside0, inside1, err := uniswapAmountsForLiquidity(inside, -60, 60, liquidity)
	if err != nil {
		t.Fatal(err)
	}
	above0, above1, err := uniswapAmountsForLiquidity(above, -60, 60, liquidity)
	if err != nil {
		t.Fatal(err)
	}
	if below0.Sign() <= 0 || below1.Sign() != 0 {
		t.Fatalf("below-range amounts = %s, %s", below0, below1)
	}
	if inside0.Sign() <= 0 || inside1.Sign() <= 0 {
		t.Fatalf("inside-range amounts = %s, %s", inside0, inside1)
	}
	if above0.Sign() != 0 || above1.Sign() <= 0 {
		t.Fatalf("above-range amounts = %s, %s", above0, above1)
	}
}

func TestUniswapFeeGrowthWraparound(t *testing.T) {
	last := new(big.Int).Sub(new(big.Int).Set(uniswapUint256), big.NewInt(1))
	if got := uniswapFeesFromGrowth(big.NewInt(2), big.NewInt(3), last); got.Sign() != 0 {
		t.Fatalf("wrapped sub-Q128 fee = %s, want 0", got)
	}
	if got := uniswapFeesFromGrowth(uniswapQ128, big.NewInt(9), big.NewInt(4)); got.Int64() != 5 {
		t.Fatalf("fee = %s, want 5", got)
	}
}

func TestUniswapDecodePackedInt24(t *testing.T) {
	lower := int32(-120)
	upper := int32(240)
	packed := new(big.Int).SetUint64(uint64(uint32(lower)&0xff_ffff) << 8)
	packed.Or(packed, new(big.Int).Lsh(new(big.Int).SetUint64(uint64(uint32(upper)&0xff_ffff)), 32))
	if got := uniswapDecodePackedInt24(packed, 8); got != lower {
		t.Fatalf("lower = %d, want %d", got, lower)
	}
	if got := uniswapDecodePackedInt24(packed, 32); got != upper {
		t.Fatalf("upper = %d, want %d", got, upper)
	}
}

func TestUniswapV4DynamicPoolUsesSlot0LPFee(t *testing.T) {
	dynamic, err := uniswapV4IsDynamicFee(new(big.Int).SetUint64(uint64(uniswapV4DynamicFeeFlag)))
	if err != nil {
		t.Fatal(err)
	}
	if !dynamic {
		t.Fatal("dynamic fee flag was not recognized")
	}
	fee, err := uniswapV4LPFeeAt([]any{
		big.NewInt(1), big.NewInt(0), big.NewInt(0), big.NewInt(500),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fee != 500 {
		t.Fatalf("LP fee = %d, want 500", fee)
	}
	if label := uniswapFeeLabel(fee); label != "0.05%" {
		t.Fatalf("LP fee label = %q, want 0.05%%", label)
	}
}

func TestUniswapV4RejectsInvalidLPFee(t *testing.T) {
	_, err := uniswapV4LPFeeAt([]any{
		big.NewInt(1), big.NewInt(0), big.NewInt(0), big.NewInt(1_000_001),
	})
	if err == nil {
		t.Fatal("LP fee above 100% was accepted")
	}
}

func TestEngineRegistersUniswapV3AndV4(t *testing.T) {
	protocols := NewEngine(nil, nil).Protocols()
	wanted := map[string]bool{"uniswap-v3": false, "uniswap-v4": false}
	for _, protocol := range protocols {
		if _, exists := wanted[protocol.ID]; !exists {
			continue
		}
		wanted[protocol.ID] = true
		if len(protocol.Chains) != len(SupportedChainIDs) {
			t.Fatalf("%s chains = %v", protocol.ID, protocol.Chains)
		}
	}
	for protocol, found := range wanted {
		if !found {
			t.Errorf("%s is not registered", protocol)
		}
	}
}

func TestUniswapCheckpointUsesSeparateRealtimeAndBackfillBounds(t *testing.T) {
	page := uniswapGraphQLPage{
		CheckpointBlock: 99,
		CheckpointMS:    uint64((1_000_000 - int64(16*time.Hour/time.Second)) * 1_000),
	}
	latest := BlockRef{ChainID: Ethereum, Number: 100, Timestamp: 1_000_000}
	if err := validateUniswapCheckpoint(latest, page); err == nil {
		t.Fatal("latest block accepted a 16-hour-old checkpoint")
	}
	fixed := latest
	fixed.Fixed = true
	if err := validateUniswapCheckpoint(fixed, page); err != nil {
		t.Fatalf("fixed block rejected a valid backfill checkpoint: %v", err)
	}
}

func TestUniswapV3DetailFailureClassification(t *testing.T) {
	position := func(id int64) uniswapV3Position {
		return uniswapV3Position{
			NFT:       uniswapIndexedNFT{TokenID: big.NewInt(id)},
			Pool:      common.HexToAddress("0x000000000000000000000000000000000000dEaD"),
			Liquidity: big.NewInt(1),
		}
	}
	healthySlot := ContractCallResult{Values: []any{big.NewInt(1 << 32)}}
	healthyCollect := ContractCallResult{Values: []any{big.NewInt(7), big.NewInt(11)}}
	revert := ContractCallResult{Error: errors.New("execution reverted: TF")}
	transport := ContractCallResult{Error: errors.New("connection refused")}

	t.Run("collect revert keeps principal with zero fees", func(t *testing.T) {
		detailed, err := applyUniswapV3Details(
			[]uniswapV3Position{position(1), position(2)},
			[]ContractCallResult{healthySlot, revert, healthySlot, healthyCollect},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(detailed) != 2 {
			t.Fatalf("got %d positions, want 2", len(detailed))
		}
		if detailed[0].Collectible0.Sign() != 0 || detailed[0].Collectible1.Sign() != 0 {
			t.Fatalf(
				"poisoned position fees = %s/%s, want 0/0",
				detailed[0].Collectible0, detailed[0].Collectible1,
			)
		}
		if detailed[1].Collectible0.Int64() != 7 || detailed[1].Collectible1.Int64() != 11 {
			t.Fatal("healthy position fees were not preserved")
		}
	})

	t.Run("slot0 revert drops only its own position", func(t *testing.T) {
		detailed, err := applyUniswapV3Details(
			[]uniswapV3Position{position(1), position(2)},
			[]ContractCallResult{revert, healthyCollect, healthySlot, healthyCollect},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(detailed) != 1 || detailed[0].NFT.TokenID.Int64() != 2 {
			t.Fatalf("got %d positions, want only token 2", len(detailed))
		}
	})

	t.Run("transport errors still fail the scan", func(t *testing.T) {
		if _, err := applyUniswapV3Details(
			[]uniswapV3Position{position(1)},
			[]ContractCallResult{healthySlot, transport},
		); err == nil {
			t.Fatal("transport-level collect error was swallowed")
		}
		if _, err := applyUniswapV3Details(
			[]uniswapV3Position{position(1)},
			[]ContractCallResult{transport, healthyCollect},
		); err == nil {
			t.Fatal("transport-level slot0 error was swallowed")
		}
	})
}
