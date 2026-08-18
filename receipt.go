package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type receiptPosition struct {
	ID              string
	Label           string
	Receipt         Token
	ActivationBlock uint64
}

func readReceiptPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	positions []receiptPosition,
) ([]Group, error) {
	active := make([]receiptPosition, 0, len(positions))
	calls := make([]ContractCall, 0, len(positions))
	for _, position := range positions {
		if block.Number < position.ActivationBlock {
			continue
		}
		active = append(active, position)
		calls = append(calls, ContractCall{
			Contract: position.Receipt.Address,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{account},
		})
	}
	if len(calls) == 0 {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("receipt balances: %w", err)
	}
	groups := make([]Group, 0, len(rows))
	for index, row := range rows {
		amount, err := BigIntAt(row, 0)
		if err != nil {
			return nil, fmt.Errorf("%s balance: %w", active[index].Label, err)
		}
		if amount.Sign() == 0 {
			continue
		}
		position := active[index]
		groups = append(groups, Group{
			ID:    position.ID,
			Label: position.Label,
			Components: []Component{NewComponent(
				"asset",
				position.Receipt,
				amount,
				Source{Contract: position.Receipt.Address, Method: "balanceOf"},
			)},
		})
	}
	return groups, nil
}
