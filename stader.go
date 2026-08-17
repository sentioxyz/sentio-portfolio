package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const staderActivationBlock = 17_416_153

type staderBSCDeploymentConfig struct {
	liquidToken            Token
	manager                common.Address
	tokenActivationBlock   uint64
	managerActivationBlock uint64
	nativeToken            Token
}

var (
	staderConfigAddress         = common.HexToAddress("0x4ABEF2263d5A5ED582FC9A9789a41D85b68d69DB")
	staderETHxAddress           = common.HexToAddress("0xA35b1B31Ce002FBF2058D22F30f95D405200A15b")
	staderPoolUtilsAddress      = common.HexToAddress("0xeDA89ed8F89D786D816F8E14CF8d2F90c6BF763f")
	staderSDCollateralAddress   = common.HexToAddress("0x7Af4730cc8EbAd1a050dcad5c03c33D2793EE91f")
	staderStakePoolManager      = common.HexToAddress("0xcf5EA1b38380f6aF39068375516Daf40Ed70D299")
	staderWithdrawalManager     = common.HexToAddress("0x9F0491B32DBce587c50c4C43AB303b06478193A7")
	staderSDUtilityPool         = common.HexToAddress("0xED6EE5049f643289ad52411E9aDeC698D04a9602")
	staderSDIncentiveController = common.HexToAddress("0xe225825bcf20F39E2F2e2170412a3247D83492D0")
	staderSDAddress             = common.HexToAddress("0x30D20208d987713f46DFD34EF128Bb16C404D10f")
	staderETHToken              = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	staderSDToken       = token(Ethereum, staderSDAddress.Hex(), "SD", 18)
	staderBSCDeployment = staderBSCDeploymentConfig{
		liquidToken: token(
			BSC,
			"0x1bdd3cf7f79cfb8edbb955f20ad99211551ba275",
			"BNBx",
			18,
		),
		manager:                common.HexToAddress("0x3b961e83400D51e6E1AF5c450d3C7d7b80588d28"),
		tokenActivationBlock:   19_907_065,
		managerActivationBlock: 40_990_394,
		nativeToken: token(
			BSC,
			"0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c",
			"BNB",
			18,
		),
	}
)

var staderBSCManagerABI = MustABI(`[
  {"type":"function","name":"convertBnbXToBnb","stateMutability":"view","inputs":[{"name":"amountInBnbX","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getUserRequestIds","stateMutability":"view","inputs":[{"name":"user","type":"address"}],"outputs":[{"type":"uint256[]"}]},
  {
    "type":"function",
    "name":"getUserRequestInfo",
    "stateMutability":"view",
    "inputs":[{"name":"requestId","type":"uint256"}],
    "outputs":[
      {"name":"user","type":"address"},
      {"name":"processed","type":"bool"},
      {"name":"claimed","type":"bool"},
      {"name":"amountInBnbX","type":"uint256"},
      {"name":"batchId","type":"uint256"}
    ]
  },
  {
    "type":"function",
    "name":"getBatchWithdrawalRequestInfo",
    "stateMutability":"view",
    "inputs":[{"name":"batchId","type":"uint256"}],
    "outputs":[
      {"name":"amountInBnb","type":"uint256"},
      {"name":"amountInBnbX","type":"uint256"},
      {"name":"unlockTime","type":"uint256"},
      {"name":"operator","type":"address"},
      {"name":"isClaimable","type":"bool"}
    ]
  }
]`)

var staderConfigABI = MustABI(`[
  {"type":"function","name":"getETHxToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getStakePoolManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getUserWithdrawManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getSDUtilityPool","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getSDIncentiveController","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getSDCollateral","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getPoolUtils","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getStaderToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var staderStakePoolManagerABI = MustABI(`[
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var staderWithdrawalABI = MustABI(`[
  {"type":"function","name":"getRequestIdsByUser","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256[]"}]},
  {
    "type":"function",
    "name":"userWithdrawRequests",
    "stateMutability":"view",
    "inputs":[{"name":"requestId","type":"uint256"}],
    "outputs":[
      {"name":"owner","type":"address"},
      {"name":"ethXAmount","type":"uint256"},
      {"name":"ethExpected","type":"uint256"},
      {"name":"ethFinalized","type":"uint256"},
      {"name":"requestBlock","type":"uint256"}
    ]
  }
]`)

var staderSDUtilityPoolABI = MustABI(`[
  {"type":"function","name":"getDelegatorLatestSDBalance","stateMutability":"view","inputs":[{"name":"delegator","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getRequestIdsByDelegator","stateMutability":"view","inputs":[{"name":"delegator","type":"address"}],"outputs":[{"type":"uint256[]"}]},
  {
    "type":"function",
    "name":"delegatorWithdrawRequests",
    "stateMutability":"view",
    "inputs":[{"name":"requestId","type":"uint256"}],
    "outputs":[
      {"name":"owner","type":"address"},
      {"name":"amountOfCToken","type":"uint256"},
      {"name":"sdExpected","type":"uint256"},
      {"name":"sdFinalized","type":"uint256"},
      {"name":"requestBlock","type":"uint256"}
    ]
  }
]`)

var staderSDCollateralABI = MustABI(`[
  {"type":"function","name":"operatorSDBalance","stateMutability":"view","inputs":[{"name":"operator","type":"address"}],"outputs":[{"type":"uint256"}]},
  {
    "type":"function",
    "name":"getOperatorInfo",
    "stateMutability":"view",
    "inputs":[{"name":"operator","type":"address"}],
    "outputs":[
      {"name":"poolId","type":"uint8"},
      {"name":"operatorId","type":"uint256"},
      {"name":"validatorCount","type":"uint256"}
    ]
  }
]`)

var staderPoolUtilsABI = MustABI(`[
  {"type":"function","name":"isExistingOperator","stateMutability":"view","inputs":[{"name":"operator","type":"address"}],"outputs":[{"type":"bool"}]},
  {"type":"function","name":"getCollateralETH","stateMutability":"view","inputs":[{"name":"poolId","type":"uint8"}],"outputs":[{"type":"uint256"}]}
]`)

type StaderAdapter struct {
	adapterBase
}

func newStaderAdapter() Adapter {
	return &StaderAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "stader", Name: "Stader", Chains: []ChainID{Ethereum, BSC},
	}}}
}

type staderBSCProcessedWithdrawal struct {
	amountInBNBx      *big.Int
	batchAmountInBNB  *big.Int
	batchAmountInBNBx *big.Int
}

func staderBSCWithdrawalAmount(
	unprocessedAmountInBNB *big.Int,
	processed []staderBSCProcessedWithdrawal,
) (*big.Int, error) {
	if unprocessedAmountInBNB == nil || unprocessedAmountInBNB.Sign() < 0 {
		return nil, fmt.Errorf("invalid unprocessed BNB amount")
	}
	total := new(big.Int).Set(unprocessedAmountInBNB)
	for index, request := range processed {
		if request.amountInBNBx == nil || request.amountInBNBx.Sign() < 0 ||
			request.batchAmountInBNB == nil || request.batchAmountInBNB.Sign() < 0 ||
			request.batchAmountInBNBx == nil || request.batchAmountInBNBx.Sign() <= 0 {
			return nil, fmt.Errorf("processed withdrawal %d has invalid batch values", index)
		}
		amount := new(big.Int).Mul(request.batchAmountInBNB, request.amountInBNBx)
		amount.Div(amount, request.batchAmountInBNBx)
		total.Add(total, amount)
	}
	return total, nil
}

func assertStaderWiring(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) error {
	methods := []string{
		"getETHxToken",
		"getStakePoolManager",
		"getUserWithdrawManager",
		"getSDUtilityPool",
		"getSDIncentiveController",
		"getSDCollateral",
		"getPoolUtils",
		"getStaderToken",
	}
	expected := []common.Address{
		staderETHxAddress,
		staderStakePoolManager,
		staderWithdrawalManager,
		staderSDUtilityPool,
		staderSDIncentiveController,
		staderSDCollateralAddress,
		staderPoolUtilsAddress,
		staderSDAddress,
	}
	calls := make([]ContractCall, len(methods))
	for index, method := range methods {
		calls[index] = ContractCall{
			Contract: staderConfigAddress,
			ABI:      staderConfigABI,
			Method:   method,
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return err
	}
	for index, row := range rows {
		actual, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return decodeErr
		}
		if actual != expected[index] {
			return fmt.Errorf("%s returned %s, expected %s", methods[index], actual, expected[index])
		}
	}
	return nil
}

func readStaderWithdrawalGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	requestIDs []*big.Int,
	contract common.Address,
	contractABIName string,
	groupID string,
	positionMethod string,
	valueLabel string,
	positionABI ContractCall,
	positionToken Token,
) (*Group, error) {
	if len(requestIDs) == 0 {
		return nil, nil
	}
	if len(requestIDs) > 4_096 {
		return nil, fmt.Errorf("%s request count %d exceeds bound", groupID, len(requestIDs))
	}
	calls := make([]ContractCall, len(requestIDs))
	for index, requestID := range requestIDs {
		call := positionABI
		call.Contract = contract
		call.Method = positionMethod
		call.Args = []any{requestID}
		calls[index] = call
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	amount := new(big.Int)
	finalizedCount := 0
	ids := make([]string, len(requestIDs))
	for index, row := range rows {
		ids[index] = requestIDs[index].String()
		owner, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if owner != account {
			return nil, fmt.Errorf("%s request %s owner changed to %s", groupID, requestIDs[index], owner)
		}
		expected, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, decodeErr
		}
		finalized, decodeErr := BigIntAt(row, 3)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if finalized.Sign() > 0 {
			amount.Add(amount, finalized)
			finalizedCount++
		} else {
			amount.Add(amount, expected)
		}
	}
	if amount.Sign() == 0 {
		return nil, nil
	}
	return &Group{
		ID:       groupID,
		MarketID: groupID,
		Label:    valueLabel,
		Components: []Component{NewComponent(
			"asset",
			positionToken,
			amount,
			Source{
				Contract: contract,
				Method:   contractABIName + " + " + positionMethod,
			},
		)},
		Metadata: map[string]any{
			"requestIds":     strings.Join(ids, ","),
			"finalizedCount": finalizedCount,
		},
	}, nil
}

func (a *StaderAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	switch block.ChainID {
	case Ethereum:
		return a.ethereumPositions(ctx, client, block, account)
	case BSC:
		return a.bscPositions(ctx, client, block, account)
	default:
		return nil, nil
	}
}

func (a *StaderAdapter) bscPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment := staderBSCDeployment
	if block.Number < deployment.tokenActivationBlock {
		return nil, nil
	}
	calls := []ContractCall{{
		Contract: deployment.liquidToken.Address,
		ABI:      erc20ABI,
		Method:   "balanceOf",
		Args:     []any{account},
	}}
	managerActive := block.Number >= deployment.managerActivationBlock
	if managerActive {
		calls = append(calls, ContractCall{
			Contract: deployment.manager,
			ABI:      staderBSCManagerABI,
			Method:   "getUserRequestIds",
			Args:     []any{account},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	shares, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, fmt.Errorf("BNBx balance: %w", err)
	}
	var requestIDs []*big.Int
	if managerActive {
		requestIDs, err = decodeBigInts(rows[1][0])
		if err != nil {
			return nil, fmt.Errorf("BNBx withdrawal request IDs: %w", err)
		}
	}
	if len(requestIDs) > 4_096 {
		return nil, fmt.Errorf("BNBx withdrawal request count %d exceeds bound", len(requestIDs))
	}

	groups := make([]Group, 0, 2)
	if shares.Sign() > 0 {
		// BNBx is the liquid position's canonical on-chain unit. DeBank presents
		// its USD value as an equivalent amount of BNB by applying independently
		// cached token prices; that presentation ratio is not a protocol exchange
		// rate and therefore does not belong in the quantity calculation.
		component := NewComponent(
			"asset",
			deployment.liquidToken,
			shares,
			Source{Contract: deployment.liquidToken.Address, Method: "balanceOf"},
		)
		groups = append(groups, Group{
			ID: "bnbx", MarketID: "bnbx", Label: "Staked · BNBx", Components: []Component{component},
		})
	}
	if len(requestIDs) == 0 {
		return groups, nil
	}

	requestCalls := make([]ContractCall, len(requestIDs))
	for index, requestID := range requestIDs {
		requestCalls[index] = ContractCall{
			Contract: deployment.manager,
			ABI:      staderBSCManagerABI,
			Method:   "getUserRequestInfo",
			Args:     []any{requestID},
		}
	}
	requestRows, err := client.ParallelCalls(ctx, block, requestCalls)
	if err != nil {
		return nil, fmt.Errorf("BNBx withdrawal requests: %w", err)
	}
	type processedRequest struct {
		amount  *big.Int
		batchID *big.Int
	}
	unprocessedShares := new(big.Int)
	processedRequests := make([]processedRequest, 0, len(requestIDs))
	batchIDs := make([]*big.Int, 0)
	seenBatch := make(map[string]struct{})
	requestIDStrings := make([]string, len(requestIDs))
	for index, row := range requestRows {
		requestIDStrings[index] = requestIDs[index].String()
		owner, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx request %s owner: %w", requestIDs[index], decodeErr)
		}
		if owner != account {
			return nil, fmt.Errorf("BNBx request %s owner changed to %s", requestIDs[index], owner)
		}
		processed, decodeErr := BoolAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx request %s processed: %w", requestIDs[index], decodeErr)
		}
		claimed, decodeErr := BoolAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx request %s claimed: %w", requestIDs[index], decodeErr)
		}
		if claimed {
			return nil, fmt.Errorf("BNBx request %s is claimed but remains in the owner index", requestIDs[index])
		}
		amount, decodeErr := BigIntAt(row, 3)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx request %s amount: %w", requestIDs[index], decodeErr)
		}
		if !processed {
			unprocessedShares.Add(unprocessedShares, amount)
			continue
		}
		batchID, decodeErr := BigIntAt(row, 4)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx request %s batch: %w", requestIDs[index], decodeErr)
		}
		processedRequests = append(processedRequests, processedRequest{amount: amount, batchID: batchID})
		if _, exists := seenBatch[batchID.String()]; !exists {
			seenBatch[batchID.String()] = struct{}{}
			batchIDs = append(batchIDs, batchID)
		}
	}

	unprocessedBNB := new(big.Int)
	if unprocessedShares.Sign() > 0 {
		row, convertErr := client.Call(
			ctx, block, deployment.manager, staderBSCManagerABI, "convertBnbXToBnb", unprocessedShares,
		)
		if convertErr != nil {
			return nil, fmt.Errorf("convert pending BNBx withdrawals: %w", convertErr)
		}
		unprocessedBNB, err = BigIntAt(row, 0)
		if err != nil {
			return nil, fmt.Errorf("converted pending BNBx withdrawals: %w", err)
		}
	}

	batchCalls := make([]ContractCall, len(batchIDs))
	for index, batchID := range batchIDs {
		batchCalls[index] = ContractCall{
			Contract: deployment.manager,
			ABI:      staderBSCManagerABI,
			Method:   "getBatchWithdrawalRequestInfo",
			Args:     []any{batchID},
		}
	}
	batchRows, err := client.ParallelCalls(ctx, block, batchCalls)
	if err != nil {
		return nil, fmt.Errorf("BNBx withdrawal batches: %w", err)
	}
	type batchAmounts struct {
		bnb  *big.Int
		bnbx *big.Int
	}
	batches := make(map[string]batchAmounts, len(batchIDs))
	for index, row := range batchRows {
		bnb, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx batch %s BNB amount: %w", batchIDs[index], decodeErr)
		}
		bnbx, decodeErr := BigIntAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("BNBx batch %s BNBx amount: %w", batchIDs[index], decodeErr)
		}
		batches[batchIDs[index].String()] = batchAmounts{bnb: bnb, bnbx: bnbx}
	}
	processed := make([]staderBSCProcessedWithdrawal, len(processedRequests))
	for index, request := range processedRequests {
		batch, exists := batches[request.batchID.String()]
		if !exists {
			return nil, fmt.Errorf("BNBx request batch %s is missing", request.batchID)
		}
		processed[index] = staderBSCProcessedWithdrawal{
			amountInBNBx: request.amount, batchAmountInBNB: batch.bnb, batchAmountInBNBx: batch.bnbx,
		}
	}
	withdrawalAmount, err := staderBSCWithdrawalAmount(unprocessedBNB, processed)
	if err != nil {
		return nil, err
	}
	if withdrawalAmount.Sign() > 0 {
		component := NewComponent(
			"asset",
			deployment.nativeToken,
			withdrawalAmount,
			Source{Contract: deployment.manager, Method: "getUserRequestInfo + batch exchange rate"},
		)
		component.Metadata = map[string]any{
			"requestIds":        strings.Join(requestIDStrings, ","),
			"unprocessedShares": unprocessedShares.String(),
		}
		groups = append(groups, Group{
			ID: "bnbx-withdrawal", MarketID: "bnbx-withdrawal", Label: "Deposit · BNBx withdrawal",
			Components: []Component{component},
		})
	}
	return groups, nil
}

func (a *StaderAdapter) ethereumPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.Number < staderActivationBlock {
		return nil, nil
	}
	if err := assertStaderWiring(ctx, client, block); err != nil {
		return nil, fmt.Errorf("deployment wiring: %w", err)
	}
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{
			Contract: staderETHxAddress,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{account},
		},
		{
			Contract: staderWithdrawalManager,
			ABI:      staderWithdrawalABI,
			Method:   "getRequestIdsByUser",
			Args:     []any{account},
		},
		{
			Contract: staderSDCollateralAddress,
			ABI:      staderSDCollateralABI,
			Method:   "operatorSDBalance",
			Args:     []any{account},
		},
		{
			Contract: staderPoolUtilsAddress,
			ABI:      staderPoolUtilsABI,
			Method:   "isExistingOperator",
			Args:     []any{account},
		},
		{
			Contract: staderSDUtilityPool,
			ABI:      staderSDUtilityPoolABI,
			Method:   "getDelegatorLatestSDBalance",
			Args:     []any{account},
		},
		{
			Contract: staderSDUtilityPool,
			ABI:      staderSDUtilityPoolABI,
			Method:   "getRequestIdsByDelegator",
			Args:     []any{account},
		},
	})
	if err != nil {
		return nil, err
	}
	ethxShares, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	ethRequestIDs, err := decodeBigInts(rows[1][0])
	if err != nil {
		return nil, err
	}
	operatorSD, err := BigIntAt(rows[2], 0)
	if err != nil {
		return nil, err
	}
	isOperator, err := BoolAt(rows[3], 0)
	if err != nil {
		return nil, err
	}
	sdDelegation, err := BigIntAt(rows[4], 0)
	if err != nil {
		return nil, err
	}
	sdRequestIDs, err := decodeBigInts(rows[5][0])
	if err != nil {
		return nil, err
	}

	groups := make([]Group, 0, 6)
	if ethxShares.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx,
			block,
			staderStakePoolManager,
			staderStakePoolManagerABI,
			"convertToAssets",
			ethxShares,
		)
		if convertErr != nil {
			return nil, convertErr
		}
		amount, decodeErr := BigIntAt(converted, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		component := NewComponent(
			"asset",
			staderETHToken,
			amount,
			Source{Contract: staderStakePoolManager, Method: "convertToAssets(ETHx.balanceOf)"},
		)
		component.Metadata = map[string]any{"shares": ethxShares.String()}
		groups = append(groups, Group{
			ID:         "ethx",
			MarketID:   "ethx",
			Label:      "Staked · ETHx",
			Components: []Component{component},
		})
	}
	ethWithdrawal, err := readStaderWithdrawalGroup(
		ctx,
		client,
		block,
		account,
		ethRequestIDs,
		staderWithdrawalManager,
		"getRequestIdsByUser",
		"ethx-withdrawal",
		"userWithdrawRequests",
		"Deposit · ETHx withdrawal",
		ContractCall{ABI: staderWithdrawalABI},
		staderETHToken,
	)
	if err != nil {
		return nil, fmt.Errorf("ETHx withdrawals: %w", err)
	}
	if ethWithdrawal != nil {
		groups = append(groups, *ethWithdrawal)
	}
	if sdDelegation.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "sd-delegation",
			MarketID: "sd-delegation",
			Label:    "Staked · SD delegation",
			Components: []Component{NewComponent(
				"asset",
				staderSDToken,
				sdDelegation,
				Source{Contract: staderSDUtilityPool, Method: "getDelegatorLatestSDBalance"},
			)},
		})
	}
	sdWithdrawal, err := readStaderWithdrawalGroup(
		ctx,
		client,
		block,
		account,
		sdRequestIDs,
		staderSDUtilityPool,
		"getRequestIdsByDelegator",
		"sd-withdrawal",
		"delegatorWithdrawRequests",
		"Deposit · SD withdrawal",
		ContractCall{ABI: staderSDUtilityPoolABI},
		staderSDToken,
	)
	if err != nil {
		return nil, fmt.Errorf("SD withdrawals: %w", err)
	}
	if sdWithdrawal != nil {
		groups = append(groups, *sdWithdrawal)
	}
	if operatorSD.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "operator-sd",
			MarketID: "operator-sd",
			Label:    "Staked · Node operator SD",
			Components: []Component{NewComponent(
				"asset",
				staderSDToken,
				operatorSD,
				Source{Contract: staderSDCollateralAddress, Method: "operatorSDBalance"},
			)},
			Metadata: map[string]any{"excludesUtilizedSd": true},
		})
	}
	if isOperator {
		operatorInfo, infoErr := client.Call(
			ctx,
			block,
			staderSDCollateralAddress,
			staderSDCollateralABI,
			"getOperatorInfo",
			account,
		)
		if infoErr != nil {
			return nil, infoErr
		}
		poolID, decodeErr := Uint8At(operatorInfo, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		validatorCount, decodeErr := BigIntAt(operatorInfo, 2)
		if decodeErr != nil {
			return nil, decodeErr
		}
		collateralResult, collateralErr := client.Call(
			ctx,
			block,
			staderPoolUtilsAddress,
			staderPoolUtilsABI,
			"getCollateralETH",
			poolID,
		)
		if collateralErr != nil {
			return nil, collateralErr
		}
		collateral, decodeErr := BigIntAt(collateralResult, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		amount := new(big.Int).Mul(validatorCount, collateral)
		if amount.Sign() > 0 {
			groups = append(groups, Group{
				ID:       "operator-eth",
				MarketID: "operator-eth",
				Label:    "Staked · Node operator ETH",
				Components: []Component{NewComponent(
					"asset",
					staderETHToken,
					amount,
					Source{
						Contract: staderSDCollateralAddress,
						Method:   "getOperatorInfo.validatorCount * PoolUtils.getCollateralETH",
					},
				)},
				Metadata: map[string]any{
					"poolId":                 poolID,
					"validatorCount":         validatorCount.String(),
					"collateralPerValidator": collateral.String(),
				},
			})
		}
	}
	return groups, nil
}
