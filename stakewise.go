package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var stakeWiseVaultABI = MustABI(`[
  {"type":"function","name":"getShares","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

type stakeWiseManifest struct {
	Version     int              `json:"version"`
	GeneratedAt string           `json:"generatedAt"`
	Source      string           `json:"source"`
	Scope       string           `json:"scope"`
	Vaults      []stakeWiseVault `json:"vaults"`
}

type stakeWiseVault struct {
	Address         common.Address `json:"address"`
	ActivationBlock uint64         `json:"activationBlock"`
}

//go:embed stakewise-vaults.json
var stakeWiseManifestJSON []byte

var stakeWiseDeployments = mustStakeWiseManifest()

func mustStakeWiseManifest() stakeWiseManifest {
	var manifest stakeWiseManifest
	if err := json.Unmarshal(stakeWiseManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode StakeWise manifest: %w", err))
	}
	if manifest.Version != 1 || manifest.GeneratedAt == "" || manifest.Source == "" || manifest.Scope == "" || len(manifest.Vaults) == 0 {
		panic(fmt.Errorf("invalid StakeWise manifest version=%d vaults=%d", manifest.Version, len(manifest.Vaults)))
	}
	seen := make(map[common.Address]struct{}, len(manifest.Vaults))
	for _, vault := range manifest.Vaults {
		if vault.Address == (common.Address{}) || vault.ActivationBlock == 0 {
			panic("StakeWise manifest contains an invalid vault")
		}
		if _, exists := seen[vault.Address]; exists {
			panic(fmt.Sprintf("StakeWise manifest contains duplicate vault %s", vault.Address))
		}
		seen[vault.Address] = struct{}{}
	}
	sort.Slice(manifest.Vaults, func(i, j int) bool {
		return strings.ToLower(manifest.Vaults[i].Address.Hex()) < strings.ToLower(manifest.Vaults[j].Address.Hex())
	})
	return manifest
}

type StakeWiseAdapter struct {
	adapterBase
	vaults []stakeWiseVault
}

func newStakeWiseAdapter() Adapter {
	return &StakeWiseAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "stakewise", Name: "StakeWise", Chains: []ChainID{Ethereum},
		}},
		vaults: stakeWiseDeployments.Vaults,
	}
}

func (a *StakeWiseAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	vaults := make([]stakeWiseVault, 0, len(a.vaults))
	for _, vault := range a.vaults {
		if vault.ActivationBlock <= block.Number {
			vaults = append(vaults, vault)
		}
	}
	calls := make([]ContractCall, len(vaults))
	for index, vault := range vaults {
		calls[index] = ContractCall{
			Contract: vault.Address, ABI: stakeWiseVaultABI, Method: "getShares", Args: []any{account},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("StakeWise verified-vault shares: %w", err)
	}
	type holding struct {
		vault  common.Address
		shares *big.Int
	}
	holdings := make([]holding, 0)
	convertCalls := make([]ContractCall, 0)
	for index, row := range rows {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("StakeWise vault %s shares: %w", vaults[index].Address, decodeErr)
		}
		if shares.Sign() == 0 {
			continue
		}
		holdings = append(holdings, holding{vault: vaults[index].Address, shares: shares})
		convertCalls = append(convertCalls, ContractCall{
			Contract: vaults[index].Address, ABI: stakeWiseVaultABI, Method: "convertToAssets", Args: []any{shares},
		})
	}
	if len(holdings) == 0 {
		return nil, nil
	}
	converted, err := client.ParallelCalls(ctx, block, convertCalls)
	if err != nil {
		return nil, fmt.Errorf("StakeWise held-vault conversions: %w", err)
	}
	eth := token(Ethereum, "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2", "ETH", 18)
	groups := make([]Group, 0, len(holdings))
	for index, holding := range holdings {
		assets, decodeErr := BigIntAt(converted[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("StakeWise vault %s assets: %w", holding.vault, decodeErr)
		}
		if assets.Sign() == 0 {
			continue
		}
		component := NewComponent("asset", eth, assets, Source{
			Contract: holding.vault, Method: "convertToAssets(getShares(account))",
		})
		component.Metadata = map[string]any{"shares": holding.shares.String()}
		groups = append(groups, Group{
			ID: strings.ToLower(holding.vault.Hex()), MarketID: strings.ToLower(holding.vault.Hex()),
			Label: "Staked ETH", Components: []Component{component},
			Metadata: map[string]any{"vault": holding.vault, "verified": true},
		})
	}
	return groups, nil
}
