package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var fraxlendRegistryABI = MustABI(`[
  {"type":"function","name":"deployedPairsLength","stateMutability":"view","inputs":[],"outputs":[{"name":"length","type":"uint256"}]},
  {"type":"function","name":"getAllPairAddresses","stateMutability":"view","inputs":[],"outputs":[{"name":"pairs","type":"address[]"}]}
]`)

var fraxlendPairABI = MustABI(`[
  {"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"name":"symbol","type":"string"}]},
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"name":"asset","type":"address"}]},
  {"type":"function","name":"collateralContract","stateMutability":"view","inputs":[],"outputs":[{"name":"collateral","type":"address"}]},
  {
    "type":"function",
    "name":"getUserSnapshot",
    "stateMutability":"view",
    "inputs":[{"name":"account","type":"address"}],
    "outputs":[
      {"name":"assetShares","type":"uint256"},
      {"name":"borrowShares","type":"uint256"},
      {"name":"collateralBalance","type":"uint256"}
    ]
  },
  {
    "type":"function",
    "name":"toAssetAmount",
    "stateMutability":"view",
    "inputs":[
      {"name":"shares","type":"uint256"},
      {"name":"roundUp","type":"bool"},
      {"name":"previewInterest","type":"bool"}
    ],
    "outputs":[{"name":"amount","type":"uint256"}]
  },
  {
    "type":"function",
    "name":"toBorrowAmount",
    "stateMutability":"view",
    "inputs":[
      {"name":"shares","type":"uint256"},
      {"name":"roundUp","type":"bool"},
      {"name":"previewInterest","type":"bool"}
    ],
    "outputs":[{"name":"amount","type":"uint256"}]
  }
]`)

var fraxlendLegacyPairABI = MustABI(`[
  {
    "type":"function",
    "name":"toAssetAmount",
    "stateMutability":"view",
    "inputs":[
      {"name":"shares","type":"uint256"},
      {"name":"roundUp","type":"bool"}
    ],
    "outputs":[{"name":"amount","type":"uint256"}]
  },
  {
    "type":"function",
    "name":"toBorrowAmount",
    "stateMutability":"view",
    "inputs":[
      {"name":"shares","type":"uint256"},
      {"name":"roundUp","type":"bool"}
    ],
    "outputs":[{"name":"amount","type":"uint256"}]
  }
]`)

var (
	fraxlendRegistry       = common.HexToAddress("0xD6E9D27C75Afd88ad24Cd5EdccdC76fd2fc3A751")
	fraxlendRegistryWindow = availableFrom(15_993_000)
)

type fraxlendPairPosition struct {
	pair              common.Address
	assetShares       *big.Int
	borrowShares      *big.Int
	collateralBalance *big.Int
}

type FraxlendAdapter struct {
	adapterBase
}

func newFraxlendAdapter() *FraxlendAdapter {
	return &FraxlendAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID:     "fraxlend",
			Name:   "Fraxlend",
			Chains: []ChainID{Ethereum},
		}},
	}
}

func fraxlendStoredAmounts(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	row fraxlendPairPosition,
) (*big.Int, *big.Int, string, error) {
	v3Supply, v3SupplyErr := client.Call(
		ctx,
		block,
		row.pair,
		fraxlendPairABI,
		"toAssetAmount",
		row.assetShares,
		false,
		false,
	)
	v3Debt, v3DebtErr := client.Call(
		ctx,
		block,
		row.pair,
		fraxlendPairABI,
		"toBorrowAmount",
		row.borrowShares,
		true,
		false,
	)
	if (v3SupplyErr == nil) != (v3DebtErr == nil) {
		return nil, nil, "", fmt.Errorf(
			"%s exposes only one V3 conversion view: supply=%v debt=%v",
			row.pair,
			v3SupplyErr,
			v3DebtErr,
		)
	}
	if v3SupplyErr == nil {
		supply, err := BigIntAt(v3Supply, 0)
		if err != nil {
			return nil, nil, "", fmt.Errorf("%s V3 supply conversion: %w", row.pair, err)
		}
		debt, err := BigIntAt(v3Debt, 0)
		if err != nil {
			return nil, nil, "", fmt.Errorf("%s V3 debt conversion: %w", row.pair, err)
		}
		return supply, debt, "v3-stored", nil
	}

	legacySupply, legacySupplyErr := client.Call(
		ctx,
		block,
		row.pair,
		fraxlendLegacyPairABI,
		"toAssetAmount",
		row.assetShares,
		false,
	)
	legacyDebt, legacyDebtErr := client.Call(
		ctx,
		block,
		row.pair,
		fraxlendLegacyPairABI,
		"toBorrowAmount",
		row.borrowShares,
		true,
	)
	if legacySupplyErr != nil || legacyDebtErr != nil {
		return nil, nil, "", fmt.Errorf(
			"%s has neither complete V3 nor legacy conversion ABI: v3 supply=%v, v3 debt=%v, legacy supply=%v, legacy debt=%v",
			row.pair,
			v3SupplyErr,
			v3DebtErr,
			legacySupplyErr,
			legacyDebtErr,
		)
	}
	supply, err := BigIntAt(legacySupply, 0)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s legacy supply conversion: %w", row.pair, err)
	}
	debt, err := BigIntAt(legacyDebt, 0)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s legacy debt conversion: %w", row.pair, err)
	}
	return supply, debt, "legacy-stored", nil
}

func (a *FraxlendAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum || !fraxlendRegistryWindow.ActiveAt(block.Number) {
		return nil, nil
	}

	registryRows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: fraxlendRegistry, ABI: fraxlendRegistryABI, Method: "deployedPairsLength"},
		{Contract: fraxlendRegistry, ABI: fraxlendRegistryABI, Method: "getAllPairAddresses"},
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate Fraxlend pairs: %w", err)
	}
	declaredLength, err := BigIntAt(registryRows[0], 0)
	if err != nil {
		return nil, fmt.Errorf("decode Fraxlend pair count: %w", err)
	}
	if len(registryRows[1]) != 1 {
		return nil, fmt.Errorf("getAllPairAddresses returned %d fields", len(registryRows[1]))
	}
	pairs, err := decodeAddresses(registryRows[1][0])
	if err != nil {
		return nil, fmt.Errorf("decode Fraxlend pairs: %w", err)
	}
	if declaredLength.Cmp(big.NewInt(int64(len(pairs)))) != 0 {
		return nil, fmt.Errorf(
			"Fraxlend registry pair count mismatch at block %d: declared=%s returned=%d",
			block.Number,
			declaredLength,
			len(pairs),
		)
	}
	if len(pairs) > 256 {
		return nil, fmt.Errorf("Fraxlend pair count %d exceeds safety bound", len(pairs))
	}

	snapshotCalls := make([]ContractCall, 0, len(pairs))
	for _, pair := range pairs {
		snapshotCalls = append(snapshotCalls, ContractCall{
			Contract: pair,
			ABI:      fraxlendPairABI,
			Method:   "getUserSnapshot",
			Args:     []any{account},
		})
	}
	snapshotRows, err := client.ParallelCalls(ctx, block, snapshotCalls)
	if err != nil {
		return nil, fmt.Errorf("read Fraxlend user snapshots: %w", err)
	}
	active := make([]fraxlendPairPosition, 0)
	for index, snapshot := range snapshotRows {
		assetShares, decodeErr := BigIntAt(snapshot, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s asset shares: %w", pairs[index], decodeErr)
		}
		borrowShares, decodeErr := BigIntAt(snapshot, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s borrow shares: %w", pairs[index], decodeErr)
		}
		collateralBalance, decodeErr := BigIntAt(snapshot, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s collateral balance: %w", pairs[index], decodeErr)
		}
		if assetShares.Sign() == 0 && borrowShares.Sign() == 0 && collateralBalance.Sign() == 0 {
			continue
		}
		active = append(active, fraxlendPairPosition{
			pair:              pairs[index],
			assetShares:       assetShares,
			borrowShares:      borrowShares,
			collateralBalance: collateralBalance,
		})
	}
	if len(active) == 0 {
		return nil, nil
	}

	metadataCalls := make([]ContractCall, 0, len(active)*3)
	for _, row := range active {
		metadataCalls = append(metadataCalls,
			ContractCall{Contract: row.pair, ABI: fraxlendPairABI, Method: "asset"},
			ContractCall{Contract: row.pair, ABI: fraxlendPairABI, Method: "collateralContract"},
			ContractCall{Contract: row.pair, ABI: fraxlendPairABI, Method: "symbol"},
		)
	}
	metadataRows, err := client.ParallelCalls(ctx, block, metadataCalls)
	if err != nil {
		return nil, fmt.Errorf("read active Fraxlend pair metadata: %w", err)
	}

	tokens := make(map[common.Address]Token)
	readCachedToken := func(address common.Address) (Token, error) {
		if token, exists := tokens[address]; exists {
			return token, nil
		}
		token, tokenErr := readToken(ctx, client, block, address)
		if tokenErr != nil {
			return Token{}, tokenErr
		}
		tokens[address] = token
		return token, nil
	}

	groups := make([]Group, 0, len(active))
	for index, row := range active {
		assetAddress, decodeErr := AddressAt(metadataRows[index*3], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s asset: %w", row.pair, decodeErr)
		}
		collateralAddress, decodeErr := AddressAt(metadataRows[index*3+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s collateral: %w", row.pair, decodeErr)
		}
		label, decodeErr := StringAt(metadataRows[index*3+2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s symbol: %w", row.pair, decodeErr)
		}
		assetToken, tokenErr := readCachedToken(assetAddress)
		if tokenErr != nil {
			return nil, fmt.Errorf("%s asset metadata: %w", row.pair, tokenErr)
		}
		collateralToken, tokenErr := readCachedToken(collateralAddress)
		if tokenErr != nil {
			return nil, fmt.Errorf("%s collateral metadata: %w", row.pair, tokenErr)
		}
		supplied, borrowed, accountingMode, conversionErr := fraxlendStoredAmounts(
			ctx,
			client,
			block,
			row,
		)
		if conversionErr != nil {
			return nil, conversionErr
		}

		components := make([]Component, 0, 3)
		if supplied.Sign() > 0 {
			component := NewComponent(
				"asset",
				assetToken,
				supplied,
				Source{Contract: row.pair, Method: accountingMode},
			)
			component.Metadata = map[string]any{"shares": row.assetShares.String()}
			components = append(components, component)
		}
		if row.collateralBalance.Sign() > 0 {
			components = append(components, NewComponent(
				"asset",
				collateralToken,
				row.collateralBalance,
				Source{Contract: row.pair, Method: "getUserSnapshot"},
			))
		}
		if borrowed.Sign() > 0 {
			component := NewComponent(
				"debt",
				assetToken,
				borrowed,
				Source{Contract: row.pair, Method: accountingMode},
			)
			component.Metadata = map[string]any{"shares": row.borrowShares.String()}
			components = append(components, component)
		}
		groups = append(groups, Group{
			ID:             strings.ToLower(row.pair.Hex()),
			MarketID:       strings.ToLower(row.pair.Hex()),
			Label:          label,
			Components:     components,
			NetValuePolicy: "floor-zero",
			Metadata: map[string]any{
				"pair":           row.pair,
				"accountingMode": accountingMode,
			},
		})
	}
	return groups, nil
}
