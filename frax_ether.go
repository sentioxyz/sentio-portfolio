package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

const fraxFeePrecision = 1_000_000

var (
	fraxEtherV1Queue = common.HexToAddress("0x82bA8da44Cd5261762e629dd5c605b17715727bd")
	fraxEtherV2Queue = common.HexToAddress("0xfDC69e6BE352BD5644C438302DE4E311AAD5565b")
	fraxEtherETH     = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	fraxEtherFRXETH = token(
		Ethereum,
		"0x5E8422345238F34275888049021821E8E08CAa1f",
		"frxETH",
		18,
	)
	fraxEtherQueueV1ABI = MustABI(`[
      {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"type":"address"}]},
      {
        "type":"function","name":"nftInformation","stateMutability":"view",
        "inputs":[{"name":"nftId","type":"uint256"}],
        "outputs":[{"type":"bool"},{"type":"uint256"},{"type":"uint120"},{"type":"uint256"}]
      }
    ]`)
	fraxEtherQueueV2ABI = MustABI(`[
      {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"type":"address"}]},
      {
        "type":"function","name":"nftInformation","stateMutability":"view",
        "inputs":[{"name":"nftId","type":"uint256"}],
        "outputs":[{"type":"bool"},{"type":"uint256"},{"type":"uint120"},{"type":"uint256"},{"type":"uint120"}]
      }
    ]`)
)

type fraxEtherQueueConfig struct {
	Address         common.Address
	ActivationBlock uint64
	Version         int
}

type FraxEtherAdapter struct {
	adapterBase
	indexer  *ownerTokenIndexer
	receipts []convertedBalancePosition
	queues   []fraxEtherQueueConfig
}

func newFraxEtherAdapter(config SentioIndexerConfig) Adapter {
	return &FraxEtherAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "frax-ether", Name: "Frax Ether", Chains: []ChainID{Ethereum},
		}},
		indexer: newOwnerTokenIndexer(config, []ChainID{Ethereum}),
		receipts: []convertedBalancePosition{
			{
				ID: "frxeth", Label: "Liquid staking · frxETH",
				BalanceContract: fraxEtherFRXETH.Address,
				Token:           fraxEtherFRXETH,
				ActivationBlock: 15_686_046,
			},
			{
				ID: "sfrxeth", Label: "Yield · sfrxETH",
				BalanceContract: common.HexToAddress("0xac3E018457B222d93114458476f3E3416Abbe38F"),
				Converter:       common.HexToAddress("0xac3E018457B222d93114458476f3E3416Abbe38F"),
				Method:          "convertToAssets",
				Token:           fraxEtherFRXETH,
				ActivationBlock: 15_686_046,
			},
		},
		queues: []fraxEtherQueueConfig{
			{Address: fraxEtherV1Queue, ActivationBlock: 18_580_357, Version: 1},
			{Address: fraxEtherV2Queue, ActivationBlock: 21_404_228, Version: 2},
		},
	}
}

func (a *FraxEtherAdapter) activeQueues(block uint64) []fraxEtherQueueConfig {
	result := make([]fraxEtherQueueConfig, 0, len(a.queues))
	for _, queue := range a.queues {
		if block >= queue.ActivationBlock {
			result = append(result, queue)
		}
	}
	return result
}

func (a *FraxEtherAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups, err := readConvertedBalancePositions(ctx, client, block, account, a.receipts)
	if err != nil {
		return nil, err
	}
	queues := a.activeQueues(block.Number)
	if len(queues) == 0 {
		return groups, nil
	}
	queueAddresses := make([]common.Address, 0, len(queues))
	queueByAddress := make(map[common.Address]fraxEtherQueueConfig, len(queues))
	for _, queue := range queues {
		queueAddresses = append(queueAddresses, queue.Address)
		queueByAddress[queue.Address] = queue
	}
	refs, err := a.indexer.PositionRefs(ctx, client, block, account, queueAddresses)
	if err != nil {
		return groups, fmt.Errorf("redemption queue enumeration: %w", err)
	}
	for _, ref := range refs {
		queue := queueByAddress[ref.Contract]
		contractABI := fraxEtherQueueV1ABI
		if queue.Version == 2 {
			contractABI = fraxEtherQueueV2ABI
		}
		rows, callErr := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: ref.Contract, ABI: contractABI, Method: "ownerOf", Args: []any{ref.TokenID}},
			{Contract: ref.Contract, ABI: contractABI, Method: "nftInformation", Args: []any{ref.TokenID}},
		})
		if callErr != nil {
			return groups, fmt.Errorf("redemption ticket %s: %w", ref.TokenID, callErr)
		}
		owner, decodeErr := AddressAt(rows[0], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("redemption ticket %s owner: %w", ref.TokenID, decodeErr)
		}
		if owner != account {
			return groups, fmt.Errorf("redemption ticket %s ownership changed at pinned block", ref.TokenID)
		}
		redeemed, decodeErr := BoolAt(rows[1], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("redemption ticket %s state: %w", ref.TokenID, decodeErr)
		}
		maturity, decodeErr := BigIntAt(rows[1], 1)
		if decodeErr != nil {
			return groups, fmt.Errorf("redemption ticket %s maturity: %w", ref.TokenID, decodeErr)
		}
		amount, decodeErr := BigIntAt(rows[1], 2)
		if decodeErr != nil {
			return groups, fmt.Errorf("redemption ticket %s amount: %w", ref.TokenID, decodeErr)
		}
		fee, decodeErr := BigIntAt(rows[1], 3)
		if decodeErr != nil {
			return groups, fmt.Errorf("redemption ticket %s fee: %w", ref.TokenID, decodeErr)
		}
		if redeemed || amount.Sign() == 0 {
			continue
		}
		netAmount := new(big.Int).Set(amount)
		if queue.Version == 2 && fee.Sign() > 0 {
			feeAmount := new(big.Int).Mul(amount, fee)
			feeAmount.Div(feeAmount, big.NewInt(fraxFeePrecision))
			netAmount.Sub(netAmount, feeAmount)
		}
		component := NewComponent(
			"asset",
			fraxEtherETH,
			netAmount,
			Source{Contract: ref.Contract, Method: "nftInformation.amount"},
		)
		component.Metadata = map[string]any{
			"tokenId": ref.TokenID.String(), "grossAmount": amount.String(),
			"feeE6": fee.String(), "maturity": maturity.String(), "queueVersion": queue.Version,
		}
		groups = append(groups, Group{
			ID:    "redemption:" + ref.Contract.Hex() + ":" + ref.TokenID.String(),
			Label: "Redemption queue", Components: []Component{component},
			Metadata: map[string]any{"queue": ref.Contract, "version": queue.Version},
		})
	}
	return groups, nil
}
