package portfolio

import (
	"context"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// walletProtocolID is a protocol identifier rather than a separate response section on purpose:
// plain holdings then reuse the whole existing pipeline — chain fan-out, account attribution,
// pricing, valuation and the protocol filter — and every consumer that already renders a
// snapshot renders holdings without a schema change.
const walletProtocolID = "wallet"

// walletNativeCoin names the coin an account holds outside any contract, and the wrapped ERC-20
// whose address prices it. The kernel already identifies a native amount by its wrapped address
// (fluidUnderlyingAddress does the same for Fluid's native sentinel) and the host's CoinQuote
// provider resolves that address to the same asset, so holdings need no new pricing identity.
var walletNativeCoin = map[ChainID]struct {
	Symbol  string
	Wrapped common.Address
}{
	Ethereum:  {Symbol: "ETH", Wrapped: common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")},
	BSC:       {Symbol: "BNB", Wrapped: common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c")},
	Base:      {Symbol: "ETH", Wrapped: common.HexToAddress("0x4200000000000000000000000000000000000006")},
	Arbitrum:  {Symbol: "ETH", Wrapped: common.HexToAddress("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1")},
	Polygon:   {Symbol: "POL", Wrapped: common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270")},
	Monad:     {Symbol: "MON", Wrapped: common.HexToAddress("0x3bd359C1119dA7Da1D913D1C4D2B7c461115433A")},
	Plasma:    {Symbol: "XPL", Wrapped: common.HexToAddress("0x6100E367285b01F48D07953803A2d8dCA5D19873")},
	Avalanche: {Symbol: "AVAX", Wrapped: common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7")},
	Optimism:  {Symbol: "ETH", Wrapped: common.HexToAddress("0x4200000000000000000000000000000000000006")},
}

// WalletAdapter reads native balances. ERC-20 candidates are supplied exclusively
// by WalletBalanceProvider and orchestrated by engine.go.
type WalletAdapter struct {
	adapterBase
}

func newWalletAdapter() *WalletAdapter {
	return &WalletAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: walletProtocolID, Name: "Wallet", Chains: append([]ChainID(nil), SupportedChainIDs...),
	}}}
}

// Positions preserves the native balance when token discovery is unavailable.
// The engine reports the provider's coverage error independently.
func (a *WalletAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	return a.nativeGroup(ctx, client, block, account)
}

func (a *WalletAdapter) nativeGroup(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	coin, exists := walletNativeCoin[block.ChainID]
	if !exists {
		return nil, nil
	}
	balance, err := client.NativeBalance(ctx, block, account)
	if err != nil {
		return nil, fmt.Errorf("%s balance: %w", coin.Symbol, err)
	}
	if balance.Sign() == 0 {
		return nil, nil
	}
	token := Token{
		ChainID:  block.ChainID,
		Address:  coin.Wrapped,
		Symbol:   coin.Symbol,
		Decimals: 18,
	}
	component := NewComponent(
		"asset",
		token,
		balance,
		Source{Method: "eth_getBalance"},
	)
	component.Metadata = map[string]any{"native": true}
	return []Group{{
		ID:         walletNativeGroupID,
		Label:      coin.Symbol,
		Components: []Component{component},
		Metadata:   map[string]any{"holding": "native"},
	}}, nil
}

// walletNativeGroupID separates the native coin from the wrapped ERC-20 of the same address, so
// an account holding both ETH and WETH reports two balances rather than one ambiguous total.
const walletNativeGroupID = "wallet:native"

func walletTokenGroupID(address common.Address) string {
	return "wallet:token:" + strings.ToLower(address.Hex())
}

// holdingScope identifies whose balance an observation is about. A scan covers the address it
// was asked for plus the accounts attributed to it, and each holds its tokens independently, so
// a position one of them opened says nothing about what the others hold.
type holdingScope struct {
	ChainID ChainID
	Account common.Address
}

// groupAccount reports the account a group belongs to. Snapshot.Account is always the address
// the scan was asked for; an attributed account is named in the group metadata engine.go
// stamps.
func groupAccount(snapshot Snapshot, group Group) common.Address {
	if raw, exists := group.Metadata["attributedAccount"]; exists {
		if account, isAddress := raw.(common.Address); isAddress {
			return account
		}
	}
	return snapshot.Account
}

// walletHeldContracts collects the ERC-20s whose wallet balance a protocol adapter already
// counted, keyed by chain and by the account that holds them.
//
// Every adapter that reads a token an account holds — an LST, a vault share, an aToken, an LP
// token — records the contract it read in Source. Matching that provenance against
// provider-discovered holdings prevents double counting as adapters are added.
//
// The account is part of the key because a scan aggregates several of them. A DSA proxy staking
// a token says nothing about the same token sitting in the root wallet: those are two balances
// that happen to share a contract.
func walletHeldContracts(snapshots []Snapshot) map[holdingScope]map[common.Address]struct{} {
	held := make(map[holdingScope]map[common.Address]struct{})
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID == walletProtocolID {
			continue
		}
		for _, group := range snapshot.Groups {
			scope := holdingScope{
				ChainID: snapshot.ChainID,
				Account: groupAccount(snapshot, group),
			}
			for _, component := range group.Components {
				contract := component.Source.Contract
				if contract == (common.Address{}) ||
					!strings.Contains(component.Source.Method, "balanceOf") {
					continue
				}
				if held[scope] == nil {
					held[scope] = make(map[common.Address]struct{})
				}
				held[scope][contract] = struct{}{}
			}
		}
	}
	return held
}

// suppressDuplicateHoldings removes the holdings a protocol snapshot already reports. It runs
// after every adapter has finished and before valuation, so the decision never depends on the
// order the adapters completed in.
func suppressDuplicateHoldings(snapshots []Snapshot) []Snapshot {
	held := walletHeldContracts(snapshots)
	result := make([]Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID != walletProtocolID || len(held) == 0 {
			result = append(result, snapshot)
			continue
		}
		groups := make([]Group, 0, len(snapshot.Groups))
		for _, group := range snapshot.Groups {
			if len(group.Components) == 1 && group.Metadata["holding"] == "token" {
				scope := holdingScope{
					ChainID: snapshot.ChainID,
					Account: groupAccount(snapshot, group),
				}
				if _, duplicate := held[scope][group.Components[0].Token.Address]; duplicate {
					continue
				}
			}
			groups = append(groups, group)
		}
		if len(groups) == 0 {
			continue
		}
		snapshot.Groups = groups
		result = append(result, snapshot)
	}
	return result
}
