package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var morphoCoreABI = MustABI(`[
	{"type":"function","name":"feeRecipient","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
	  {"type":"function","name":"position","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"},{"name":"user","type":"address"}],"outputs":[
    {"name":"supplyShares","type":"uint256"},{"name":"borrowShares","type":"uint128"},{"name":"collateral","type":"uint128"}
  ]},
  {"type":"function","name":"market","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"}],"outputs":[
    {"name":"totalSupplyAssets","type":"uint128"},{"name":"totalSupplyShares","type":"uint128"},
    {"name":"totalBorrowAssets","type":"uint128"},{"name":"totalBorrowShares","type":"uint128"},
    {"name":"lastUpdate","type":"uint128"},{"name":"fee","type":"uint128"}
  ]},
  {"type":"function","name":"idToMarketParams","stateMutability":"view","inputs":[{"name":"id","type":"bytes32"}],"outputs":[
    {"name":"loanToken","type":"address"},{"name":"collateralToken","type":"address"},
    {"name":"oracle","type":"address"},{"name":"irm","type":"address"},{"name":"lltv","type":"uint256"}
  ]}
]`)

var morphoFactoryABI = MustABI(`[
  {"type":"function","name":"isMetaMorpho","stateMutability":"view","inputs":[{"name":"candidate","type":"address"}],"outputs":[{"type":"bool"}]},
  {"type":"function","name":"isVaultV2","stateMutability":"view","inputs":[{"name":"candidate","type":"address"}],"outputs":[{"type":"bool"}]}
]`)

type morphoVaultFactory struct {
	Address common.Address
	Version morphoVaultVersion
	Window  deploymentWindow
}

type morphoDeployment struct {
	Morpho           common.Address
	Window           deploymentWindow
	VaultV1Factories []morphoVaultFactory
	VaultV2Factories []morphoVaultFactory
}

var morphoDeployments = map[ChainID]morphoDeployment{
	Ethereum: {
		Morpho: common.HexToAddress("0xBBBBBbbBBb9cC5e90e3b3Af64bdAF62C37EEFFCb"),
		Window: deploymentWindow{ActivationBlock: 18_883_124},
		VaultV1Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0xa9c3d3a366466fa809d1ae982fb2c46e5fc41101"), Version: morphoVaultV1, Window: deploymentWindow{ActivationBlock: 18_925_584}},
			{Address: common.HexToAddress("0x1897a8997241c1cd4bd0698647e4eb7213535c24"), Version: morphoVaultV1, Window: deploymentWindow{ActivationBlock: 21_439_510}},
		},
		VaultV2Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0xA1D94F746dEfa1928926b84fB2596c06926C0405"), Version: morphoVaultV2, Window: deploymentWindow{ActivationBlock: 23_375_073}},
		},
	},
	BSC: {
		Morpho: common.HexToAddress("0x01b0Bd309AA75547f7a37Ad7B1219A898E67a83a"),
		Window: deploymentWindow{ActivationBlock: 54_344_680},
		VaultV1Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0x92983687e672cA6d96530f9Dbe11a196cE905d72"), Version: morphoVaultV1, Window: deploymentWindow{ActivationBlock: 54_344_985}},
		},
	},
	Base: {
		Morpho: common.HexToAddress("0xBBBBBbbBBb9cC5e90e3b3Af64bdAF62C37EEFFCb"),
		Window: deploymentWindow{ActivationBlock: 13_977_148},
		VaultV1Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0xFf62A7c278C62eD665133147129245053Bbf5918"), Version: morphoVaultV1, Window: deploymentWindow{ActivationBlock: 23_928_808}},
		},
		VaultV2Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0x4501125508079A99ebBebCE205DeC9593C2b5857"), Version: morphoVaultV2, Window: deploymentWindow{ActivationBlock: 35_615_206}},
		},
	},
	Arbitrum: {
		Morpho: common.HexToAddress("0x6c247b1F6182318877311737BaC0844bAa518F5e"),
		Window: deploymentWindow{ActivationBlock: 296_446_593},
		VaultV1Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0x878988f5f561081deEa117717052164ea1Ef0c82"), Version: morphoVaultV1, Window: deploymentWindow{ActivationBlock: 296_447_195}},
		},
		VaultV2Factories: []morphoVaultFactory{
			{Address: common.HexToAddress("0x6b46fa3cc9EBF8aB230aBAc664E37F2966Bf7971"), Version: morphoVaultV2, Window: deploymentWindow{ActivationBlock: 387_016_724}},
		},
	},
}

type morphoCorePosition struct {
	MarketID          common.Hash
	SupplyShares      *big.Int
	BorrowShares      *big.Int
	Collateral        *big.Int
	TotalSupplyAssets *big.Int
	TotalSupplyShares *big.Int
	TotalBorrowAssets *big.Int
	TotalBorrowShares *big.Int
	LastUpdate        *big.Int
	LoanToken         common.Address
	CollateralToken   common.Address
	Oracle            common.Address
	IRM               common.Address
	LLTV              *big.Int
}

type morphoComponentDraft struct {
	Kind        string
	Token       common.Address
	Amount      *big.Int
	Denominator *big.Int
	Source      Source
	Metadata    map[string]any
}

type morphoGroupDraft struct {
	ID             string
	MarketID       string
	Label          string
	Components     []morphoComponentDraft
	NetValuePolicy string
	Metadata       map[string]any
}

type MorphoAdapter struct {
	adapterBase
	indexer morphoPositionIndexer
}

func newMorphoAdapter(config SentioIndexerConfig) *MorphoAdapter {
	return newMorphoAdapterWithIndexer(newMorphoIndexer(config))
}

func newMorphoAdapterWithIndexer(indexer morphoPositionIndexer) *MorphoAdapter {
	return &MorphoAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "morpho-blue", Name: "Morpho Blue", Chains: append([]ChainID(nil), SupportedChainIDs...),
		}},
		indexer: indexer,
	}
}

func morphoMarketID(
	loanToken common.Address,
	collateralToken common.Address,
	oracle common.Address,
	irm common.Address,
	lltv *big.Int,
) (common.Hash, error) {
	encoded, err := morphoCoreABI.Methods["idToMarketParams"].Outputs.Pack(
		loanToken, collateralToken, oracle, irm, lltv,
	)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(encoded), nil
}

func morphoStoredShareFraction(
	shares *big.Int,
	totalAssets *big.Int,
	totalShares *big.Int,
) (*big.Int, *big.Int) {
	numerator := new(big.Int).Mul(shares, new(big.Int).Add(totalAssets, big.NewInt(1)))
	denominator := new(big.Int).Add(totalShares, big.NewInt(1_000_000))
	return numerator, denominator
}

func readMorphoCorePositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	deployment morphoDeployment,
	marketIDs []common.Hash,
) ([]morphoCorePosition, error) {
	if len(marketIDs) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(marketIDs)*3)
	for _, marketID := range marketIDs {
		calls = append(calls,
			ContractCall{Contract: deployment.Morpho, ABI: morphoCoreABI, Method: "position", Args: []any{marketID, account}},
			ContractCall{Contract: deployment.Morpho, ABI: morphoCoreABI, Method: "market", Args: []any{marketID}},
			ContractCall{Contract: deployment.Morpho, ABI: morphoCoreABI, Method: "idToMarketParams", Args: []any{marketID}},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("core state: %w", err)
	}
	positions := make([]morphoCorePosition, 0, len(marketIDs))
	for index, marketID := range marketIDs {
		positionRow, marketRow, paramsRow := rows[index*3], rows[index*3+1], rows[index*3+2]
		position := morphoCorePosition{MarketID: marketID}
		var decodeErr error
		if position.SupplyShares, decodeErr = BigIntAt(positionRow, 0); decodeErr != nil {
			return nil, fmt.Errorf("market %s supply shares: %w", marketID, decodeErr)
		}
		if position.BorrowShares, decodeErr = BigIntAt(positionRow, 1); decodeErr != nil {
			return nil, fmt.Errorf("market %s borrow shares: %w", marketID, decodeErr)
		}
		if position.Collateral, decodeErr = BigIntAt(positionRow, 2); decodeErr != nil {
			return nil, fmt.Errorf("market %s collateral: %w", marketID, decodeErr)
		}
		if position.TotalSupplyAssets, decodeErr = BigIntAt(marketRow, 0); decodeErr != nil {
			return nil, decodeErr
		}
		if position.TotalSupplyShares, decodeErr = BigIntAt(marketRow, 1); decodeErr != nil {
			return nil, decodeErr
		}
		if position.TotalBorrowAssets, decodeErr = BigIntAt(marketRow, 2); decodeErr != nil {
			return nil, decodeErr
		}
		if position.TotalBorrowShares, decodeErr = BigIntAt(marketRow, 3); decodeErr != nil {
			return nil, decodeErr
		}
		if position.LastUpdate, decodeErr = BigIntAt(marketRow, 4); decodeErr != nil {
			return nil, decodeErr
		}
		if position.LoanToken, decodeErr = AddressAt(paramsRow, 0); decodeErr != nil {
			return nil, decodeErr
		}
		if position.CollateralToken, decodeErr = AddressAt(paramsRow, 1); decodeErr != nil {
			return nil, decodeErr
		}
		if position.Oracle, decodeErr = AddressAt(paramsRow, 2); decodeErr != nil {
			return nil, decodeErr
		}
		if position.IRM, decodeErr = AddressAt(paramsRow, 3); decodeErr != nil {
			return nil, decodeErr
		}
		if position.LLTV, decodeErr = BigIntAt(paramsRow, 4); decodeErr != nil {
			return nil, decodeErr
		}
		computedID, hashErr := morphoMarketID(
			position.LoanToken, position.CollateralToken, position.Oracle, position.IRM, position.LLTV,
		)
		if hashErr != nil || computedID != marketID || position.LastUpdate.Sign() == 0 {
			return nil, fmt.Errorf("index returned unknown Morpho market %s", marketID)
		}
		if position.SupplyShares.Sign() > 0 || position.BorrowShares.Sign() > 0 || position.Collateral.Sign() > 0 {
			positions = append(positions, position)
		}
	}
	return positions, nil
}

func morphoCoreDrafts(deployment morphoDeployment, positions []morphoCorePosition) []morphoGroupDraft {
	drafts := make([]morphoGroupDraft, 0, len(positions))
	for _, position := range positions {
		components := make([]morphoComponentDraft, 0, 3)
		if position.Collateral.Sign() > 0 {
			components = append(components, morphoComponentDraft{
				Kind: "asset", Token: position.CollateralToken, Amount: position.Collateral,
				Source:   Source{Contract: deployment.Morpho, Method: "position.collateral"},
				Metadata: map[string]any{"role": "collateral", "marketId": position.MarketID.Hex()},
			})
		}
		if position.SupplyShares.Sign() > 0 {
			numerator, denominator := morphoStoredShareFraction(
				position.SupplyShares, position.TotalSupplyAssets, position.TotalSupplyShares,
			)
			components = append(components, morphoComponentDraft{
				Kind: "asset", Token: position.LoanToken, Amount: numerator, Denominator: denominator,
				Source: Source{Contract: deployment.Morpho, Method: "stored supply-share ratio"},
				Metadata: map[string]any{
					"role": "supply", "marketId": position.MarketID.Hex(),
					"supplyShares": position.SupplyShares.String(),
				},
			})
		}
		if position.BorrowShares.Sign() > 0 {
			numerator, denominator := morphoStoredShareFraction(
				position.BorrowShares, position.TotalBorrowAssets, position.TotalBorrowShares,
			)
			components = append(components, morphoComponentDraft{
				Kind: "debt", Token: position.LoanToken, Amount: numerator, Denominator: denominator,
				Source: Source{Contract: deployment.Morpho, Method: "stored borrow-share ratio"},
				Metadata: map[string]any{
					"role": "borrow", "marketId": position.MarketID.Hex(),
					"borrowShares": position.BorrowShares.String(),
				},
			})
		}
		marketKey := "market:" + strings.ToLower(position.MarketID.Hex())
		label := "Yield"
		if position.BorrowShares.Sign() > 0 || position.Collateral.Sign() > 0 {
			label = "Lending"
		}
		drafts = append(drafts, morphoGroupDraft{
			ID: marketKey, MarketID: marketKey, Label: label, Components: components,
			NetValuePolicy: "floor-zero",
			Metadata: map[string]any{
				"marketId": position.MarketID.Hex(), "loanToken": position.LoanToken,
				"collateralToken": position.CollateralToken, "oracle": position.Oracle,
				"irm": position.IRM, "lltv": position.LLTV.String(),
				"marketLastUpdate": position.LastUpdate.String(), "accounting": "stored-share-ratio",
			},
		})
	}
	return drafts
}

func morphoVaultDrafts(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	vaults []morphoVaultRef,
) ([]morphoGroupDraft, error) {
	if len(vaults) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(vaults)*2)
	for _, vault := range vaults {
		calls = append(calls,
			ContractCall{Contract: vault.Address, ABI: erc4626ABI, Method: "asset"},
			ContractCall{Contract: vault.Address, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("vault headers: %w", err)
	}
	type activeVault struct {
		ref    morphoVaultRef
		asset  common.Address
		shares *big.Int
	}
	active := make([]activeVault, 0, len(vaults))
	for index, vault := range vaults {
		asset, decodeErr := AddressAt(rows[index*2], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s asset: %w", vault.Address, decodeErr)
		}
		shares, decodeErr := BigIntAt(rows[index*2+1], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s shares: %w", vault.Address, decodeErr)
		}
		if shares.Sign() > 0 {
			active = append(active, activeVault{ref: vault, asset: asset, shares: shares})
		}
	}
	convertCalls := make([]ContractCall, len(active))
	for index, vault := range active {
		convertCalls[index] = ContractCall{
			Contract: vault.ref.Address, ABI: erc4626ABI, Method: "convertToAssets", Args: []any{vault.shares},
		}
	}
	amountRows, err := client.ParallelCalls(ctx, block, convertCalls)
	if err != nil {
		return nil, fmt.Errorf("vault conversions: %w", err)
	}
	drafts := make([]morphoGroupDraft, 0, len(active))
	for index, vault := range active {
		amount, decodeErr := BigIntAt(amountRows[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s assets: %w", vault.ref.Address, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		key := "vault:" + strings.ToLower(vault.ref.Address.Hex())
		drafts = append(drafts, morphoGroupDraft{
			ID: key, MarketID: key, Label: "Yield",
			Components: []morphoComponentDraft{{
				Kind: "asset", Token: vault.asset, Amount: amount,
				Source: Source{Contract: vault.ref.Address, Method: "convertToAssets(balanceOf)"},
				Metadata: map[string]any{
					"vault": vault.ref.Address, "vaultVersion": string(vault.ref.Version),
					"shares": vault.shares.String(),
				},
			}},
			Metadata: map[string]any{"vault": vault.ref.Address, "vaultVersion": string(vault.ref.Version)},
		})
	}
	return drafts, nil
}

func attachMorphoTokenMetadata(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	drafts []morphoGroupDraft,
) ([]Group, error) {
	addresses := make([]common.Address, 0)
	for _, draft := range drafts {
		for _, component := range draft.Components {
			addresses = append(addresses, component.Token)
		}
	}
	tokens, err := tokenMetadataAt(ctx, client, block, addresses)
	if err != nil {
		return nil, err
	}
	groups := make([]Group, 0, len(drafts))
	for _, draft := range drafts {
		components := make([]Component, 0, len(draft.Components))
		for _, componentDraft := range draft.Components {
			tokenInfo, exists := tokens[componentDraft.Token]
			if !exists {
				return nil, fmt.Errorf("token metadata is absent for %s", componentDraft.Token)
			}
			component := NewComponent(
				componentDraft.Kind, tokenInfo, componentDraft.Amount, componentDraft.Source,
			)
			if componentDraft.Denominator != nil {
				component.AmountDenominatorRaw = componentDraft.Denominator.String()
			}
			component.Metadata = componentDraft.Metadata
			components = append(components, component)
		}
		groups = append(groups, Group{
			ID: draft.ID, MarketID: draft.MarketID, Label: draft.Label,
			Components: components, NetValuePolicy: draft.NetValuePolicy, Metadata: draft.Metadata,
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return groups, nil
}

func (a *MorphoAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := morphoDeployments[block.ChainID]
	if !exists || !deployment.Window.ActiveAt(block.Number) {
		return nil, nil
	}
	refs, err := a.indexer.PositionRefs(ctx, client, block, account, deployment)
	if err != nil {
		return nil, err
	}
	core, err := readMorphoCorePositions(
		ctx, client, block, account, deployment, refs.MarketIDs,
	)
	if err != nil {
		return nil, err
	}
	coreDrafts := morphoCoreDrafts(deployment, core)
	if err := expandMorphoStrategyCollateral(ctx, client, block, deployment, core, coreDrafts); err != nil {
		return nil, err
	}
	vaults, err := morphoVaultDrafts(ctx, client, block, account, refs.Vaults)
	if err != nil {
		return nil, err
	}
	drafts := append(coreDrafts, vaults...)
	groups, err := attachMorphoTokenMetadata(ctx, client, block, drafts)
	if err != nil {
		return nil, fmt.Errorf("token metadata: %w", err)
	}
	for index := range groups {
		if groups[index].Metadata == nil {
			groups[index].Metadata = make(map[string]any)
		}
		groups[index].Metadata["indexerBlock"] = refs.IndexerBlock
	}
	return groups, nil
}
