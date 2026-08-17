package portfolio

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

var morphoIRMABI = MustABI(`[
  {"type":"function","name":"borrowRateView","stateMutability":"view","inputs":[
    {"name":"marketParams","type":"tuple","components":[{"name":"loanToken","type":"address"},{"name":"collateralToken","type":"address"},{"name":"oracle","type":"address"},{"name":"irm","type":"address"},{"name":"lltv","type":"uint256"}]},
    {"name":"market","type":"tuple","components":[{"name":"totalSupplyAssets","type":"uint128"},{"name":"totalSupplyShares","type":"uint128"},{"name":"totalBorrowAssets","type":"uint128"},{"name":"totalBorrowShares","type":"uint128"},{"name":"lastUpdate","type":"uint128"},{"name":"fee","type":"uint128"}]}
  ],"outputs":[{"type":"uint256"}]}
]`)

type morphoMarketParams struct {
	LoanToken       common.Address
	CollateralToken common.Address
	Oracle          common.Address
	Irm             common.Address
	Lltv            *big.Int
}

type morphoMarketState struct {
	TotalSupplyAssets *big.Int
	TotalSupplyShares *big.Int
	TotalBorrowAssets *big.Int
	TotalBorrowShares *big.Int
	LastUpdate        *big.Int
	Fee               *big.Int
}

func morphoMulDivDown(x, y, denominator *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(x, y), denominator)
}

func morphoMulDivUp(x, y, denominator *big.Int) *big.Int {
	if x.Sign() == 0 || y.Sign() == 0 {
		return new(big.Int)
	}
	numerator := new(big.Int).Mul(x, y)
	numerator.Add(numerator, new(big.Int).Sub(denominator, big.NewInt(1)))
	return numerator.Div(numerator, denominator)
}

func morphoShareFraction(shares, totalAssets, totalShares *big.Int) (*big.Int, *big.Int) {
	numerator := new(big.Int).Mul(shares, new(big.Int).Add(totalAssets, big.NewInt(1)))
	denominator := new(big.Int).Add(totalShares, big.NewInt(1_000_000))
	return numerator, denominator
}

func morphoEffectiveSupplyShares(
	storedShares *big.Int,
	pendingFeeShares *big.Int,
	account common.Address,
	feeRecipient common.Address,
) *big.Int {
	result := new(big.Int).Set(storedShares)
	if feeRecipient != (common.Address{}) && account == feeRecipient {
		result.Add(result, pendingFeeShares)
	}
	return result
}

// morphoExpectedMarketBalances mirrors Morpho Blue's _accrueInterest using the rate returned by
// borrowRateView for the stored market. It operates on copies so callers retain the pinned storage
// values for validation and metadata.
func morphoExpectedMarketBalances(
	state morphoMarketState,
	borrowRate *big.Int,
	elapsed uint64,
) (morphoMarketState, *big.Int) {
	result := morphoMarketState{
		TotalSupplyAssets: new(big.Int).Set(state.TotalSupplyAssets),
		TotalSupplyShares: new(big.Int).Set(state.TotalSupplyShares),
		TotalBorrowAssets: new(big.Int).Set(state.TotalBorrowAssets),
		TotalBorrowShares: new(big.Int).Set(state.TotalBorrowShares),
		LastUpdate:        new(big.Int).Set(state.LastUpdate),
		Fee:               new(big.Int).Set(state.Fee),
	}
	feeShares := new(big.Int)
	if elapsed == 0 || result.TotalBorrowAssets.Sign() == 0 || borrowRate.Sign() == 0 {
		return result, feeShares
	}
	wad := big.NewInt(1_000_000_000_000_000_000)
	firstTerm := new(big.Int).Mul(borrowRate, new(big.Int).SetUint64(elapsed))
	secondTerm := morphoMulDivDown(firstTerm, firstTerm, new(big.Int).Mul(big.NewInt(2), wad))
	thirdTerm := morphoMulDivDown(secondTerm, firstTerm, new(big.Int).Mul(big.NewInt(3), wad))
	compounded := new(big.Int).Add(firstTerm, secondTerm)
	compounded.Add(compounded, thirdTerm)
	interest := morphoMulDivDown(result.TotalBorrowAssets, compounded, wad)
	result.TotalBorrowAssets.Add(result.TotalBorrowAssets, interest)
	result.TotalSupplyAssets.Add(result.TotalSupplyAssets, interest)
	if result.Fee.Sign() > 0 && interest.Sign() > 0 {
		feeAmount := morphoMulDivDown(interest, result.Fee, wad)
		feeAssets := new(big.Int).Sub(result.TotalSupplyAssets, feeAmount)
		virtualShares := new(big.Int).Add(result.TotalSupplyShares, big.NewInt(1_000_000))
		virtualAssets := new(big.Int).Add(feeAssets, big.NewInt(1))
		feeShares = morphoMulDivDown(feeAmount, virtualShares, virtualAssets)
		result.TotalSupplyShares.Add(result.TotalSupplyShares, feeShares)
	}
	return result, feeShares
}
