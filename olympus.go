package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

var olympusABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"balanceFrom","stateMutability":"view","inputs":[{"name":"amount","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"gOHM","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"stakingContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"collateralToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"debtToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"accountCollateral","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint128"}]},
  {"type":"function","name":"accountDebt","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint128"}]}
]`)

type OlympusAdapter struct {
	adapterBase
}

func (a *OlympusAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	const (
		stakingActivation = 13_803_969
		coolerActivation  = 22_423_121
	)
	if block.Number < stakingActivation {
		return nil, nil
	}
	sOhm := common.HexToAddress("0x04906695D6D12CF5459975d7C3C03356E4Ccd460")
	staking := common.HexToAddress("0xB63cac384247597756545b500253ff8E607a8020")
	gOhm := common.HexToAddress("0x0ab87046fBb341D058F17CBC4c1133F25a20a52f")
	monoCooler := common.HexToAddress("0xdb591Ea2e5Db886dA872654D58f6cc584b68e7cC")
	usds := common.HexToAddress("0xdC035D45d973E3EC169d2276DDab16f1e407384F")

	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: sOhm, ABI: olympusABI, Method: "balanceOf", Args: []any{account}},
		{Contract: sOhm, ABI: olympusABI, Method: "gOHM"},
		{Contract: sOhm, ABI: olympusABI, Method: "stakingContract"},
		{Contract: gOhm, ABI: olympusABI, Method: "balanceOf", Args: []any{account}},
	})
	if err != nil {
		return nil, fmt.Errorf("sOHM position: %w", err)
	}
	balance, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	actualGOhm, err := AddressAt(rows[1], 0)
	if err != nil {
		return nil, err
	}
	actualStaking, err := AddressAt(rows[2], 0)
	if err != nil {
		return nil, err
	}
	if actualGOhm != gOhm || actualStaking != staking {
		return nil, fmt.Errorf("sOHM contract wiring changed")
	}
	gOhmBalance, err := BigIntAt(rows[3], 0)
	if err != nil {
		return nil, err
	}
	ohmToken := token(
		Ethereum,
		"0x64aa3364F17a4D01c6f1751Fd97C2BD3D7e7f1D5",
		"OHM",
		9,
	)
	groups := make([]Group, 0, 3)
	if balance.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "sohm-staking",
			MarketID: "sohm-staking",
			Label:    "Staked · sOHM",
			Components: []Component{NewComponent(
				"asset",
				ohmToken,
				balance,
				Source{Contract: sOhm, Method: "balanceOf"},
			)},
		})
	}
	// gOHM is the non-rebasing wrapper of the staked position, so a wallet balance is a
	// staking position the same way wstETH is a Lido position; convert it to OHM at the
	// wrapper's own rate, matching the wstETH treatment in the Lido adapter.
	if gOhmBalance.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx, block, gOhm, olympusABI, "balanceFrom", gOhmBalance,
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
			ohmToken,
			amount,
			Source{Contract: gOhm, Method: "balanceFrom(balanceOf)"},
		)
		component.Metadata = map[string]any{"gOhmBalance": gOhmBalance.String()}
		groups = append(groups, Group{
			ID:         "gohm",
			MarketID:   "gohm",
			Label:      "Staked · gOHM",
			Components: []Component{component},
		})
	}
	if block.Number < coolerActivation {
		return groups, nil
	}
	coolerRows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: monoCooler, ABI: olympusABI, Method: "collateralToken"},
		{Contract: monoCooler, ABI: olympusABI, Method: "debtToken"},
		{Contract: monoCooler, ABI: olympusABI, Method: "accountCollateral", Args: []any{account}},
		{Contract: monoCooler, ABI: olympusABI, Method: "accountDebt", Args: []any{account}},
	})
	if err != nil {
		return nil, fmt.Errorf("MonoCooler position: %w", err)
	}
	actualCollateral, err := AddressAt(coolerRows[0], 0)
	if err != nil {
		return nil, err
	}
	actualDebt, err := AddressAt(coolerRows[1], 0)
	if err != nil {
		return nil, err
	}
	if actualCollateral != gOhm || actualDebt != usds {
		return nil, fmt.Errorf("MonoCooler token wiring changed")
	}
	collateral, err := BigIntAt(coolerRows[2], 0)
	if err != nil {
		return nil, err
	}
	debt, err := BigIntAt(coolerRows[3], 0)
	if err != nil {
		return nil, err
	}
	components := make([]Component, 0, 2)
	if collateral.Sign() > 0 {
		components = append(components, NewComponent(
			"asset",
			token(Ethereum, gOhm.Hex(), "gOHM", 18),
			collateral,
			Source{Contract: monoCooler, Method: "accountCollateral"},
		))
	}
	if debt.Sign() > 0 {
		components = append(components, NewComponent(
			"debt",
			token(Ethereum, usds.Hex(), "USDS", 18),
			debt,
			Source{Contract: monoCooler, Method: "accountDebt"},
		))
	}
	if len(components) > 0 {
		groups = append(groups, Group{
			ID:         "mono-cooler",
			MarketID:   "mono-cooler",
			Label:      "Lending · MonoCooler",
			Components: components,
		})
	}
	return groups, nil
}

func newOlympusAdapter() Adapter {
	return &OlympusAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "olympus", Name: "Olympus", Chains: []ChainID{Ethereum},
	}}}
}
