package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

var etherfiAccountantABI = MustABI(`[
  {"type":"function","name":"vault","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"base","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]},
  {"type":"function","name":"getRate","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getRateSafe","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

type etherfiVaultPosition struct {
	ID              string
	Label           string
	Vault           common.Address
	Accountant      common.Address
	ActivationBlock uint64
}

func etherfiVault(
	id string,
	label string,
	vault string,
	accountant string,
	activationBlock uint64,
) etherfiVaultPosition {
	return etherfiVaultPosition{
		ID:              id,
		Label:           label,
		Vault:           common.HexToAddress(vault),
		Accountant:      common.HexToAddress(accountant),
		ActivationBlock: activationBlock,
	}
}

func etherfiVaultAssets(shares, rate *big.Int, rateDecimals uint8) (*big.Int, error) {
	if shares == nil || shares.Sign() < 0 {
		return nil, fmt.Errorf("shares must be non-negative")
	}
	if rate == nil || rate.Sign() <= 0 {
		return nil, fmt.Errorf("accountant rate must be positive")
	}
	if rateDecimals > 77 {
		return nil, fmt.Errorf("accountant decimals %d exceed the safety bound", rateDecimals)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(rateDecimals)), nil)
	return new(big.Int).Quo(new(big.Int).Mul(new(big.Int).Set(shares), rate), scale), nil
}

func readEtherfiVaultPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	positions []etherfiVaultPosition,
) ([]Group, error) {
	active := make([]etherfiVaultPosition, 0, len(positions))
	balanceCalls := make([]ContractCall, 0, len(positions))
	for _, position := range positions {
		if block.Number < position.ActivationBlock {
			continue
		}
		active = append(active, position)
		balanceCalls = append(balanceCalls, ContractCall{
			Contract: position.Vault,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{account},
		})
	}
	if len(balanceCalls) == 0 {
		return nil, nil
	}
	balanceRows, err := client.ParallelCalls(ctx, block, balanceCalls)
	if err != nil {
		return nil, fmt.Errorf("vault share balances: %w", err)
	}

	type nonZeroPosition struct {
		position etherfiVaultPosition
		shares   *big.Int
	}
	nonZero := make([]nonZeroPosition, 0, len(active))
	for index, row := range balanceRows {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s share balance: %w", active[index].Vault, decodeErr)
		}
		if shares.Sign() != 0 {
			nonZero = append(nonZero, nonZeroPosition{position: active[index], shares: shares})
		}
	}
	if len(nonZero) == 0 {
		return nil, nil
	}

	headerCalls := make([]ContractCall, 0, len(nonZero)*5)
	for _, item := range nonZero {
		headerCalls = append(headerCalls,
			ContractCall{Contract: item.position.Accountant, ABI: etherfiAccountantABI, Method: "vault"},
			ContractCall{Contract: item.position.Accountant, ABI: etherfiAccountantABI, Method: "base"},
			ContractCall{Contract: item.position.Accountant, ABI: etherfiAccountantABI, Method: "decimals"},
			ContractCall{Contract: item.position.Accountant, ABI: etherfiAccountantABI, Method: "getRateSafe"},
			ContractCall{Contract: item.position.Accountant, ABI: etherfiAccountantABI, Method: "getRate"},
		)
	}
	headerRows, err := client.ParallelCallsAllowFailure(ctx, block, headerCalls)
	if err != nil {
		return nil, fmt.Errorf("vault accountant state: %w", err)
	}

	groups := make([]Group, 0, len(nonZero))
	for index, item := range nonZero {
		rows := headerRows[index*5 : index*5+5]
		for rowIndex, name := range []string{"vault", "base", "decimals"} {
			if rows[rowIndex].Error != nil {
				return groups, fmt.Errorf("vault %s accountant %s: %w", item.position.Vault, name, rows[rowIndex].Error)
			}
		}
		accountantVault, decodeErr := AddressAt(rows[0].Values, 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("vault %s accountant vault: %w", item.position.Vault, decodeErr)
		}
		if accountantVault != item.position.Vault {
			return groups, fmt.Errorf(
				"vault %s accountant points to %s",
				item.position.Vault,
				accountantVault,
			)
		}
		base, decodeErr := AddressAt(rows[1].Values, 0)
		if decodeErr != nil || base == (common.Address{}) {
			return groups, fmt.Errorf("vault %s accountant base is invalid", item.position.Vault)
		}
		rateDecimals, decodeErr := Uint8At(rows[2].Values, 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("vault %s accountant decimals: %w", item.position.Vault, decodeErr)
		}

		rateMethod := "getRateSafe"
		rateRow := rows[3]
		if rateRow.Error != nil {
			rateMethod = "getRate"
			rateRow = rows[4]
		}
		if rateRow.Error != nil {
			return groups, fmt.Errorf("vault %s accountant rate: %w", item.position.Vault, rateRow.Error)
		}
		rate, decodeErr := BigIntAt(rateRow.Values, 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("vault %s accountant rate: %w", item.position.Vault, decodeErr)
		}
		amount, amountErr := etherfiVaultAssets(item.shares, rate, rateDecimals)
		if amountErr != nil {
			return groups, fmt.Errorf("vault %s assets: %w", item.position.Vault, amountErr)
		}
		baseToken, tokenErr := readERC20Token(ctx, client, block, base)
		if tokenErr != nil {
			return groups, fmt.Errorf("vault %s base token: %w", item.position.Vault, tokenErr)
		}
		component := NewComponent(
			"asset",
			baseToken,
			amount,
			Source{Contract: item.position.Accountant, Method: rateMethod},
		)
		component.Metadata = map[string]any{
			"vault":            item.position.Vault,
			"accountant":       item.position.Accountant,
			"sharesRaw":        item.shares.String(),
			"rateRaw":          rate.String(),
			"rateDecimals":     rateDecimals,
			"shareBalanceCall": "balanceOf",
		}
		groups = append(groups, Group{
			ID:         item.position.ID,
			Label:      item.position.Label,
			Components: []Component{component},
		})
	}
	return groups, nil
}
