package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type vaultConfig struct {
	ID              string
	Label           string
	Address         common.Address
	Asset           Token
	ActivationBlock uint64
	CooldownID      string
}

type ERC4626Adapter struct {
	adapterBase
	vaults map[ChainID][]vaultConfig
}

func NewERC4626Adapter(
	id string,
	name string,
	vaults map[ChainID][]vaultConfig,
) *ERC4626Adapter {
	return &ERC4626Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: id, Name: name, Chains: deploymentChains(vaults),
		}},
		vaults: vaults,
	}
}

func (a *ERC4626Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	return readVaultPositions(ctx, client, block, account, a.vaults[block.ChainID])
}

func readVaultPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	vaults []vaultConfig,
) ([]Group, error) {
	groups := make([]Group, 0)
	for _, vault := range activeVaultsAt(vaults, block.Number) {
		headerCalls := []ContractCall{
			{Contract: vault.Address, ABI: erc4626ABI, Method: "asset"},
			{Contract: vault.Address, ABI: erc4626ABI, Method: "balanceOf", Args: []any{account}},
		}
		if vault.CooldownID != "" {
			headerCalls = append(headerCalls, ContractCall{
				Contract: vault.Address,
				ABI:      erc4626ABI,
				Method:   "cooldowns",
				Args:     []any{account},
			})
		}
		header, err := client.ParallelCalls(ctx, block, headerCalls)
		if err != nil {
			return nil, fmt.Errorf("%s vault header: %w", vault.Label, err)
		}
		asset, err := AddressAt(header[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s asset: %w", vault.Label, err)
		}
		if asset != vault.Asset.Address {
			return nil, fmt.Errorf(
				"%s asset changed from %s to %s",
				vault.Label,
				vault.Asset.Address,
				asset,
			)
		}
		shares, err := BigIntAt(header[1], 0)
		if err != nil {
			return nil, fmt.Errorf("%s shares: %w", vault.Label, err)
		}
		if shares.Sign() > 0 {
			converted, convertErr := client.Call(
				ctx,
				block,
				vault.Address,
				erc4626ABI,
				"convertToAssets",
				shares,
			)
			if convertErr != nil {
				return nil, fmt.Errorf("%s convert shares: %w", vault.Label, convertErr)
			}
			amount, decodeErr := BigIntAt(converted, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("%s assets: %w", vault.Label, decodeErr)
			}
			if amount.Sign() > 0 {
				component := NewComponent(
					"asset",
					vault.Asset,
					amount,
					Source{Contract: vault.Address, Method: "convertToAssets(balanceOf)"},
				)
				component.Metadata = map[string]any{"shares": shares.String()}
				groups = append(groups, Group{
					ID:         vault.ID,
					MarketID:   vault.ID,
					Label:      vault.Label,
					Components: []Component{component},
					Metadata:   map[string]any{"vault": vault.Address},
				})
			}
		}
		if vault.CooldownID != "" {
			cooldownEnd, decodeErr := BigIntAt(header[2], 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("%s cooldown end: %w", vault.Label, decodeErr)
			}
			cooldownAmount, decodeErr := BigIntAt(header[2], 1)
			if decodeErr != nil {
				return nil, fmt.Errorf("%s cooldown amount: %w", vault.Label, decodeErr)
			}
			if cooldownAmount.Sign() > 0 {
				component := NewComponent(
					"asset",
					vault.Asset,
					cooldownAmount,
					Source{Contract: vault.Address, Method: "cooldowns.underlyingAmount"},
				)
				component.Metadata = map[string]any{"cooldownEnd": cooldownEnd.String()}
				groups = append(groups, Group{
					ID:         vault.CooldownID,
					MarketID:   vault.CooldownID,
					Label:      vault.Label + " cooldown",
					Components: []Component{component},
					Metadata:   map[string]any{"vault": vault.Address},
				})
			}
		}
	}
	return groups, nil
}

func activeVaultsAt(vaults []vaultConfig, block uint64) []vaultConfig {
	active := make([]vaultConfig, 0, len(vaults))
	for _, vault := range vaults {
		if block >= vault.ActivationBlock {
			active = append(active, vault)
		}
	}
	return active
}

func erc4626Adapters() []Adapter {
	return []Adapter{
		NewERC4626Adapter("fluid-lite", "Fluid Lite", map[ChainID][]vaultConfig{
			Ethereum: {{
				ID:      "0xa0d3707c569ff8c87fa923d3823ec5d81c98be78",
				Label:   "Yield · iETHv2",
				Address: common.HexToAddress("0xA0D3707c569ff8C87FA923d3823eC5D81c98Be78"),
				Asset: token(
					Ethereum,
					"0xae7ab96520DE3A18E5e111B5EaAb095312D7fE84",
					"stETH",
					18,
				),
				ActivationBlock: 16_609_585,
			}},
		}),
		NewERC4626Adapter("cap", "Cap", map[ChainID][]vaultConfig{
			Ethereum: {{
				ID:      "stcusd",
				Label:   "Yield · stcUSD",
				Address: common.HexToAddress("0x88887be419578051ff9f4eb6c858a951921d8888"),
				Asset: token(
					Ethereum,
					"0xcccc62962d17b8914c62d74ffb843d73b2a3cccc",
					"cUSD",
					18,
				),
				ActivationBlock: 22_874_057,
			}},
		}),
		NewERC4626Adapter("ethena", "Ethena", map[ChainID][]vaultConfig{
			Ethereum: {
				{
					ID:              "susde:staked",
					CooldownID:      "susde:cooldown",
					Label:           "Ethena sUSDe staked",
					Address:         common.HexToAddress("0x9D39a5DE30e57443BfF2A8307A4256c8797A3497"),
					ActivationBlock: 18_571_359,
					Asset: token(
						Ethereum,
						"0x4c9EDD5852cd905f086C759E8383e09bff1E68B3",
						"USDe",
						18,
					),
				},
				{
					ID:              "sena:staked",
					CooldownID:      "sena:cooldown",
					Label:           "Ethena sENA staked",
					Address:         common.HexToAddress("0x8bE3460A480c80728a8C4D7a5D5303c85ba7B3b9"),
					ActivationBlock: 20_713_442,
					Asset: token(
						Ethereum,
						"0x57e114B691Db790C35207b2e685D4A43181e6061",
						"ENA",
						18,
					),
				},
			},
		}),
		NewERC4626Adapter("usdd", "USDD", map[ChainID][]vaultConfig{
			Ethereum: {{
				ID:      "susdd",
				Label:   "Yield · sUSDD",
				Address: common.HexToAddress("0xC5d6A7B61d18AfA11435a889557b068BB9f29930"),
				Asset: token(
					Ethereum,
					"0x4f8e5DE400DE08B164E7421B3EE387f461beCD1A",
					"USDD",
					18,
				),
				ActivationBlock: 23_275_147,
			}},
			BSC: {{
				ID:      "susdd",
				Label:   "Yield · sUSDD",
				Address: common.HexToAddress("0x8bA9dA757d1D66c58b1ae7e2ED6c04087348A82d"),
				Asset: token(
					BSC,
					"0x45E51bc23D592EB2DBA86da3985299f7895d66Ba",
					"USDD",
					18,
				),
				ActivationBlock: 63_887_220,
			}},
		}),
		NewERC4626Adapter("unitas", "Unitas", map[ChainID][]vaultConfig{
			BSC: {{
				ID:         "susdu:staked",
				CooldownID: "susdu:cooldown",
				Label:      "Unitas sUSDu staked",
				Address:    common.HexToAddress("0x385c279445581a186a4182a5503094ebb652ec71"),
				Asset: token(
					BSC,
					"0xea953ea6634d55dac6697c436b1e81a679db5882",
					"USDu",
					18,
				),
				ActivationBlock: 69_059_010,
			}},
		}),
	}
}
