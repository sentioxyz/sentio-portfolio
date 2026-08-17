package portfolio

import (
	"bytes"
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

var (
	makerRay = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)
	makerWad = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
)

var makerManagerABI = MustABI(`[
  {"type":"function","name":"count","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"first","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"list","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]},
  {"type":"function","name":"owns","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"urns","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"ilks","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"bytes32"}]}
]`)

var makerVatABI = MustABI(`[
  {"type":"function","name":"urns","stateMutability":"view","inputs":[{"type":"bytes32"},{"type":"address"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]},
  {"type":"function","name":"ilks","stateMutability":"view","inputs":[{"type":"bytes32"}],"outputs":[{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"}]}
]`)

var makerIlkRegistryABI = MustABI(`[
  {"type":"function","name":"gem","stateMutability":"view","inputs":[{"type":"bytes32"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"dec","stateMutability":"view","inputs":[{"type":"bytes32"}],"outputs":[{"type":"uint256"}]}
]`)

var makerProxyRegistryABI = MustABI(`[
  {"type":"function","name":"proxies","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"address"}]}
]`)

var makerDSProxyABI = MustABI(`[
  {"type":"function","name":"owner","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var makerPotABI = MustABI(`[
  {"type":"function","name":"pie","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"chi","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"dsr","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"rho","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
]`)

var makerDSRManagerABI = MustABI(`[
  {"type":"function","name":"pieOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]}
]`)

var makerDeployment = struct {
	Manager, Vat, IlkRegistry, ProxyRegistry, Pot, DSRManager common.Address
	DAI                                                       Token
	MaximumVaultsPerOwner                                     uint64
}{
	Manager:               common.HexToAddress("0x5ef30b9986345249bc32d8928B7ee64DE9435E39"),
	Vat:                   common.HexToAddress("0x35D1b3F3D7966A1DFe207aa4514C12a259A0492B"),
	IlkRegistry:           common.HexToAddress("0x5a464C28D19848f44199D003BeF5ecc87d090F87"),
	ProxyRegistry:         common.HexToAddress("0x4678f0a6958e4D2Bc4F1BAF7Bc52E8F3564f3fE4"),
	Pot:                   common.HexToAddress("0x197E90f9FAD81970bA7976f33CbD77088E5D7cf7"),
	DSRManager:            common.HexToAddress("0x373238337Bfe1146fb49989fc222523f83081dDb"),
	DAI:                   token(Ethereum, "0x6B175474E89094C44Da98b954EedeAC495271d0F", "DAI", 18),
	MaximumVaultsPerOwner: 1_024,
}

type MakerDAOAdapter struct{ adapterBase }

type makerVault struct {
	CDP        *big.Int
	Owner, Urn common.Address
	Ilk        [32]byte
	Ink, Art   *big.Int
}

type makerIlk struct {
	Rate     *big.Int
	Gem      common.Address
	Decimals uint8
}

func newMakerDAOAdapter() Adapter {
	return &MakerDAOAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "makerdao", Name: "Sky Lending / MakerDAO", Chains: []ChainID{Ethereum},
	}}}
}

func makerDebtRaw(art, rate *big.Int) *big.Int {
	return new(big.Int).Quo(new(big.Int).Mul(art, rate), makerRay)
}

func makerCollateralRaw(ink *big.Int, decimals uint8) *big.Int {
	if decimals <= 18 {
		return new(big.Int).Quo(new(big.Int).Set(ink), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(18-decimals)), nil))
	}
	return new(big.Int).Mul(new(big.Int).Set(ink), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals-18)), nil))
}

func makerSavingsRaw(pie, chi *big.Int) *big.Int {
	return new(big.Int).Quo(new(big.Int).Mul(pie, chi), makerRay)
}

func makerRayPower(x *big.Int, exponent uint64) *big.Int {
	if x.Sign() == 0 {
		if exponent == 0 {
			return new(big.Int).Set(makerRay)
		}
		return new(big.Int)
	}
	base := new(big.Int).Set(makerRay)
	half := new(big.Int).Quo(new(big.Int).Set(base), big.NewInt(2))
	z := new(big.Int).Set(base)
	if exponent%2 != 0 {
		z.Set(x)
	}
	current := new(big.Int).Set(x)
	for n := exponent / 2; n > 0; n /= 2 {
		current.Quo(new(big.Int).Add(new(big.Int).Mul(current, current), half), base)
		if n%2 != 0 {
			z.Quo(new(big.Int).Add(new(big.Int).Mul(z, current), half), base)
		}
	}
	return z
}

func makerAccruedChi(persistedChi, dsr *big.Int, rho, timestamp uint64) (*big.Int, error) {
	if timestamp < rho {
		return nil, fmt.Errorf("Maker Pot rho %d is ahead of pinned timestamp %d", rho, timestamp)
	}
	power := makerRayPower(dsr, timestamp-rho)
	return new(big.Int).Quo(new(big.Int).Mul(power, persistedChi), makerRay), nil
}

func bytes32At(values []any, index int) ([32]byte, error) {
	if index < 0 || index >= len(values) {
		return [32]byte{}, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].([32]byte)
	if !ok {
		return [32]byte{}, fmt.Errorf("result %d is %T, expected bytes32", index, values[index])
	}
	return value, nil
}

func makerIlkName(ilk [32]byte) string { return string(bytes.TrimRight(ilk[:], "\x00")) }

func makerPositionOwners(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]common.Address, error) {
	row, err := client.Call(ctx, block, makerDeployment.ProxyRegistry, makerProxyRegistryABI, "proxies", account)
	if err != nil {
		return nil, fmt.Errorf("proxy registry: %w", err)
	}
	proxy, err := AddressAt(row, 0)
	if err != nil {
		return nil, err
	}
	if proxy == (common.Address{}) {
		return []common.Address{account}, nil
	}
	ownerRow, err := client.Call(ctx, block, proxy, makerDSProxyABI, "owner")
	if err != nil {
		return nil, fmt.Errorf("proxy owner: %w", err)
	}
	owner, err := AddressAt(ownerRow, 0)
	if err != nil {
		return nil, err
	}
	if owner != account {
		return []common.Address{account}, nil
	}
	return []common.Address{account, proxy}, nil
}

func makerOwnerCDPs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]*big.Int, error) {
	header, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: makerDeployment.Manager, ABI: makerManagerABI, Method: "count", Args: []any{owner}},
		{Contract: makerDeployment.Manager, ABI: makerManagerABI, Method: "first", Args: []any{owner}},
	})
	if err != nil {
		return nil, err
	}
	count, err := BigIntAt(header[0], 0)
	if err != nil {
		return nil, err
	}
	first, err := BigIntAt(header[1], 0)
	if err != nil {
		return nil, err
	}
	if !count.IsUint64() || count.Uint64() > makerDeployment.MaximumVaultsPerOwner {
		return nil, fmt.Errorf("Maker owner %s vault count %s exceeds maximum %d", owner, count, makerDeployment.MaximumVaultsPerOwner)
	}
	ids := make([]*big.Int, 0, count.Uint64())
	seen := make(map[string]struct{}, count.Uint64())
	current := first
	for offset := uint64(0); offset < count.Uint64(); offset++ {
		key := current.String()
		if current.Sign() == 0 {
			return nil, fmt.Errorf("Maker owner %s vault list ended early", owner)
		}
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("Maker owner %s vault list contains cycle at %s", owner, key)
		}
		seen[key] = struct{}{}
		ids = append(ids, new(big.Int).Set(current))
		row, callErr := client.Call(ctx, block, makerDeployment.Manager, makerManagerABI, "list", current)
		if callErr != nil {
			return nil, callErr
		}
		current, err = BigIntAt(row, 1)
		if err != nil {
			return nil, err
		}
	}
	if current.Sign() != 0 {
		return nil, fmt.Errorf("Maker owner %s vault list exceeds declared count", owner)
	}
	return ids, nil
}

func makerVaults(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owners []common.Address,
) ([]makerVault, error) {
	type ownerCDP struct {
		owner common.Address
		cdp   *big.Int
	}
	entries := make([]ownerCDP, 0)
	for _, owner := range owners {
		ids, err := makerOwnerCDPs(ctx, client, block, owner)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			entries = append(entries, ownerCDP{owner: owner, cdp: id})
		}
	}
	metadataCalls := make([]ContractCall, 0, len(entries)*3)
	for _, entry := range entries {
		metadataCalls = append(metadataCalls,
			ContractCall{Contract: makerDeployment.Manager, ABI: makerManagerABI, Method: "owns", Args: []any{entry.cdp}},
			ContractCall{Contract: makerDeployment.Manager, ABI: makerManagerABI, Method: "urns", Args: []any{entry.cdp}},
			ContractCall{Contract: makerDeployment.Manager, ABI: makerManagerABI, Method: "ilks", Args: []any{entry.cdp}},
		)
	}
	metadata, err := client.ParallelCalls(ctx, block, metadataCalls)
	if err != nil {
		return nil, err
	}
	vaults := make([]makerVault, 0, len(entries))
	for index, entry := range entries {
		actualOwner, decodeErr := AddressAt(metadata[index*3], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if actualOwner != entry.owner {
			return nil, fmt.Errorf("Maker CDP %s owner changed from %s to %s", entry.cdp, entry.owner, actualOwner)
		}
		urn, decodeErr := AddressAt(metadata[index*3+1], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		ilk, decodeErr := bytes32At(metadata[index*3+2], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		state, callErr := client.Call(ctx, block, makerDeployment.Vat, makerVatABI, "urns", ilk, urn)
		if callErr != nil {
			return nil, callErr
		}
		ink, decodeErr := BigIntAt(state, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		art, decodeErr := BigIntAt(state, 1)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if ink.Sign() > 0 || art.Sign() > 0 {
			vaults = append(vaults, makerVault{CDP: entry.cdp, Owner: entry.owner, Urn: urn, Ilk: ilk, Ink: ink, Art: art})
		}
	}
	return vaults, nil
}

func makerVaultGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	vaults []makerVault,
) ([]Group, error) {
	ilks := make(map[[32]byte]makerIlk)
	for _, vault := range vaults {
		if _, exists := ilks[vault.Ilk]; exists {
			continue
		}
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: makerDeployment.Vat, ABI: makerVatABI, Method: "ilks", Args: []any{vault.Ilk}},
			{Contract: makerDeployment.IlkRegistry, ABI: makerIlkRegistryABI, Method: "gem", Args: []any{vault.Ilk}},
			{Contract: makerDeployment.IlkRegistry, ABI: makerIlkRegistryABI, Method: "dec", Args: []any{vault.Ilk}},
		})
		if err != nil {
			return nil, err
		}
		rate, err := BigIntAt(rows[0], 1)
		if err != nil {
			return nil, err
		}
		gem, err := AddressAt(rows[1], 0)
		if err != nil {
			return nil, err
		}
		decimals, err := BigIntAt(rows[2], 0)
		if err != nil || !decimals.IsUint64() || decimals.Uint64() > 255 {
			return nil, fmt.Errorf("Maker ilk %s has invalid decimals", makerIlkName(vault.Ilk))
		}
		ilks[vault.Ilk] = makerIlk{Rate: rate, Gem: gem, Decimals: uint8(decimals.Uint64())}
	}
	tokens := make(map[common.Address]Token)
	groups := make([]Group, 0, len(vaults))
	for _, vault := range vaults {
		info := ilks[vault.Ilk]
		collateralToken, exists := tokens[info.Gem]
		if !exists {
			var err error
			collateralToken, err = readToken(ctx, client, block, info.Gem)
			if err != nil {
				return nil, fmt.Errorf("Maker collateral %s metadata: %w", info.Gem, err)
			}
			tokens[info.Gem] = collateralToken
		}
		components := make([]Component, 0, 2)
		collateral := makerCollateralRaw(vault.Ink, info.Decimals)
		if collateral.Sign() > 0 {
			component := NewComponent("asset", collateralToken, collateral, Source{Contract: makerDeployment.Vat, Method: "urns.ink"})
			component.Metadata = map[string]any{"cdp": vault.CDP.String(), "urn": vault.Urn, "ilk": makerIlkName(vault.Ilk), "normalizedInk": vault.Ink.String()}
			components = append(components, component)
		}
		debt := makerDebtRaw(vault.Art, info.Rate)
		if debt.Sign() > 0 {
			component := NewComponent("debt", makerDeployment.DAI, debt, Source{Contract: makerDeployment.Vat, Method: "urns.art*ilks.rate/1e27"})
			component.Metadata = map[string]any{"cdp": vault.CDP.String(), "urn": vault.Urn, "ilk": makerIlkName(vault.Ilk), "normalizedDebt": vault.Art.String(), "rate": info.Rate.String()}
			components = append(components, component)
		}
		name := makerIlkName(vault.Ilk)
		groups = append(groups, Group{
			ID: "vault:" + vault.CDP.String(), MarketID: "vault:" + name,
			Label: name + " Vault #" + vault.CDP.String(), Components: components,
			Metadata: map[string]any{"owner": vault.Owner, "urn": vault.Urn, "cdp": vault.CDP.String()},
		})
	}
	return groups, nil
}

func makerSavingsGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owners []common.Address,
) (*Group, error) {
	calls := []ContractCall{
		{Contract: makerDeployment.Pot, ABI: makerPotABI, Method: "chi"},
		{Contract: makerDeployment.Pot, ABI: makerPotABI, Method: "dsr"},
		{Contract: makerDeployment.Pot, ABI: makerPotABI, Method: "rho"},
	}
	for _, owner := range owners {
		calls = append(calls,
			ContractCall{Contract: makerDeployment.Pot, ABI: makerPotABI, Method: "pie", Args: []any{owner}},
			ContractCall{Contract: makerDeployment.DSRManager, ABI: makerDSRManagerABI, Method: "pieOf", Args: []any{owner}},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	chi, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, err
	}
	dsr, err := BigIntAt(rows[1], 0)
	if err != nil {
		return nil, err
	}
	rho, err := BigIntAt(rows[2], 0)
	if err != nil || !rho.IsUint64() {
		return nil, fmt.Errorf("Maker Pot rho is invalid")
	}
	directPie := new(big.Int)
	managedPie := new(big.Int)
	for index := range owners {
		direct, decodeErr := BigIntAt(rows[3+index*2], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		managed, decodeErr := BigIntAt(rows[4+index*2], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		directPie.Add(directPie, direct)
		managedPie.Add(managedPie, managed)
	}
	effectiveChi, err := makerAccruedChi(chi, dsr, rho.Uint64(), block.Timestamp)
	if err != nil {
		return nil, err
	}
	totalPie := new(big.Int).Add(directPie, managedPie)
	amount := makerSavingsRaw(totalPie, effectiveChi)
	if amount.Sign() == 0 {
		return nil, nil
	}
	component := NewComponent("asset", makerDeployment.DAI, amount, Source{Contract: makerDeployment.Pot, Method: "pie*chi*rpow(dsr,timestamp-rho)/1e54"})
	component.Metadata = map[string]any{
		"directPie": directPie.String(), "dsrManagerPie": managedPie.String(),
		"persistedChi": chi.String(), "effectiveChi": effectiveChi.String(),
		"dsr": dsr.String(), "rho": rho.String(),
	}
	return &Group{ID: "dai-savings", MarketID: "dai-savings", Label: "DAI Savings Rate", Components: []Component{component}}, nil
}

func (a *MakerDAOAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	owners, err := makerPositionOwners(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	vaults, err := makerVaults(ctx, client, block, owners)
	if err != nil {
		return nil, err
	}
	groups, err := makerVaultGroups(ctx, client, block, vaults)
	if err != nil {
		return nil, err
	}
	savings, err := makerSavingsGroup(ctx, client, block, owners)
	if err != nil {
		return nil, err
	}
	if savings != nil {
		groups = append(groups, *savings)
	}
	return groups, nil
}
