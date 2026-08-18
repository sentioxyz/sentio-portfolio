package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

const etherfiMembershipActivationBlock = 17_664_328

var (
	etherfiMembershipNFT     = common.HexToAddress("0xb49e4420eA6e35F98060Cd133842DbeA9c27e479")
	etherfiMembershipManager = common.HexToAddress("0x3d320286E014C3e1ce99Af6d6B00f0C1D63E3000")
	etherfiMembershipABI     = MustABI(`[
      {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"valueOf","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"membershipManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
    ]`)
)

func (a *EtherfiAdapter) readMemberships(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum || block.Number < etherfiMembershipActivationBlock {
		return nil, nil
	}
	refs, err := a.indexer.PositionRefsERC1155(
		ctx, client, block, account, []common.Address{etherfiMembershipNFT},
	)
	if err != nil {
		return nil, fmt.Errorf("membership enumeration: %w", err)
	}
	if len(refs) == 0 {
		return nil, nil
	}
	managerRow, err := client.Call(
		ctx, block, etherfiMembershipNFT, etherfiMembershipABI, "membershipManager",
	)
	if err != nil {
		return nil, fmt.Errorf("membership manager: %w", err)
	}
	manager, err := AddressAt(managerRow, 0)
	if err != nil || manager != etherfiMembershipManager {
		return nil, fmt.Errorf("membership manager identity changed")
	}
	calls := make([]ContractCall, 0, len(refs)*2)
	for _, ref := range refs {
		calls = append(calls,
			ContractCall{
				Contract: etherfiMembershipNFT, ABI: etherfiMembershipABI,
				Method: "balanceOf", Args: []any{account, ref.TokenID},
			},
			ContractCall{
				Contract: etherfiMembershipNFT, ABI: etherfiMembershipABI,
				Method: "valueOf", Args: []any{ref.TokenID},
			},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("membership state: %w", err)
	}
	groups := make([]Group, 0, len(refs))
	for index, ref := range refs {
		balance, decodeErr := BigIntAt(rows[index*2], 0)
		if decodeErr != nil || balance.Cmp(big.NewInt(1)) != 0 {
			return groups, fmt.Errorf("membership %s does not match the pinned index", ref.TokenID)
		}
		amount, decodeErr := BigIntAt(rows[index*2+1], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("membership %s value: %w", ref.TokenID, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent(
			"asset", etherfiETH, amount,
			Source{Contract: etherfiMembershipNFT, Method: "valueOf"},
		)
		component.Metadata = map[string]any{
			"tokenId": ref.TokenID.String(), "erc1155BalanceRaw": balance.String(),
		}
		groups = append(groups, Group{
			ID: "membership:" + ref.TokenID.String(), Label: "Membership #" + ref.TokenID.String(),
			Components: []Component{component},
			Metadata: map[string]any{
				"membershipNFT": etherfiMembershipNFT, "membershipManager": manager,
			},
		})
	}
	return groups, nil
}
