package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	rocketStorageAddress = common.HexToAddress("0x1d8f8f00cfa6758d7bE78336684788Fb0ee0Fa46")
	rocketRETHAddress    = common.HexToAddress("0xae78736Cd615f374D3085123A210448E74Fc6393")
	rocketRPLAddress     = common.HexToAddress("0xD33526068D116cE69F19A9ee46F0bd304F21A51f")
	rocketETHToken       = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	rocketRPLToken = token(Ethereum, rocketRPLAddress.Hex(), "RPL", 18)
)

var rocketStorageABI = MustABI(`[
  {"type":"function","name":"getAddress","stateMutability":"view","inputs":[{"name":"key","type":"bytes32"}],"outputs":[{"type":"address"}]}
]`)

var rocketRETHABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getEthValue","stateMutability":"view","inputs":[{"name":"amount","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var rocketNodeManagerABI = MustABI(`[
  {"type":"function","name":"getNodeExists","stateMutability":"view","inputs":[{"name":"node","type":"address"}],"outputs":[{"type":"bool"}]}
]`)

var rocketNodeStakingABI = MustABI(`[
  {"type":"function","name":"getNodeMinipoolETHBonded","stateMutability":"view","inputs":[{"name":"node","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getNodeStakedRPL","stateMutability":"view","inputs":[{"name":"node","type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

type RocketPoolAdapter struct {
	adapterBase
}

func newRocketPoolAdapter() Adapter {
	return &RocketPoolAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "rocketpool", Name: "Rocket Pool", Chains: []ChainID{Ethereum},
	}}}
}

func rocketRegistryKey(name string) common.Hash {
	return crypto.Keccak256Hash([]byte("contract.address" + name))
}

func resolveRocketPoolContracts(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) (common.Address, common.Address, error) {
	names := []string{
		"rocketTokenRETH",
		"rocketTokenRPL",
		"rocketNodeManager",
		"rocketNodeStaking",
	}
	calls := make([]ContractCall, len(names))
	for index, name := range names {
		calls[index] = ContractCall{
			Contract: rocketStorageAddress,
			ABI:      rocketStorageABI,
			Method:   "getAddress",
			Args:     []any{rocketRegistryKey(name)},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return common.Address{}, common.Address{}, err
	}
	addresses := make([]common.Address, len(rows))
	for index, row := range rows {
		addresses[index], err = AddressAt(row, 0)
		if err != nil {
			return common.Address{}, common.Address{}, err
		}
		if addresses[index] == (common.Address{}) {
			return common.Address{}, common.Address{}, fmt.Errorf(
				"registry returned zero address for %s",
				names[index],
			)
		}
	}
	if addresses[0] != rocketRETHAddress || addresses[1] != rocketRPLAddress {
		return common.Address{}, common.Address{}, fmt.Errorf(
			"registry token identity changed: rETH=%s RPL=%s",
			addresses[0],
			addresses[1],
		)
	}
	return addresses[2], addresses[3], nil
}

func (a *RocketPoolAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	nodeManager, nodeStaking, err := resolveRocketPoolContracts(ctx, client, block)
	if err != nil {
		return nil, err
	}
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{
			Contract: rocketRETHAddress,
			ABI:      rocketRETHABI,
			Method:   "balanceOf",
			Args:     []any{account},
		},
		{
			Contract: nodeManager,
			ABI:      rocketNodeManagerABI,
			Method:   "getNodeExists",
			Args:     []any{account},
		},
	})
	if err != nil {
		return nil, err
	}
	rethBalance, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	nodeExists, err := BoolAt(rows[1], 0)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0, 3)
	if rethBalance.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx,
			block,
			rocketRETHAddress,
			rocketRETHABI,
			"getEthValue",
			rethBalance,
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
			rocketETHToken,
			amount,
			Source{Contract: rocketRETHAddress, Method: "getEthValue(balanceOf)"},
		)
		component.Metadata = map[string]any{"rethBalance": rethBalance.String()}
		groups = append(groups, Group{
			ID:         "reth",
			MarketID:   "reth",
			Label:      "Rocket Pool rETH",
			Components: []Component{component},
		})
	}
	if !nodeExists {
		return groups, nil
	}
	nodeRows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{
			Contract: nodeStaking,
			ABI:      rocketNodeStakingABI,
			Method:   "getNodeMinipoolETHBonded",
			Args:     []any{account},
		},
		{
			Contract: nodeStaking,
			ABI:      rocketNodeStakingABI,
			Method:   "getNodeStakedRPL",
			Args:     []any{account},
		},
	})
	if err != nil {
		return nil, err
	}
	bondedETH, err := BigIntAt(nodeRows[0], 0)
	if err != nil {
		return nil, err
	}
	stakedRPL, err := BigIntAt(nodeRows[1], 0)
	if err != nil {
		return nil, err
	}
	if bondedETH.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "node-bond",
			MarketID: "node-bond",
			Label:    "Rocket Pool node bond",
			Components: []Component{NewComponent(
				"asset",
				rocketETHToken,
				bondedETH,
				Source{Contract: nodeStaking, Method: "getNodeMinipoolETHBonded"},
			)},
		})
	}
	if stakedRPL.Sign() > 0 {
		groups = append(groups, Group{
			ID:       "node-rpl",
			MarketID: "node-rpl",
			Label:    "Rocket Pool node RPL stake",
			Components: []Component{NewComponent(
				"asset",
				rocketRPLToken,
				stakedRPL,
				Source{Contract: nodeStaking, Method: "getNodeStakedRPL"},
			)},
		})
	}
	return groups, nil
}
