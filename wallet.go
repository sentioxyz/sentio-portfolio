package portfolio

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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
	Ethereum: {Symbol: "ETH", Wrapped: common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")},
	BSC:      {Symbol: "BNB", Wrapped: common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c")},
	Base:     {Symbol: "ETH", Wrapped: common.HexToAddress("0x4200000000000000000000000000000000000006")},
	Arbitrum: {Symbol: "ETH", Wrapped: common.HexToAddress("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1")},
}

// walletTokenEntry is one ERC-20 the holdings adapter reads. Symbol and decimals are committed
// rather than read per scan because they are immutable and a manifest read is one fewer RPC
// round trip per token; the generator verifies both against the chain.
type walletTokenEntry struct {
	ChainID     ChainID        `json:"chainId"`
	Address     common.Address `json:"address"`
	Symbol      string         `json:"symbol"`
	Decimals    uint8          `json:"decimals"`
	CoinGeckoID string         `json:"coinGeckoId,omitempty"`
	// ActivationBlock is the first canonical block with contract code, established by an archive
	// eth_getCode binary search. Zero means the generator has not established one yet, and a
	// fixed-block scan then probes the code itself rather than guessing.
	ActivationBlock uint64 `json:"activationBlock,omitempty"`
}

func (e walletTokenEntry) token() Token {
	return Token{
		ChainID:  e.ChainID,
		Address:  e.Address,
		Symbol:   e.Symbol,
		Decimals: e.Decimals,
	}
}

// walletTokenCoinQuote records that every listed token was confirmed priceable by CoinQuote.
// An unpriceable token is not a harmless extra row: it reports an amount the response cannot
// value and adds a pricing failure to every scan of every account, so the manifest is generated
// from what CoinQuote quotes rather than from a market-cap list.
type walletTokenCoinQuote struct {
	Method     string `json:"method"`
	VerifiedAt string `json:"verifiedAt"`
}

type walletTokenManifest struct {
	Version     int                  `json:"version"`
	GeneratedAt string               `json:"generatedAt"`
	Source      string               `json:"source"`
	Scope       string               `json:"scope"`
	CoinQuote   walletTokenCoinQuote `json:"coinQuote"`
	Tokens      []walletTokenEntry   `json:"tokens"`
}

//go:embed wallet-tokens.json
var walletTokenManifestJSON []byte

var walletTokens = mustWalletTokenManifest()

func mustWalletTokenManifest() walletTokenManifest {
	var manifest walletTokenManifest
	if err := json.Unmarshal(walletTokenManifestJSON, &manifest); err != nil {
		panic(fmt.Errorf("decode wallet token manifest: %w", err))
	}
	if manifest.Version != 1 || manifest.GeneratedAt == "" || manifest.Source == "" ||
		manifest.Scope == "" || manifest.CoinQuote.Method == "" || len(manifest.Tokens) == 0 {
		panic(fmt.Errorf(
			"invalid wallet token manifest version=%d tokens=%d",
			manifest.Version,
			len(manifest.Tokens),
		))
	}
	seen := make(map[ChainID]map[common.Address]struct{})
	for _, entry := range manifest.Tokens {
		if !supportsChain(SupportedChainIDs, entry.ChainID) ||
			entry.Address == (common.Address{}) || entry.Symbol == "" || entry.Decimals > 36 {
			panic(fmt.Sprintf("invalid wallet token %s on chain %d", entry.Address, entry.ChainID))
		}
		if seen[entry.ChainID] == nil {
			seen[entry.ChainID] = make(map[common.Address]struct{})
		}
		if _, exists := seen[entry.ChainID][entry.Address]; exists {
			panic(fmt.Sprintf("duplicate wallet token %s on chain %d", entry.Address, entry.ChainID))
		}
		seen[entry.ChainID][entry.Address] = struct{}{}
	}
	return manifest
}

// WalletAdapter reports the coins an account simply holds: the native coin and the listed
// ERC-20s, none of which belongs to a protocol position.
//
// Holdings deliberately do not chase the long tail. The manifest is the set of tokens CoinQuote
// prices, so a scan returns balances it can value instead of a wider list of amounts it cannot.
type WalletAdapter struct {
	adapterBase
	tokens map[ChainID][]walletTokenEntry
}

func newWalletAdapter() *WalletAdapter {
	tokens := make(map[ChainID][]walletTokenEntry, len(SupportedChainIDs))
	for _, entry := range walletTokens.Tokens {
		tokens[entry.ChainID] = append(tokens[entry.ChainID], entry)
	}
	chains := make([]ChainID, 0, len(SupportedChainIDs))
	for _, chainID := range SupportedChainIDs {
		if _, exists := walletNativeCoin[chainID]; exists || len(tokens[chainID]) > 0 {
			chains = append(chains, chainID)
		}
	}
	return &WalletAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID:     walletProtocolID,
			Name:   "Wallet",
			Chains: chains,
		}},
		tokens: tokens,
	}
}

// Positions reads the native balance and the listed token balances. The two surfaces are
// independent: a failure in one keeps whatever the other verified, which is the behaviour
// adapter.go documents and engine.go relies on.
func (a *WalletAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0, 8)
	failures := make([]error, 0, 2)

	native, err := a.nativeGroup(ctx, client, block, account)
	if err != nil {
		failures = append(failures, err)
	}
	groups = append(groups, native...)

	tokens, err := a.tokenGroups(ctx, client, block, account)
	groups = append(groups, tokens...)
	if err != nil {
		failures = append(failures, err)
	}
	return groups, errors.Join(failures...)
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

func (a *WalletAdapter) tokenGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	entries, err := a.readableTokens(ctx, client, block)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(entries))
	for index, entry := range entries {
		calls[index] = ContractCall{
			Contract: entry.Address,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{account},
		}
	}
	// A revert from one listed token must not discard the rest of the holdings, but it is also
	// not a zero balance: every failure is reported so a degraded RPC cannot quietly shrink a
	// portfolio.
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("wallet balances: %w", err)
	}
	groups := make([]Group, 0, len(entries))
	failures := make([]error, 0)
	for index, row := range rows {
		entry := entries[index]
		if row.Error != nil {
			failures = append(failures, fmt.Errorf("%s balance: %w", entry.Symbol, row.Error))
			continue
		}
		amount, decodeErr := BigIntAt(row.Values, 0)
		if decodeErr != nil {
			failures = append(failures, fmt.Errorf("%s balance: %w", entry.Symbol, decodeErr))
			continue
		}
		if amount.Sign() == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:    walletTokenGroupID(entry.Address),
			Label: entry.Symbol,
			Components: []Component{NewComponent(
				"asset",
				entry.token(),
				amount,
				Source{Contract: entry.Address, Method: "balanceOf"},
			)},
			Metadata: map[string]any{"holding": "token"},
		})
	}
	return groups, errors.Join(failures...)
}

// readableTokens drops the tokens that do not exist at the pinned block.
//
// A token deployed after the scanned block answers eth_call with empty data, which fails to
// decode and is then indistinguishable from a token whose balance could not be read. Committed
// activation blocks answer that for free; the rest are probed with eth_getCode, but only on a
// fixed-block scan — every listed token exists at the head.
func (a *WalletAdapter) readableTokens(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) ([]walletTokenEntry, error) {
	entries := make([]walletTokenEntry, 0, len(a.tokens[block.ChainID]))
	probe := make([]common.Address, 0)
	probed := make([]walletTokenEntry, 0)
	for _, entry := range a.tokens[block.ChainID] {
		switch {
		case entry.ActivationBlock > 0:
			if block.Number >= entry.ActivationBlock {
				entries = append(entries, entry)
			}
		case !block.Fixed:
			entries = append(entries, entry)
		default:
			probe = append(probe, entry.Address)
			probed = append(probed, entry)
		}
	}
	if len(probe) == 0 {
		return entries, nil
	}
	deployed, err := client.DeployedAt(ctx, block, probe)
	if err != nil {
		return nil, fmt.Errorf("wallet token deployment: %w", err)
	}
	for index, entry := range probed {
		if deployed[index] {
			entries = append(entries, entry)
		}
	}
	return entries, nil
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
// token — records the contract it read in Source. Collecting those is what lets the holdings
// manifest stay a plain list of assets: the same token can never be reported twice, and the
// guarantee survives a new adapter rather than depending on someone remembering to prune a list.
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
