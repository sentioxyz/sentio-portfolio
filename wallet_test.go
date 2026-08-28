package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestWalletTokenManifestIsWellFormed(t *testing.T) {
	if walletTokens.CoinQuote.Method == "" {
		t.Fatal("manifest does not record how CoinQuote support was established")
	}
	perChain := make(map[ChainID]int)
	for _, entry := range walletTokens.Tokens {
		if entry.Symbol == "" || entry.Address == (common.Address{}) {
			t.Fatalf("token %+v has an empty identity", entry)
		}
		if entry.Decimals == 0 || entry.Decimals > 36 {
			t.Errorf("token %s on chain %d has decimals %d", entry.Symbol, entry.ChainID, entry.Decimals)
		}
		if entry.CoinGeckoID == "" {
			t.Errorf("token %s on chain %d has no CoinGecko identity", entry.Symbol, entry.ChainID)
		}
		perChain[entry.ChainID]++
	}
	for _, chainID := range SupportedChainIDs {
		if perChain[chainID] == 0 {
			t.Errorf("chain %d has no wallet tokens", chainID)
		}
	}
}

// The holdings manifest lists assets, never the receipt tokens an adapter reads. A wrapped
// staking token in the manifest is double counting rather than extra coverage, and it is the
// mistake a growing list invites.
func TestWalletTokenManifestExcludesTokensAdaptersRead(t *testing.T) {
	claimed := make(map[ChainID]map[common.Address]string)
	record := func(chainID ChainID, address common.Address, label string) {
		if claimed[chainID] == nil {
			claimed[chainID] = make(map[common.Address]string)
		}
		claimed[chainID][address] = label
	}
	for _, adapter := range NewEngine(nil, nil).adapters {
		switch typed := adapter.(type) {
		case *ConvertedBalanceAdapter:
			for chainID, positions := range typed.positions {
				for _, position := range positions {
					record(chainID, position.BalanceContract, position.Label)
				}
			}
		case *ERC4626Adapter:
			for chainID, vaults := range typed.vaults {
				for _, vault := range vaults {
					record(chainID, vault.Address, vault.Label)
				}
			}
		}
	}
	for _, entry := range walletTokens.Tokens {
		if label, exists := claimed[entry.ChainID][entry.Address]; exists {
			t.Errorf(
				"wallet token %s on chain %d is already read as %q",
				entry.Symbol,
				entry.ChainID,
				label,
			)
		}
	}
}

func TestWalletNativeCoinsUseTheWrappedTokenIdentity(t *testing.T) {
	for _, chainID := range SupportedChainIDs {
		coin, exists := walletNativeCoin[chainID]
		if !exists {
			t.Fatalf("chain %d has no native coin", chainID)
		}
		if coin.Symbol == "" {
			t.Errorf("chain %d native coin has no symbol", chainID)
		}
		if coin.Wrapped != fluidWrappedNative[chainID] {
			t.Errorf(
				"chain %d native coin prices as %s, but the kernel's wrapped native is %s",
				chainID,
				coin.Wrapped,
				fluidWrappedNative[chainID],
			)
		}
	}
}

type walletTestServer struct {
	t        *testing.T
	native   *big.Int
	balances map[common.Address]*big.Int
	reverts  map[common.Address]struct{}
	code     map[common.Address]struct{}
	calls    int
}

type walletTestRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (s *walletTestServer) target(param json.RawMessage) common.Address {
	s.t.Helper()
	var object struct {
		To common.Address `json:"to"`
	}
	if err := json.Unmarshal(param, &object); err != nil {
		var address common.Address
		if addressErr := json.Unmarshal(param, &address); addressErr != nil {
			s.t.Fatalf("decode call target: %v", err)
		}
		return address
	}
	return object.To
}

func (s *walletTestServer) answer(call walletTestRequest) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = "0x1"
	case "eth_getBalance":
		var account common.Address
		if err := json.Unmarshal(call.Params[0], &account); err != nil {
			s.t.Fatalf("decode balance account: %v", err)
		}
		response["result"] = "0x" + s.native.Text(16)
	case "eth_getCode":
		address := s.target(call.Params[0])
		if _, exists := s.code[address]; exists {
			response["result"] = "0x60006000"
			break
		}
		response["result"] = "0x"
	case "eth_call":
		s.calls++
		address := s.target(call.Params[0])
		if _, reverts := s.reverts[address]; reverts {
			response["error"] = map[string]any{"code": 3, "message": "execution reverted"}
			break
		}
		balance, exists := s.balances[address]
		if !exists {
			balance = big.NewInt(0)
		}
		response["result"] = "0x" + common.Bytes2Hex(common.LeftPadBytes(balance.Bytes(), 32))
	default:
		s.t.Fatalf("unexpected method %q", call.Method)
	}
	return response
}

func (s *walletTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Fatalf("decode request: %v", err)
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call walletTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Fatalf("decode call: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(s.answer(call))
		return
	}
	var calls []walletTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Fatalf("decode batch: %v", err)
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = s.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

var (
	walletTestUSDC = common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	walletTestWBTC = common.HexToAddress("0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599")
	walletTestLate = common.HexToAddress("0x1111111111111111111111111111111111111111")
)

func newWalletTestAdapter() *WalletAdapter {
	return &WalletAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: walletProtocolID, Name: "Wallet", Chains: []ChainID{Ethereum},
		}},
		tokens: map[ChainID][]walletTokenEntry{Ethereum: {
			{ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6},
			{ChainID: Ethereum, Address: walletTestWBTC, Symbol: "WBTC", Decimals: 8},
			{
				ChainID:         Ethereum,
				Address:         walletTestLate,
				Symbol:          "LATE",
				Decimals:        18,
				ActivationBlock: 5_000,
			},
		}},
	}
}

func newWalletTestClient(t *testing.T, server *walletTestServer) *RPCClient {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := DialRPC(context.Background(), Ethereum, httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func groupByID(groups []Group, id string) (Group, bool) {
	for _, group := range groups {
		if group.ID == id {
			return group, true
		}
	}
	return Group{}, false
}

func TestWalletAdapterReportsNativeAndTokenBalances(t *testing.T) {
	server := &walletTestServer{
		t:      t,
		native: big.NewInt(1_500_000_000_000_000_000),
		balances: map[common.Address]*big.Int{
			walletTestUSDC: big.NewInt(250_000_000),
			walletTestWBTC: big.NewInt(0),
		},
	}
	groups, err := newWalletTestAdapter().Positions(
		context.Background(),
		newWalletTestClient(t, server),
		BlockRef{ChainID: Ethereum, Number: 4_000},
		common.HexToAddress("0xabc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want the native coin and USDC only: %+v", len(groups), groups)
	}
	native, exists := groupByID(groups, walletNativeGroupID)
	if !exists {
		t.Fatal("native coin is missing")
	}
	if native.Components[0].Token.Address != walletNativeCoin[Ethereum].Wrapped ||
		native.Components[0].AmountRaw != "1500000000000000000" ||
		native.Components[0].Metadata["native"] != true {
		t.Fatalf("native component = %+v", native.Components[0])
	}
	usdc, exists := groupByID(groups, walletTokenGroupID(walletTestUSDC))
	if !exists {
		t.Fatal("USDC holding is missing")
	}
	if usdc.Components[0].AmountRaw != "250000000" ||
		usdc.Components[0].Source.Method != "balanceOf" ||
		usdc.Components[0].Source.Contract != walletTestUSDC {
		t.Fatalf("USDC component = %+v", usdc.Components[0])
	}
	// The token whose activation block is above the scanned block is never called, so a
	// historical scan does not spend a call on a contract that cannot exist.
	if server.calls != 2 {
		t.Fatalf("eth_call count = %d, want 2", server.calls)
	}
}

func TestWalletAdapterReportsFailuresWithoutDiscardingBalances(t *testing.T) {
	server := &walletTestServer{
		t:      t,
		native: big.NewInt(0),
		balances: map[common.Address]*big.Int{
			walletTestUSDC: big.NewInt(1),
		},
		reverts: map[common.Address]struct{}{walletTestWBTC: {}},
	}
	groups, err := newWalletTestAdapter().Positions(
		context.Background(),
		newWalletTestClient(t, server),
		BlockRef{ChainID: Ethereum, Number: 4_000},
		common.HexToAddress("0xabc"),
	)
	if err == nil {
		t.Fatal("a failed balance read must be reported")
	}
	if !strings.Contains(err.Error(), "WBTC") {
		t.Fatalf("error does not name the failed token: %v", err)
	}
	if _, exists := groupByID(groups, walletTokenGroupID(walletTestUSDC)); !exists {
		t.Fatalf("verified balances were discarded: %+v", groups)
	}
}

func TestWalletAdapterSkipsTokensWithoutCodeAtAFixedBlock(t *testing.T) {
	server := &walletTestServer{
		t:        t,
		native:   big.NewInt(0),
		balances: map[common.Address]*big.Int{walletTestUSDC: big.NewInt(7)},
		code:     map[common.Address]struct{}{walletTestUSDC: {}},
	}
	groups, err := newWalletTestAdapter().Positions(
		context.Background(),
		newWalletTestClient(t, server),
		BlockRef{ChainID: Ethereum, Number: 4_000, Fixed: true},
		common.HexToAddress("0xabc"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != walletTokenGroupID(walletTestUSDC) {
		t.Fatalf("groups = %+v, want USDC only", groups)
	}
	if server.calls != 1 {
		t.Fatalf("eth_call count = %d, want 1: an undeployed token must not be called", server.calls)
	}
}

func holdingSnapshot(groups ...Group) Snapshot {
	return Snapshot{
		ProtocolID:   walletProtocolID,
		ProtocolName: "Wallet",
		ChainID:      Ethereum,
		Groups:       groups,
	}
}

func parsedAmount(value string) *big.Int {
	amount, ok := new(big.Int).SetString(value, 10)
	if !ok {
		panic("invalid amount " + value)
	}
	return amount
}

func holdingGroup(kind string, token common.Address, amount string) Group {
	return Group{
		ID:    walletTokenGroupID(token),
		Label: "token",
		Components: []Component{NewComponent(
			"asset",
			Token{ChainID: Ethereum, Address: token, Decimals: 18},
			parsedAmount(amount),
			Source{Contract: token, Method: "balanceOf"},
		)},
		Metadata: map[string]any{"holding": kind},
	}
}

func TestSuppressDuplicateHoldingsDropsTokensAnAdapterAlreadyRead(t *testing.T) {
	wstETH := common.HexToAddress("0x7f39C581F595B53c5cb19bD0b3f8dA6c935E2Ca0")
	stETH := common.HexToAddress("0xae7ab96520DE3A18E5e111B5EaAb095312D7fE84")
	snapshots := suppressDuplicateHoldings([]Snapshot{
		{
			ProtocolID:   "lido",
			ProtocolName: "Lido",
			ChainID:      Ethereum,
			Groups: []Group{{
				ID: "lido:wsteth",
				Components: []Component{NewComponent(
					"asset",
					Token{ChainID: Ethereum, Address: stETH, Decimals: 18},
					big.NewInt(5),
					Source{Contract: wstETH, Method: "getStETHByWstETH(balanceOf)"},
				)},
			}},
		},
		holdingSnapshot(
			holdingGroup("token", wstETH, "5"),
			holdingGroup("token", walletTestUSDC, "9"),
			Group{
				ID:       walletNativeGroupID,
				Metadata: map[string]any{"holding": "native"},
				Components: []Component{NewComponent(
					"asset",
					Token{ChainID: Ethereum, Address: walletNativeCoin[Ethereum].Wrapped, Decimals: 18},
					big.NewInt(3),
					Source{Method: "eth_getBalance"},
				)},
			},
		),
	})
	var wallet Snapshot
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID == walletProtocolID {
			wallet = snapshot
		}
	}
	if len(wallet.Groups) != 2 {
		t.Fatalf("holdings = %+v, want USDC and the native coin", wallet.Groups)
	}
	if _, exists := groupByID(wallet.Groups, walletTokenGroupID(wstETH)); exists {
		t.Error("wstETH is counted twice: Lido already reads that wallet balance")
	}
	if _, exists := groupByID(wallet.Groups, walletTokenGroupID(walletTestUSDC)); !exists {
		t.Error("USDC was dropped even though no adapter reads it")
	}
}

// Supplying USDC to a lending market reports USDC as the component token while reading the
// aToken balance. The wallet's own USDC is a different balance and must survive.
func TestSuppressDuplicateHoldingsKeepsAnUnderlyingAssetHeldInTheWallet(t *testing.T) {
	aToken := common.HexToAddress("0x98C23E9d8f34FEFb1B7BD6a91B7FF122F4e16F5c")
	snapshots := suppressDuplicateHoldings([]Snapshot{
		{
			ProtocolID: "aave-v3",
			ChainID:    Ethereum,
			Groups: []Group{{
				ID: "aave:usdc",
				Components: []Component{NewComponent(
					"asset",
					Token{ChainID: Ethereum, Address: walletTestUSDC, Decimals: 6},
					big.NewInt(100),
					Source{Contract: aToken, Method: "balanceOf"},
				)},
			}},
		},
		holdingSnapshot(holdingGroup("token", walletTestUSDC, "9")),
	})
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID != walletProtocolID {
			continue
		}
		if _, exists := groupByID(snapshot.Groups, walletTokenGroupID(walletTestUSDC)); !exists {
			t.Fatal("wallet USDC was dropped because a market reports USDC as its underlying")
		}
		return
	}
	t.Fatal("the holdings snapshot was removed entirely")
}

func TestSuppressDuplicateHoldingsIgnoresOtherChains(t *testing.T) {
	snapshots := suppressDuplicateHoldings([]Snapshot{
		{
			ProtocolID: "venus",
			ChainID:    BSC,
			Groups: []Group{{
				ID: "venus:usdc",
				Components: []Component{NewComponent(
					"asset",
					Token{ChainID: BSC, Address: walletTestUSDC, Decimals: 18},
					big.NewInt(1),
					Source{Contract: walletTestUSDC, Method: "balanceOf"},
				)},
			}},
		},
		holdingSnapshot(holdingGroup("token", walletTestUSDC, "9")),
	})
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID == walletProtocolID && len(snapshot.Groups) == 0 {
			t.Fatal("a BSC position suppressed an Ethereum holding")
		}
	}
}

// A scan aggregates the address it was asked for and the accounts attributed to it. Those hold
// their tokens independently: a DSA proxy staking a token says nothing about the same token
// sitting in the root wallet, so suppressing one must never suppress the other.
func TestSuppressDuplicateHoldingsScopesToTheAccountThatHoldsTheToken(t *testing.T) {
	root := common.HexToAddress("0xR00T")
	proxy := common.HexToAddress("0xDSA")
	staked := common.HexToAddress("0x7f39C581F595B53c5cb19bD0b3f8dA6c935E2Ca0")

	attributed := func(group Group) Group {
		group.ID = "attributed:instadapp:" + strings.ToLower(proxy.Hex()) + ":" + group.ID
		metadata := map[string]any{"attributedAccount": proxy}
		for key, value := range group.Metadata {
			metadata[key] = value
		}
		group.Metadata = metadata
		return group
	}

	snapshots := suppressDuplicateHoldings([]Snapshot{
		{
			ProtocolID: "lido",
			ChainID:    Ethereum,
			Account:    root,
			Groups: []Group{attributed(Group{
				ID: "lido:wsteth",
				Components: []Component{NewComponent(
					"asset",
					Token{ChainID: Ethereum, Address: staked, Decimals: 18},
					big.NewInt(5),
					Source{Contract: staked, Method: "balanceOf"},
				)},
			})},
		},
		{
			ProtocolID:   walletProtocolID,
			ProtocolName: "Wallet",
			ChainID:      Ethereum,
			Account:      root,
			Groups: []Group{
				holdingGroup("token", staked, "7"),
				attributed(holdingGroup("token", staked, "5")),
			},
		},
	})

	var wallet Snapshot
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID == walletProtocolID {
			wallet = snapshot
		}
	}
	if len(wallet.Groups) != 1 {
		t.Fatalf("holdings = %+v, want the root balance only", wallet.Groups)
	}
	if _, exists := wallet.Groups[0].Metadata["attributedAccount"]; exists {
		t.Fatal("the proxy's holding survived even though Lido already counts it")
	}
	if wallet.Groups[0].Components[0].AmountRaw != "7" {
		t.Fatalf(
			"surviving holding = %s, want the root's 7: a position held by an attributed "+
				"account must not suppress the root wallet's own balance",
			wallet.Groups[0].Components[0].AmountRaw,
		)
	}
}
