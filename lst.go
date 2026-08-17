package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

var convertedBalanceABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getStETHByWstETH","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"underlyingBalanceFromShares","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"mETHToETH","stateMutability":"view","inputs":[{"name":"amount","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getEthValue","stateMutability":"view","inputs":[{"name":"amount","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

type convertedBalancePosition struct {
	ID              string
	Label           string
	BalanceContract common.Address
	Converter       common.Address
	Method          string
	Token           Token
	ActivationBlock uint64
}

type ConvertedBalanceAdapter struct {
	adapterBase
	positions map[ChainID][]convertedBalancePosition
}

func NewConvertedBalanceAdapter(
	id string,
	name string,
	positions map[ChainID][]convertedBalancePosition,
) *ConvertedBalanceAdapter {
	chains := make([]ChainID, 0, len(positions))
	for _, chainID := range SupportedChainIDs {
		if len(positions[chainID]) > 0 {
			chains = append(chains, chainID)
		}
	}
	return &ConvertedBalanceAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: id, Name: name, Chains: chains}},
		positions:   positions,
	}
}

func (a *ConvertedBalanceAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0)
	for _, position := range a.positions[block.ChainID] {
		if block.Number < position.ActivationBlock {
			continue
		}
		row, err := client.Call(
			ctx,
			block,
			position.BalanceContract,
			convertedBalanceABI,
			"balanceOf",
			account,
		)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", position.Label, err)
		}
		shares, err := BigIntAt(row, 0)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", position.Label, err)
		}
		if shares.Sign() == 0 {
			continue
		}
		amount := shares
		source := Source{Contract: position.BalanceContract, Method: "balanceOf"}
		if position.Method != "" {
			converted, err := client.Call(
				ctx,
				block,
				position.Converter,
				convertedBalanceABI,
				position.Method,
				shares,
			)
			if err != nil {
				return nil, fmt.Errorf("%s conversion: %w", position.Label, err)
			}
			amount, err = BigIntAt(converted, 0)
			if err != nil {
				return nil, fmt.Errorf("%s conversion: %w", position.Label, err)
			}
			source = Source{
				Contract: position.Converter,
				Method:   position.Method + "(balanceOf)",
			}
		}
		component := NewComponent("asset", position.Token, amount, source)
		if position.Method != "" {
			component.Metadata = map[string]any{"shares": shares.String()}
		}
		groups = append(groups, Group{
			ID:         position.ID,
			Label:      position.Label,
			Components: []Component{component},
		})
	}
	return groups, nil
}

func lstAdapters() []Adapter {
	eth := token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	return []Adapter{
		NewConvertedBalanceAdapter(
			"liquid-collective",
			"Liquid Collective",
			map[ChainID][]convertedBalancePosition{
				Ethereum: {{
					ID:              "lseth",
					Label:           "Yield · LsETH",
					BalanceContract: common.HexToAddress("0x8c1BEd5b9a0928467c9B1341Da1D7BD5e10b6549"),
					Converter:       common.HexToAddress("0x8c1BEd5b9a0928467c9B1341Da1D7BD5e10b6549"),
					Method:          "underlyingBalanceFromShares",
					Token:           eth,
				}},
			},
		),
	}
}
