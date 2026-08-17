package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

var instadappListABI = MustABI(`[
  {"type":"function","name":"userLink","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint64"},{"type":"uint64"},{"type":"uint64"}]},
  {"type":"function","name":"userList","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"accountId","type":"uint64"}],"outputs":[{"type":"uint64"},{"type":"uint64"}]},
  {"type":"function","name":"accountAddr","stateMutability":"view","inputs":[{"name":"accountId","type":"uint64"}],"outputs":[{"type":"address"}]}
]`)

type attributedAccount struct {
	Address     common.Address
	Attribution string
	Source      string
}

func resolveAccountScope(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]attributedAccount, error) {
	direct := attributedAccount{Address: owner, Attribution: "wallet", Source: "direct"}
	if block.ChainID != Ethereum {
		return []attributedAccount{direct}, nil
	}
	list := common.HexToAddress("0x4c8a1BEb8a87765788946D6B19C6C6355194AbEb")
	header, err := client.Call(ctx, block, list, instadappListABI, "userLink", owner)
	if err != nil {
		return nil, fmt.Errorf("enumerate Instadapp accounts: %w", err)
	}
	first, err := Uint64At(header, 0)
	if err != nil {
		return nil, err
	}
	last, err := Uint64At(header, 1)
	if err != nil {
		return nil, err
	}
	count, err := Uint64At(header, 2)
	if err != nil {
		return nil, err
	}
	if count > 256 {
		return nil, fmt.Errorf("Instadapp account count %d exceeds safety bound", count)
	}
	if count == 0 {
		if first != 0 || last != 0 {
			return nil, fmt.Errorf("Instadapp empty list has non-empty endpoints")
		}
		return []attributedAccount{direct}, nil
	}
	if first == 0 || last == 0 {
		return nil, fmt.Errorf("Instadapp non-empty list has empty endpoint")
	}

	result := []attributedAccount{direct}
	seenIDs := make(map[uint64]struct{}, count)
	seenAddresses := map[common.Address]struct{}{owner: {}}
	current := first
	var previous uint64
	for offset := uint64(0); offset < count; offset++ {
		if current == 0 {
			return nil, fmt.Errorf("Instadapp account list ended early")
		}
		if _, exists := seenIDs[current]; exists {
			return nil, fmt.Errorf("Instadapp account list contains a cycle")
		}
		seenIDs[current] = struct{}{}
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{
				Contract: list,
				ABI:      instadappListABI,
				Method:   "accountAddr",
				Args:     []any{current},
			},
			{
				Contract: list,
				ABI:      instadappListABI,
				Method:   "userList",
				Args:     []any{owner, current},
			},
		})
		if err != nil {
			return nil, err
		}
		address, err := AddressAt(rows[0], 0)
		if err != nil {
			return nil, err
		}
		edgePrevious, err := Uint64At(rows[1], 0)
		if err != nil {
			return nil, err
		}
		edgeNext, err := Uint64At(rows[1], 1)
		if err != nil {
			return nil, err
		}
		if edgePrevious != previous {
			return nil, fmt.Errorf("Instadapp account list has inconsistent previous edge")
		}
		if address == (common.Address{}) {
			return nil, fmt.Errorf("Instadapp account list contains zero address")
		}
		if _, exists := seenAddresses[address]; exists {
			return nil, fmt.Errorf("Instadapp account list contains duplicate address")
		}
		seenAddresses[address] = struct{}{}
		result = append(result, attributedAccount{
			Address:     address,
			Attribution: "instadapp-dsa",
			Source:      "instadapp-list",
		})
		previous = current
		current = edgeNext
	}
	if previous != last || current != 0 {
		return nil, fmt.Errorf("Instadapp account list does not match declared tail")
	}
	return result, nil
}
