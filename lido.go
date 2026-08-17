package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

var (
	lidoStETHAddress           = common.HexToAddress("0xae7ab96520DE3A18E5e111B5EaAb095312D7fE84")
	lidoWstETHAddress          = common.HexToAddress("0x7f39C581F595B53c5cb19bD0b3f8dA6c935E2Ca0")
	lidoWithdrawalQueueAddress = common.HexToAddress(
		"0x889edC2eDab5f40e902b864aD4d7AdE8E412F9B1",
	)
	lidoEarnETHAddress       = common.HexToAddress("0xbbfc8683c8fe8cf73777fede7ab9574935fea0a4")
	lidoEarnETHVaultAddress  = common.HexToAddress("0x6a37725ca7f4CE81c004c955f7280d5C704a249e")
	lidoStETHDeployment      = deploymentWindow{ActivationBlock: 11_473_216}
	lidoWstETHDeployment     = deploymentWindow{ActivationBlock: 11_888_477}
	lidoWithdrawalDeployment = deploymentWindow{
		ActivationBlock: 17_172_547,
	}
	lidoEarnETHDeployment = deploymentWindow{ActivationBlock: 24_370_480}
	lidoETHToken          = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	lidoStETHToken = token(
		Ethereum,
		lidoStETHAddress.Hex(),
		"stETH",
		18,
	)
)

var lidoWstETHABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getStETHByWstETH","stateMutability":"view","inputs":[{"name":"amount","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var lidoWithdrawalQueueABI = MustABI(`[
  {"type":"function","name":"getWithdrawalRequests","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"name":"requestIds","type":"uint256[]"}]},
  {
    "type":"function",
    "name":"getWithdrawalStatus",
    "stateMutability":"view",
    "inputs":[{"name":"requestIds","type":"uint256[]"}],
    "outputs":[{"name":"statuses","type":"tuple[]","components":[
      {"name":"amountOfStETH","type":"uint256"},
      {"name":"amountOfShares","type":"uint256"},
      {"name":"owner","type":"address"},
      {"name":"timestamp","type":"uint256"},
      {"name":"isFinalized","type":"bool"},
      {"name":"isClaimed","type":"bool"}
    ]}]
  }
]`)

var lidoEarnShareManagerABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"vault","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var lidoEarnVaultABI = MustABI(`[
  {"type":"function","name":"shareManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"oracle","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"getAssetCount","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"assetAt","stateMutability":"view","inputs":[{"name":"index","type":"uint256"}],"outputs":[{"type":"address"}]}
]`)

var lidoEarnOracleABI = MustABI(`[
  {
    "type":"function",
    "name":"getReport",
    "stateMutability":"view",
    "inputs":[{"name":"asset","type":"address"}],
    "outputs":[{"name":"report","type":"tuple","components":[
      {"name":"priceD18","type":"uint224"},
      {"name":"timestamp","type":"uint32"},
      {"name":"isSuspicious","type":"bool"}
    ]}]
  }
]`)

type lidoWithdrawalStatus struct {
	AmountOfStETH  *big.Int
	AmountOfShares *big.Int
	Owner          common.Address
	Timestamp      *big.Int
	IsFinalized    bool
	IsClaimed      bool
}

type lidoEarnReport struct {
	PriceD18     *big.Int
	Timestamp    uint32
	IsSuspicious bool
}

type LidoAdapter struct {
	adapterBase
}

func newLidoAdapter() Adapter {
	return &LidoAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "lido", Name: "Lido", Chains: []ChainID{Ethereum},
	}}}
}

func decodeLidoWithdrawalStatuses(value any) ([]lidoWithdrawalStatus, error) {
	converted := abi.ConvertType(value, new([]lidoWithdrawalStatus))
	statuses, ok := converted.(*[]lidoWithdrawalStatus)
	if !ok || statuses == nil {
		return nil, fmt.Errorf("unexpected withdrawal status type %T", value)
	}
	return *statuses, nil
}

func decodeLidoEarnReport(value any) (lidoEarnReport, error) {
	converted := abi.ConvertType(value, new(lidoEarnReport))
	report, ok := converted.(*lidoEarnReport)
	if !ok || report == nil || report.PriceD18 == nil {
		return lidoEarnReport{}, fmt.Errorf("unexpected earn report type %T", value)
	}
	return *report, nil
}

func readLidoWithdrawals(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) (*Group, error) {
	if !lidoWithdrawalDeployment.ActiveAt(block.Number) {
		return nil, nil
	}
	requestResult, err := client.Call(
		ctx,
		block,
		lidoWithdrawalQueueAddress,
		lidoWithdrawalQueueABI,
		"getWithdrawalRequests",
		account,
	)
	if err != nil {
		return nil, err
	}
	requestIDs, err := decodeBigInts(requestResult[0])
	if err != nil {
		return nil, err
	}
	if len(requestIDs) == 0 {
		return nil, nil
	}
	if len(requestIDs) > 4_096 {
		return nil, fmt.Errorf("withdrawal request count %d exceeds bound", len(requestIDs))
	}
	statusResult, err := client.Call(
		ctx,
		block,
		lidoWithdrawalQueueAddress,
		lidoWithdrawalQueueABI,
		"getWithdrawalStatus",
		requestIDs,
	)
	if err != nil {
		return nil, err
	}
	statuses, err := decodeLidoWithdrawalStatuses(statusResult[0])
	if err != nil {
		return nil, err
	}
	if len(statuses) != len(requestIDs) {
		return nil, fmt.Errorf(
			"withdrawal status count %d differs from request count %d",
			len(statuses),
			len(requestIDs),
		)
	}
	amount := new(big.Int)
	finalized := 0
	ids := make([]string, len(requestIDs))
	for index, status := range statuses {
		ids[index] = requestIDs[index].String()
		if status.IsClaimed {
			continue
		}
		if status.AmountOfStETH == nil {
			return nil, fmt.Errorf("withdrawal status %d has nil amount", index)
		}
		amount.Add(amount, status.AmountOfStETH)
		if status.IsFinalized {
			finalized++
		}
	}
	if amount.Sign() == 0 {
		return nil, nil
	}
	return &Group{
		ID:       "withdrawals",
		MarketID: "withdrawals",
		Label:    "Lido withdrawal queue",
		Components: []Component{NewComponent(
			"asset",
			lidoStETHToken,
			amount,
			Source{
				Contract: lidoWithdrawalQueueAddress,
				Method:   "getWithdrawalStatus(unclaimed).amountOfStETH",
			},
		)},
		Metadata: map[string]any{
			"requestIds":     strings.Join(ids, ","),
			"finalizedCount": finalized,
		},
	}, nil
}

func readLidoEarnETH(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) (*Group, error) {
	if !lidoEarnETHDeployment.ActiveAt(block.Number) {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: lidoEarnETHAddress, ABI: lidoEarnShareManagerABI, Method: "vault"},
		{Contract: lidoEarnETHVaultAddress, ABI: lidoEarnVaultABI, Method: "shareManager"},
		{Contract: lidoEarnETHVaultAddress, ABI: lidoEarnVaultABI, Method: "oracle"},
		{Contract: lidoEarnETHVaultAddress, ABI: lidoEarnVaultABI, Method: "getAssetCount"},
		{
			Contract: lidoEarnETHAddress,
			ABI:      lidoEarnShareManagerABI,
			Method:   "balanceOf",
			Args:     []any{account},
		},
	})
	if err != nil {
		return nil, err
	}
	managerVault, err := AddressAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	vaultShareManager, err := AddressAt(rows[1], 0)
	if err != nil {
		return nil, err
	}
	if managerVault != lidoEarnETHVaultAddress || vaultShareManager != lidoEarnETHAddress {
		return nil, fmt.Errorf("earnETH vault/share-manager wiring changed")
	}
	oracle, err := AddressAt(rows[2], 0)
	if err != nil {
		return nil, err
	}
	assetCount, err := BigIntAt(rows[3], 0)
	if err != nil {
		return nil, err
	}
	if !assetCount.IsUint64() || assetCount.Uint64() > 32 {
		return nil, fmt.Errorf("earnETH asset count %s exceeds bound", assetCount)
	}
	shares, err := BigIntAt(rows[4], 0)
	if err != nil {
		return nil, err
	}
	if shares.Sign() == 0 {
		return nil, nil
	}

	assetCalls := make([]ContractCall, assetCount.Uint64())
	for index := range assetCalls {
		assetCalls[index] = ContractCall{
			Contract: lidoEarnETHVaultAddress,
			ABI:      lidoEarnVaultABI,
			Method:   "assetAt",
			Args:     []any{new(big.Int).SetUint64(uint64(index))},
		}
	}
	assetRows, err := client.ParallelCalls(ctx, block, assetCalls)
	if err != nil {
		return nil, err
	}
	foundWstETH := false
	for _, assetRow := range assetRows {
		asset, decodeErr := AddressAt(assetRow, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		foundWstETH = foundWstETH || asset == lidoWstETHAddress
	}
	if !foundWstETH {
		return nil, fmt.Errorf("earnETH asset registry omitted wstETH")
	}
	reportResult, err := client.Call(
		ctx,
		block,
		oracle,
		lidoEarnOracleABI,
		"getReport",
		lidoWstETHAddress,
	)
	if err != nil {
		return nil, err
	}
	report, err := decodeLidoEarnReport(reportResult[0])
	if err != nil {
		return nil, err
	}
	if report.PriceD18.Sign() == 0 || report.IsSuspicious {
		return nil, fmt.Errorf("earnETH wstETH report is unusable")
	}
	wad := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	wstETHAmount := new(big.Int).Quo(new(big.Int).Mul(shares, wad), report.PriceD18)
	converted, err := client.Call(
		ctx,
		block,
		lidoWstETHAddress,
		lidoWstETHABI,
		"getStETHByWstETH",
		wstETHAmount,
	)
	if err != nil {
		return nil, err
	}
	stETHAmount, err := BigIntAt(converted, 0)
	if err != nil {
		return nil, err
	}
	if stETHAmount.Sign() == 0 {
		return nil, nil
	}
	component := NewComponent(
		"asset",
		lidoStETHToken,
		stETHAmount,
		Source{Contract: lidoEarnETHVaultAddress, Method: "balanceOf + oracle.getReport(wstETH)"},
	)
	component.Metadata = map[string]any{"shares": shares.String(), "oracle": oracle}
	return &Group{
		ID:         "earn-eth",
		MarketID:   "earn-eth",
		Label:      "Lido earnETH",
		Components: []Component{component},
	}, nil
}

func (a *LidoAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if !lidoStETHDeployment.ActiveAt(block.Number) {
		return []Group{}, nil
	}
	balanceCalls := []ContractCall{
		{
			Contract: lidoStETHAddress,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{account},
		},
	}
	if lidoWstETHDeployment.ActiveAt(block.Number) {
		balanceCalls = append(balanceCalls, ContractCall{
			Contract: lidoWstETHAddress,
			ABI:      lidoWstETHABI,
			Method:   "balanceOf",
			Args:     []any{account},
		}, ContractCall{
			Contract: lidoWstETHAddress,
			ABI:      lidoWstETHABI,
			Method:   "stETH",
		})
	}
	rows, err := client.ParallelCalls(ctx, block, balanceCalls)
	if err != nil {
		return nil, err
	}
	stETHBalance, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	wstETHBalance := new(big.Int)
	if lidoWstETHDeployment.ActiveAt(block.Number) {
		wstETHBalance, err = BigIntAt(rows[1], 0)
		if err != nil {
			return nil, err
		}
		wrappedStETH, decodeErr := AddressAt(rows[2], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if wrappedStETH != lidoStETHAddress {
			return nil, fmt.Errorf("wstETH underlying changed to %s", wrappedStETH)
		}
	}
	withdrawals, err := readLidoWithdrawals(ctx, client, block, account)
	if err != nil {
		return nil, fmt.Errorf("withdrawals: %w", err)
	}
	earnETH, err := readLidoEarnETH(ctx, client, block, account)
	if err != nil {
		return nil, fmt.Errorf("earnETH: %w", err)
	}
	groups := make([]Group, 0, 4)
	if stETHBalance.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "steth",
			MarketID: "steth",
			Label:    "Lido stETH",
			Components: []Component{NewComponent(
				"asset",
				lidoETHToken,
				stETHBalance,
				Source{Contract: lidoStETHAddress, Method: "balanceOf"},
			)},
		})
	}
	if wstETHBalance.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx,
			block,
			lidoWstETHAddress,
			lidoWstETHABI,
			"getStETHByWstETH",
			wstETHBalance,
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
			lidoStETHToken,
			amount,
			Source{Contract: lidoWstETHAddress, Method: "getStETHByWstETH(balanceOf)"},
		)
		component.Metadata = map[string]any{"wstEthShares": wstETHBalance.String()}
		groups = append(groups, Group{
			ID:         "wsteth",
			MarketID:   "wsteth",
			Label:      "Lido wstETH",
			Components: []Component{component},
		})
	}
	if withdrawals != nil {
		groups = append(groups, *withdrawals)
	}
	if earnETH != nil {
		groups = append(groups, *earnETH)
	}
	return groups, nil
}
