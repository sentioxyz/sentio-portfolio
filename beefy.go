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

var beefyVaultABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getPricePerFullShare","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

type beefyManifestVault struct {
	ChainID  ChainID        `json:"chainId"`
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Vault    common.Address `json:"vault"`
	Asset    common.Address `json:"asset"`
	Symbol   string         `json:"symbol"`
	Decimals uint8          `json:"decimals"`
	// CreatedAt is upstream provenance metadata, not a deployment boundary.
	CreatedAt uint64 `json:"createdAt"`
	// ActivationBlock is the first block where balanceOf and
	// getPricePerFullShare are both safe for this adapter.
	ActivationBlock uint64 `json:"activationBlock"`
	Status          string `json:"status"`
}

type beefyManifest struct {
	Version     int                  `json:"version"`
	GeneratedAt string               `json:"generatedAt"`
	Source      string               `json:"source"`
	Scope       string               `json:"scope"`
	Vaults      []beefyManifestVault `json:"vaults"`
}

//go:embed beefy-vaults.json
var beefyManifestJSON []byte

var beefyDeployments = mustBeefyManifest()

func mustBeefyManifest() beefyManifest {
	var manifest beefyManifest
	if err := json.Unmarshal(beefyManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode Beefy manifest: %w", err))
	}
	if manifest.Version != 2 || manifest.GeneratedAt == "" || manifest.Source == "" || manifest.Scope == "" || len(manifest.Vaults) == 0 {
		panic(fmt.Errorf("invalid Beefy manifest version=%d vaults=%d", manifest.Version, len(manifest.Vaults)))
	}
	seen := make(map[ChainID]map[common.Address]struct{})
	for _, vault := range manifest.Vaults {
		if !supportsChain(SupportedChainIDs, vault.ChainID) || vault.ID == "" || vault.Name == "" ||
			vault.Vault == (common.Address{}) || vault.Asset == (common.Address{}) || vault.Symbol == "" ||
			vault.CreatedAt == 0 || vault.ActivationBlock == 0 ||
			(vault.Status != "active" && vault.Status != "eol") {
			panic(fmt.Sprintf("invalid Beefy vault entry %q on chain %d", vault.ID, vault.ChainID))
		}
		if seen[vault.ChainID] == nil {
			seen[vault.ChainID] = make(map[common.Address]struct{})
		}
		if _, exists := seen[vault.ChainID][vault.Vault]; exists {
			panic(fmt.Sprintf("duplicate Beefy vault %s on chain %d", vault.Vault, vault.ChainID))
		}
		seen[vault.ChainID][vault.Vault] = struct{}{}
	}
	return manifest
}

type BeefyAdapter struct {
	adapterBase
	vaults map[ChainID][]beefyManifestVault
}

func newBeefyAdapter() Adapter {
	vaults := make(map[ChainID][]beefyManifestVault)
	for _, vault := range beefyDeployments.Vaults {
		vaults[vault.ChainID] = append(vaults[vault.ChainID], vault)
	}
	for chainID := range vaults {
		sort.Slice(vaults[chainID], func(i, j int) bool { return vaults[chainID][i].ID < vaults[chainID][j].ID })
	}
	return &BeefyAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "beefy", Name: "Beefy", Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		vaults: vaults,
	}
}

func activeBeefyVaults(vaults []beefyManifestVault, block uint64) []beefyManifestVault {
	active := make([]beefyManifestVault, 0, len(vaults))
	for _, vault := range vaults {
		if vault.ActivationBlock <= block {
			active = append(active, vault)
		}
	}
	return active
}

func beefyUnderlyingAmount(shares, pricePerFullShare *big.Int) *big.Int {
	return new(big.Int).Div(new(big.Int).Mul(shares, pricePerFullShare), big.NewInt(1_000_000_000_000_000_000))
}

func (a *BeefyAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	vaults := activeBeefyVaults(a.vaults[block.ChainID], block.Number)
	if len(vaults) == 0 {
		return nil, nil
	}
	balances, err := client.ParallelCalls(ctx, block, func() []ContractCall {
		calls := make([]ContractCall, len(vaults))
		for index, vault := range vaults {
			calls[index] = ContractCall{Contract: vault.Vault, ABI: beefyVaultABI, Method: "balanceOf", Args: []any{account}}
		}
		return calls
	}())
	if err != nil {
		return nil, fmt.Errorf("Beefy vault balances: %w", err)
	}

	type holding struct {
		vault  beefyManifestVault
		shares *big.Int
	}
	holdings := make([]holding, 0)
	stateCalls := make([]ContractCall, 0)
	for index, row := range balances {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Beefy vault %s balance: %w", vaults[index].ID, decodeErr)
		}
		if shares.Sign() == 0 {
			continue
		}
		holdings = append(holdings, holding{vault: vaults[index], shares: shares})
		stateCalls = append(stateCalls, ContractCall{
			Contract: vaults[index].Vault, ABI: beefyVaultABI, Method: "getPricePerFullShare",
		})
	}
	if len(holdings) == 0 {
		return nil, nil
	}
	state, err := client.ParallelCalls(ctx, block, stateCalls)
	if err != nil {
		return nil, fmt.Errorf("Beefy held-vault state: %w", err)
	}
	groups := make([]Group, 0, len(holdings))
	for index, holding := range holdings {
		pricePerFullShare, decodeErr := BigIntAt(state[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Beefy vault %s price per share: %w", holding.vault.ID, decodeErr)
		}
		amount := beefyUnderlyingAmount(holding.shares, pricePerFullShare)
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent("asset", Token{
			ChainID: block.ChainID, Address: holding.vault.Asset,
			Symbol: holding.vault.Symbol, Decimals: holding.vault.Decimals,
		}, amount, Source{Contract: holding.vault.Vault, Method: "balanceOf*getPricePerFullShare/1e18"})
		component.Metadata = map[string]any{
			"shares": holding.shares.String(), "pricePerFullShare": pricePerFullShare.String(),
		}
		groups = append(groups, Group{
			ID: strings.ToLower(holding.vault.Vault.Hex()), MarketID: holding.vault.ID,
			Label: "Yield · " + holding.vault.Name, Components: []Component{component},
			Metadata: map[string]any{"vault": holding.vault.Vault, "status": holding.vault.Status},
		})
	}
	return groups, nil
}
