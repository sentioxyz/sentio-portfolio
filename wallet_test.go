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
	t          *testing.T
	native     *big.Int
	balances   map[common.Address]*big.Int
	reverts    map[common.Address]struct{}
	rawResults map[common.Address]string
	code       map[common.Address]struct{}
	calls      int
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
		if raw, exists := s.rawResults[address]; exists {
			response["result"] = raw
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
)

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

// walletHoldings returns the holdings that survived suppression, none if the snapshot is gone.
func walletHoldings(snapshots []Snapshot) []Group {
	for _, snapshot := range snapshots {
		if snapshot.ProtocolID == walletProtocolID {
			return snapshot.Groups
		}
	}
	return nil
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

// Supplying USDC to a lending market reports USDC as the component token while the account holds
// the aToken. The source is the shape the Aave adapter really emits: it reads the supply from the
// data provider, so only the declared holding names the aToken. The wallet's aToken is the same
// balance and must go; the wallet's own USDC is a different balance and must survive.
func TestSuppressDuplicateHoldingsKeepsAnUnderlyingAssetHeldInTheWallet(t *testing.T) {
	aToken := common.HexToAddress("0x98C23E9d8f34FEFb1B7BD6a91B7FF122F4e16F5c")
	dataProvider := common.HexToAddress("0x497a1994c46d4f6C864904A9f1fac6328Cb7C8a6")
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
					Source{
						Contract: dataProvider,
						Method:   "getUserReserveData.currentATokenBalance",
						Holds:    []common.Address{aToken},
					},
				)},
			}},
		},
		holdingSnapshot(
			holdingGroup("token", walletTestUSDC, "9"),
			holdingGroup("token", aToken, "100"),
		),
	})
	holdings := walletHoldings(snapshots)
	if _, exists := groupByID(holdings, walletTokenGroupID(walletTestUSDC)); !exists {
		t.Error("wallet USDC was dropped because a market reports USDC as its underlying")
	}
	if _, exists := groupByID(holdings, walletTokenGroupID(aToken)); exists {
		t.Error("the aToken is counted twice: the market already reports it as supplied USDC")
	}
}

// An adapter often reads an amount through a converter, an oracle or a data provider and names
// that contract in Source, or describes the read without the word balanceOf. Declared holdings
// say which ERC-20s the account held, so the wallet drops exactly those and keeps the rest.
func TestSuppressDuplicateHoldingsDropsDeclaredHoldings(t *testing.T) {
	converter := common.HexToAddress("0x0000000000000000000000000000000000000c01")
	receipt := common.HexToAddress("0x0000000000000000000000000000000000000a01")
	gauge := common.HexToAddress("0x0000000000000000000000000000000000000a02")
	tests := []struct {
		name       string
		source     Source
		suppressed []common.Address
		kept       []common.Address
	}{
		{
			// The converter is not held: declared holdings replace the inference from Contract.
			name: "the source is a converter",
			source: Source{
				Contract: converter, Method: "mETHToETH(mETH.balanceOf)",
				Holds: []common.Address{receipt},
			},
			suppressed: []common.Address{receipt},
			kept:       []common.Address{converter, walletTestUSDC},
		},
		{
			name: "the method never says balanceOf",
			source: Source{
				Contract: receipt, Method: "convertToAssets(vault balance + gauge balance)",
				Holds: []common.Address{receipt, gauge},
			},
			suppressed: []common.Address{receipt, gauge},
			kept:       []common.Address{walletTestUSDC},
		},
		{
			// A staking contract's internal balance is no ERC-20, whatever the method mentions.
			name: "an empty declaration holds nothing",
			source: Source{
				Contract: receipt, Method: "Unipool.balanceOf * pair.getReserves / totalSupply",
				Holds: []common.Address{},
			},
			kept: []common.Address{receipt, walletTestUSDC},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			holdings := make([]Group, 0, len(test.suppressed)+len(test.kept))
			for _, token := range append(append([]common.Address(nil), test.suppressed...), test.kept...) {
				holdings = append(holdings, holdingGroup("token", token, "1"))
			}
			remaining := walletHoldings(suppressDuplicateHoldings([]Snapshot{
				{
					ProtocolID: "protocol",
					ChainID:    Ethereum,
					Groups: []Group{{
						ID: "position",
						Components: []Component{NewComponent(
							"asset",
							Token{ChainID: Ethereum, Address: walletTestUSDC, Decimals: 6},
							big.NewInt(1),
							test.source,
						)},
					}},
				},
				holdingSnapshot(holdings...),
			}))
			for _, token := range test.suppressed {
				if _, exists := groupByID(remaining, walletTokenGroupID(token)); exists {
					t.Errorf("%s is counted twice: the position already consumed it", token)
				}
			}
			for _, token := range test.kept {
				if _, exists := groupByID(remaining, walletTokenGroupID(token)); !exists {
					t.Errorf("%s was dropped although no position holds it", token)
				}
			}
		})
	}
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
