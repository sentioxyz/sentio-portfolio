package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type recordingWalletBalanceProvider struct {
	requests []WalletBalanceRequest
	result   WalletBalanceResult
	err      error
}

func (p *recordingWalletBalanceProvider) WalletBalances(
	_ context.Context,
	request WalletBalanceRequest,
) (WalletBalanceResult, error) {
	copyRequest := WalletBalanceRequest{
		RootAccount: request.RootAccount,
		Targets:     append([]WalletBalanceTarget(nil), request.Targets...),
	}
	p.requests = append(p.requests, copyRequest)
	return p.result, p.err
}

func TestProviderWalletGroupsPreservesHoldingShape(t *testing.T) {
	longTailZero := common.HexToAddress("0x2222222222222222222222222222222222222222")
	groups, err := providerWalletGroups(
		context.Background(),
		nil,
		BlockRef{},
		Ethereum,
		common.HexToAddress("0xabc"),
		walletProviderAccount{
			exactBlock: true,
			balances: []WalletBalance{
				{
					Token:     Token{ChainID: Ethereum, Symbol: "Ether", Decimals: 18},
					AmountRaw: "1500000000000000000",
					Native:    true,
				},
				{
					Token: Token{
						ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6,
					},
					AmountRaw:        "250000000",
					MetadataComplete: true,
				},
				{
					Token: Token{
						ChainID: Ethereum, Address: longTailZero,
					},
					AmountRaw: "0",
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want native and USDC only", groups)
	}

	native, exists := groupByID(groups, walletNativeGroupID)
	if !exists {
		t.Fatal("native holding is missing")
	}
	nativeComponent := native.Components[0]
	if nativeComponent.Token.Address != walletNativeCoin[Ethereum].Wrapped ||
		nativeComponent.Token.Symbol != walletNativeCoin[Ethereum].Symbol ||
		nativeComponent.AmountRaw != "1500000000000000000" ||
		nativeComponent.Source.Method != "eth_getBalance" ||
		nativeComponent.Metadata["native"] != true {
		t.Fatalf("native component = %+v", nativeComponent)
	}

	usdc, exists := groupByID(groups, walletTokenGroupID(walletTestUSDC))
	if !exists {
		t.Fatal("USDC holding is missing")
	}
	usdcComponent := usdc.Components[0]
	if usdcComponent.Token.Symbol != "USDC" || usdcComponent.Token.Decimals != 6 ||
		usdcComponent.AmountRaw != "250000000" ||
		usdcComponent.Source.Contract != walletTestUSDC ||
		usdcComponent.Source.Method != "balanceOf" {
		t.Fatalf("USDC component = %+v", usdcComponent)
	}
}

func TestProviderWalletTokenReadsIncompleteLongTailMetadataAtTheCanonicalBlock(t *testing.T) {
	longTail := common.HexToAddress("0x2222222222222222222222222222222222222222")
	symbolRaw := [32]byte{}
	copy(symbolRaw[:], "LONG")
	symbolResult, err := erc20Bytes32SymbolABI.Methods["symbol"].Outputs.Pack(symbolRaw)
	if err != nil {
		t.Fatal(err)
	}
	decimalsResult, err := erc20ABI.Methods["decimals"].Outputs.Pack(uint8(9))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		handle := func(call walletTestRequest) map[string]any {
			response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
			switch call.Method {
			case "eth_chainId":
				response["result"] = "0x1"
			case "eth_call":
				var object struct {
					Data string `json:"data"`
				}
				if err := json.Unmarshal(call.Params[0], &object); err != nil {
					t.Fatalf("decode call object: %v", err)
				}
				switch object.Data {
				case "0x" + common.Bytes2Hex(erc20ABI.Methods["decimals"].ID):
					response["result"] = "0x" + common.Bytes2Hex(decimalsResult)
				case "0x" + common.Bytes2Hex(erc20ABI.Methods["symbol"].ID):
					// A bytes32 result intentionally fails the string decoder, then succeeds
					// through readToken's legacy-token fallback using the same selector.
					response["result"] = "0x" + common.Bytes2Hex(symbolResult)
				default:
					t.Fatalf("unexpected call data %q", object.Data)
				}
			default:
				t.Fatalf("unexpected method %q", call.Method)
			}
			return response
		}
		writer.Header().Set("Content-Type", "application/json")
		if len(raw) > 0 && raw[0] == '[' {
			var calls []walletTestRequest
			if err := json.Unmarshal(raw, &calls); err != nil {
				t.Fatalf("decode batch: %v", err)
			}
			responses := make([]map[string]any, len(calls))
			for index, call := range calls {
				responses[index] = handle(call)
			}
			_ = json.NewEncoder(writer).Encode(responses)
			return
		}
		var call walletTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			t.Fatalf("decode call: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(handle(call))
	}))
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	token, err := providerWalletToken(
		context.Background(),
		client,
		BlockRef{ChainID: Ethereum, Number: 999},
		Token{ChainID: Ethereum, Address: longTail},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if token != (Token{ChainID: Ethereum, Address: longTail, Symbol: "LONG", Decimals: 9}) {
		t.Fatalf("enriched token = %+v", token)
	}
}

func TestProviderWalletGroupsKeepsValidBalancesAlongsideFailures(t *testing.T) {
	groups, err := providerWalletGroups(
		context.Background(),
		nil,
		BlockRef{},
		Ethereum,
		common.HexToAddress("0xabc"),
		walletProviderAccount{exactBlock: true, balances: []WalletBalance{
			{
				Token: Token{
					ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6,
				},
				AmountRaw:        "7",
				MetadataComplete: true,
			},
			{
				Token: Token{
					ChainID: Ethereum, Address: walletTestWBTC, Symbol: "WBTC", Decimals: 8,
				},
				AmountRaw:        "0x8",
				MetadataComplete: true,
			},
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "invalid raw amount") {
		t.Fatalf("error = %v, want the malformed amount reported", err)
	}
	if len(groups) != 1 || groups[0].ID != walletTokenGroupID(walletTestUSDC) {
		t.Fatalf("verified balance was discarded: %+v", groups)
	}
}

func TestProviderWalletGroupsReReadsZeroDiscoveryRowsAtSettledBlock(t *testing.T) {
	longTail := common.HexToAddress("0x2222222222222222222222222222222222222222")
	server := &walletTestServer{
		t:      t,
		native: big.NewInt(0),
		balances: map[common.Address]*big.Int{
			longTail: big.NewInt(7),
		},
	}
	account := common.HexToAddress("0xabc")
	groups, err := providerWalletGroups(
		context.Background(),
		newWalletTestClient(t, server),
		BlockRef{ChainID: Ethereum, Number: 996},
		Ethereum,
		account,
		walletProviderAccount{balances: []WalletBalance{{
			Token: Token{
				ChainID: Ethereum, Address: longTail, Symbol: "LONG", Decimals: 18,
			},
			AmountRaw:        "0",
			MetadataComplete: true,
		}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	group, exists := groupByID(groups, walletTokenGroupID(longTail))
	if !exists || group.Components[0].AmountRaw != "7" {
		t.Fatalf("settled-block balance is missing: %+v", groups)
	}
	wantCalls := 1
	if server.calls != wantCalls {
		t.Fatalf("balanceOf calls = %d, want discovered token = %d", server.calls, wantCalls)
	}
}

func TestDiscoveryOnlyPathDoesNotInventUndiscoveredTokens(t *testing.T) {
	server := &walletTestServer{
		t:      t,
		native: big.NewInt(0),
		balances: map[common.Address]*big.Int{
			walletTestUSDC: big.NewInt(7),
		},
	}
	groups, err := providerWalletGroups(
		context.Background(),
		newWalletTestClient(t, server),
		BlockRef{ChainID: Ethereum, Number: 996},
		Ethereum,
		common.HexToAddress("0xabc"),
		walletProviderAccount{balances: nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 || server.calls != 0 {
		t.Fatalf("undiscovered tokens were queried: groups=%+v calls=%d", groups, server.calls)
	}
}

func TestDiscoveryOnlyPathClassifiesInvalidBalanceResults(t *testing.T) {
	longTail := common.HexToAddress("0x2222222222222222222222222222222222222222")
	for _, test := range []struct {
		name      string
		candidate common.Address
		raw       string
		revert    bool
		wantError bool
	}{
		{name: "empty discovered candidate", candidate: longTail, raw: "0x"},
		{name: "malformed discovered balance", candidate: longTail, raw: "0x01", wantError: true},
		{name: "reverting discovered candidate", candidate: longTail, revert: true, wantError: true},
		{name: "empty previously listed token", candidate: walletTestWBTC, raw: "0x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &walletTestServer{
				t: t, native: big.NewInt(0),
				balances:   map[common.Address]*big.Int{walletTestUSDC: big.NewInt(7)},
				rawResults: map[common.Address]string{test.candidate: test.raw},
			}
			if test.revert {
				server.reverts = map[common.Address]struct{}{test.candidate: {}}
			}
			account := walletProviderAccount{balances: []WalletBalance{
				{Token: Token{ChainID: Ethereum, Address: test.candidate, Symbol: "CANDIDATE", Decimals: 18}, AmountRaw: "123", MetadataComplete: true},
				{Token: Token{ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6}, AmountRaw: "7", MetadataComplete: true},
			}}
			groups, err := providerWalletGroups(
				context.Background(), newWalletTestClient(t, server),
				BlockRef{ChainID: Ethereum, Number: 996}, Ethereum,
				common.HexToAddress("0xabc"), account,
			)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError = %t", err, test.wantError)
			}
			if test.wantError && !strings.Contains(err.Error(), test.candidate.Hex()) {
				t.Fatalf("candidate failure not reported: %v", err)
			}
			if len(groups) != 1 || groups[0].ID != walletTokenGroupID(walletTestUSDC) || groups[0].Components[0].AmountRaw != "7" {
				t.Fatalf("valid balance lost or invalid candidate retained: %+v", groups)
			}
			wantCalls := 2
			if server.calls != wantCalls {
				t.Fatalf("balanceOf calls = %d, want %d", server.calls, wantCalls)
			}
		})
	}
}

func TestConfigureWalletBalancesUsesNewerProviderBlockForDiscovery(t *testing.T) {
	client, _ := newLaggingPoolClient(t, 1_000, 1_000)
	initial, err := client.BlockByNumber(context.Background(), 996)
	if err != nil {
		t.Fatal(err)
	}
	providerBlock, err := client.BlockByNumber(context.Background(), 999)
	if err != nil {
		t.Fatal(err)
	}
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Chains: []WalletBalanceChain{{
			Block: providerBlock,
			Accounts: []WalletBalanceAccount{{
				Account: owner,
				Balances: []WalletBalance{{
					Token: Token{
						ChainID:  Ethereum,
						Address:  walletTestUSDC,
						Symbol:   "USDC",
						Decimals: 6,
					},
					AmountRaw: "7",
				}},
			}},
		}},
	}}
	chains := map[ChainID]*chainScan{
		Ethereum: {
			client: client,
			block:  initial,
			accounts: []attributedAccount{{
				Address: owner, Attribution: "wallet", Source: "direct",
			}},
		},
	}

	configureWalletBalances(context.Background(), provider, owner, chains)

	if len(provider.requests) != 1 || provider.requests[0].RootAccount != owner ||
		len(provider.requests[0].Targets) != 1 ||
		provider.requests[0].Targets[0] != (WalletBalanceTarget{ChainID: Ethereum, Account: owner}) {
		t.Fatalf("provider requests = %+v", provider.requests)
	}
	chain := chains[Ethereum]
	if chain.block != initial {
		t.Fatalf("provider latest replaced settled block: %+v", chain.block)
	}
	account, exists := chain.walletProviderAccounts[owner]
	if !exists || len(account.balances) != 1 || account.exactBlock {
		t.Fatalf("installed provider account = %+v", account)
	}
	if len(chain.walletProviderErrors) != 0 {
		t.Fatalf("provider errors = %v", chain.walletProviderErrors)
	}
}

func TestEngineUsesExactProviderBalanceAtSettledBlockAndCoinQuotePrice(t *testing.T) {
	const providerBlockNumber = uint64(996)
	pool := &laggingPool{t: t, announcedHead: 1_000, servedHead: 1_000}
	server := httptest.NewServer(pool)
	t.Cleanup(server.Close)
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Chains: []WalletBalanceChain{{
			Block: BlockRef{
				ChainID:   Ethereum,
				Number:    providerBlockNumber,
				Hash:      common.BytesToHash([]byte{byte(providerBlockNumber & 0xff)}),
				Timestamp: 1,
			},
			Accounts: []WalletBalanceAccount{{
				Account: owner,
				Balances: []WalletBalance{{
					Token:     Token{ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6},
					AmountRaw: "250000000", MetadataComplete: true,
				}},
			}},
		}},
	}}
	prices := &recordingPriceProvider{}
	engine := NewEngineWithConfig(
		map[ChainID]string{Ethereum: server.URL},
		prices,
		EngineConfig{WalletBalanceProvider: provider},
	)

	response := engine.ScanWithOptions(context.Background(), owner, ScanOptions{
		ProtocolIDs: map[string]struct{}{walletProtocolID: {}},
		ChainIDs:    map[ChainID]struct{}{Ethereum: {}},
	})

	if response.ChainBlocks[Ethereum] != providerBlockNumber {
		t.Fatalf("response block = %d, want settled block %d", response.ChainBlocks[Ethereum], providerBlockNumber)
	}
	if len(response.Snapshots) != 1 || response.Snapshots[0].Block.Number != providerBlockNumber {
		t.Fatalf("snapshots = %+v", response.Snapshots)
	}
	group, exists := groupByID(response.Snapshots[0].Groups, walletTokenGroupID(walletTestUSDC))
	if !exists || group.Components[0].AmountRaw != "250000000" {
		t.Fatalf("provider balance is missing: %+v", response.Snapshots[0].Groups)
	}
	if group.Components[0].PriceUSD == nil || *group.Components[0].PriceUSD != 1 {
		t.Fatalf("component price = %v, want CoinQuote test provider price 1", group.Components[0].PriceUSD)
	}
	if len(prices.tokens) != 1 || AssetForToken(prices.tokens[0]) != AssetForToken(group.Components[0].Token) {
		t.Fatalf("price provider tokens = %+v", prices.tokens)
	}
	if len(response.Errors) != 0 {
		t.Fatalf("scan errors = %+v", response.Errors)
	}
}

func TestConfigureWalletBalancesReportsSameHeightHashMismatch(t *testing.T) {
	client, _ := newLaggingPoolClient(t, 1_000, 1_000)
	initial, err := client.BlockByNumber(context.Background(), 996)
	if err != nil {
		t.Fatal(err)
	}
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Chains: []WalletBalanceChain{{
			Block: BlockRef{
				ChainID: Ethereum,
				Number:  996,
				Hash:    common.HexToHash("0x1234"),
			},
			Accounts: []WalletBalanceAccount{{Account: owner}},
		}},
	}}
	chains := map[ChainID]*chainScan{
		Ethereum: {
			client: client,
			block:  initial,
			accounts: []attributedAccount{{
				Address: owner, Attribution: "wallet", Source: "direct",
			}},
		},
	}

	configureWalletBalances(context.Background(), provider, owner, chains)

	chain := chains[Ethereum]
	if chain.block != initial {
		t.Fatalf("unverified provider block became canonical: %+v", chain.block)
	}
	account, exists := chain.walletProviderAccounts[owner]
	if !exists || account.exactBlock {
		t.Fatalf("same-height fork must be discovery-only: %+v", chain.walletProviderAccounts)
	}
	joined := errors.Join(chain.walletProviderErrors...)
	if joined == nil || !strings.Contains(joined.Error(), "block hash mismatch") {
		t.Fatalf("provider errors = %v, want hash mismatch", joined)
	}
}

func TestConfigureWalletBalancesRequestFailureReportsCoverageGap(t *testing.T) {
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{err: errors.New("provider unavailable")}
	chain := &chainScan{
		block: BlockRef{ChainID: Ethereum, Number: 996},
		accounts: []attributedAccount{{
			Address: owner, Attribution: "wallet", Source: "direct",
		}},
	}
	configureWalletBalances(
		context.Background(),
		provider,
		owner,
		map[ChainID]*chainScan{Ethereum: chain},
	)
	if chain.walletProviderAccounts != nil {
		t.Fatalf("request-level failure installed provider data: %+v", chain.walletProviderAccounts)
	}
	joined := errors.Join(chain.walletProviderErrors...)
	if joined == nil || !strings.Contains(joined.Error(), "provider unavailable") {
		t.Fatalf("provider errors = %v", joined)
	}
}

func TestConfigureWalletBalancesFailureDoesNotAddMissingResultError(t *testing.T) {
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Failures: []WalletBalanceFailure{{
			ChainID: Ethereum,
			Account: owner,
			Message: "genuine provider failure",
		}},
	}}
	chain := &chainScan{
		block: BlockRef{ChainID: Ethereum, Number: 996},
		accounts: []attributedAccount{{
			Address: owner, Attribution: "wallet", Source: "direct",
		}},
	}

	configureWalletBalances(
		context.Background(),
		provider,
		owner,
		map[ChainID]*chainScan{Ethereum: chain},
	)

	joined := errors.Join(chain.walletProviderErrors...)
	if len(chain.walletProviderErrors) != 1 || joined == nil ||
		!strings.Contains(joined.Error(), "genuine provider failure") ||
		strings.Contains(joined.Error(), "no chain result") ||
		strings.Contains(joined.Error(), "no account result") {
		t.Fatalf("provider errors = %v", joined)
	}
}

func TestConfigureWalletBalancesChainFailureDoesNotAddMissingResultError(t *testing.T) {
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Failures: []WalletBalanceFailure{{
			ChainID: Ethereum,
			Message: "genuine chain failure",
		}},
	}}
	chain := &chainScan{
		block: BlockRef{ChainID: Ethereum, Number: 996},
		accounts: []attributedAccount{{
			Address: owner, Attribution: "wallet", Source: "direct",
		}},
	}

	configureWalletBalances(
		context.Background(),
		provider,
		owner,
		map[ChainID]*chainScan{Ethereum: chain},
	)

	joined := errors.Join(chain.walletProviderErrors...)
	if len(chain.walletProviderErrors) != 1 || joined == nil ||
		!strings.Contains(joined.Error(), "genuine chain failure") ||
		strings.Contains(joined.Error(), "no chain result") ||
		strings.Contains(joined.Error(), "no account result") {
		t.Fatalf("provider errors = %v", joined)
	}
}

func TestConfigureWalletBalancesReportsUnclassifiedMissingTarget(t *testing.T) {
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	provider := &recordingWalletBalanceProvider{}
	chain := &chainScan{
		block: BlockRef{ChainID: Ethereum, Number: 996},
		accounts: []attributedAccount{{
			Address: owner, Attribution: "wallet", Source: "direct",
		}},
	}

	configureWalletBalances(
		context.Background(),
		provider,
		owner,
		map[ChainID]*chainScan{Ethereum: chain},
	)

	joined := errors.Join(chain.walletProviderErrors...)
	if joined == nil || !strings.Contains(joined.Error(), "no chain result") {
		t.Fatalf("provider errors = %v, want unclassified missing target", joined)
	}
}

func TestConfigureWalletBalancesAssetFailureReportsIncompleteAccount(t *testing.T) {
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	block := BlockRef{ChainID: Ethereum, Number: 996, Hash: common.HexToHash("0x996")}
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{
		Chains: []WalletBalanceChain{{
			Block: block,
			Accounts: []WalletBalanceAccount{{
				Account: owner,
				Balances: []WalletBalance{{
					Token:     Token{ChainID: Ethereum, Address: walletTestUSDC},
					AmountRaw: "7",
				}},
			}},
		}},
		Failures: []WalletBalanceFailure{{
			ChainID: Ethereum,
			Account: owner,
			Asset:   &AssetID{ChainID: Ethereum, Address: walletTestUSDC},
			Message: "account result is incomplete",
		}},
	}}
	chain := &chainScan{
		block: block,
		accounts: []attributedAccount{{
			Address: owner, Attribution: "wallet", Source: "direct",
		}},
	}
	configureWalletBalances(
		context.Background(),
		provider,
		owner,
		map[ChainID]*chainScan{Ethereum: chain},
	)
	if chain.walletProviderAccounts != nil {
		t.Fatalf("failed account installed provider data: %+v", chain.walletProviderAccounts)
	}
	if len(chain.walletProviderErrors) != 1 ||
		!strings.Contains(chain.walletProviderErrors[0].Error(), "account result is incomplete") {
		t.Fatalf("provider errors = %v", chain.walletProviderErrors)
	}
}

func TestConfigureWalletBalancesUsesDiscoveryForHistoricalBlocks(t *testing.T) {
	owner := common.HexToAddress("0x1")
	pin := BlockRef{ChainID: Ethereum, Number: 996, Hash: common.HexToHash("0x996"), Fixed: true}
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{Chains: []WalletBalanceChain{{
		Block: BlockRef{ChainID: Ethereum, Number: 1000, Hash: common.HexToHash("0x1000")},
		Accounts: []WalletBalanceAccount{{Account: owner, Balances: []WalletBalance{{
			Token:     Token{ChainID: Ethereum, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6},
			AmountRaw: "123", MetadataComplete: true,
		}}}},
	}}}}
	chain := &chainScan{block: pin, accounts: []attributedAccount{{Address: owner}}}
	configureWalletBalances(context.Background(), provider, owner, map[ChainID]*chainScan{Ethereum: chain})
	if len(provider.requests) != 1 || chain.block != pin {
		t.Fatal("historical discovery bypassed provider or changed the pin")
	}
	account, ok := chain.walletProviderAccounts[owner]
	if !ok || account.exactBlock {
		t.Fatal("current provider balance accepted as historical")
	}
	if err := errors.Join(chain.walletProviderErrors...); err == nil || !strings.Contains(err.Error(), "historical wallet token discovery is incomplete") {
		t.Fatalf("missing historical coverage error: %v", err)
	}
	server := &walletTestServer{t: t, native: big.NewInt(0), balances: map[common.Address]*big.Int{walletTestUSDC: big.NewInt(7)}}
	groups, err := providerWalletGroups(context.Background(), newWalletTestClient(t, server), pin, Ethereum, owner, account)
	if err != nil || len(groups) != 1 || groups[0].Components[0].AmountRaw != "7" || server.calls != 1 {
		t.Fatalf("historical balance was not re-read: groups=%+v calls=%d err=%v", groups, server.calls, err)
	}
}

func TestConfigureWalletBalancesHonorsPerAccountBlockSamples(t *testing.T) {
	owner, other := common.HexToAddress("0x1"), common.HexToAddress("0x2")
	pin := BlockRef{ChainID: Ethereum, Number: 996, Hash: common.HexToHash("0x996")}
	newer := BlockRef{ChainID: Ethereum, Number: 1000, Hash: common.HexToHash("0x1000")}
	provider := &recordingWalletBalanceProvider{result: WalletBalanceResult{Chains: []WalletBalanceChain{{
		Block: pin, Accounts: []WalletBalanceAccount{{Account: owner}, {Account: other, Block: &newer}},
	}}}}
	chain := &chainScan{block: pin, accounts: []attributedAccount{{Address: owner}, {Address: other}}}
	configureWalletBalances(context.Background(), provider, owner, map[ChainID]*chainScan{Ethereum: chain})
	if !chain.walletProviderAccounts[owner].exactBlock || chain.walletProviderAccounts[other].exactBlock || len(chain.walletProviderErrors) != 0 {
		t.Fatalf("per-account samples lost: accounts=%+v errors=%v", chain.walletProviderAccounts, chain.walletProviderErrors)
	}
}

func TestWalletWithoutDiscoveryReturnsOnlyNativeAndCoverageError(t *testing.T) {
	owner := common.HexToAddress("0x1")
	for _, provider := range []WalletBalanceProvider{nil, &recordingWalletBalanceProvider{err: errors.New("provider unavailable")}} {
		chain := &chainScan{block: BlockRef{ChainID: Ethereum, Number: 996}, accounts: []attributedAccount{{Address: owner}}}
		configureWalletBalances(context.Background(), provider, owner, map[ChainID]*chainScan{Ethereum: chain})
		if len(chain.walletProviderErrors) == 0 {
			t.Fatal("missing discovery reported as complete wallet")
		}
		server := &walletTestServer{t: t, native: big.NewInt(1), balances: map[common.Address]*big.Int{walletTestUSDC: big.NewInt(7)}}
		groups, err := newWalletAdapter().Positions(context.Background(), newWalletTestClient(t, server), chain.block, owner)
		if err != nil || len(groups) != 1 || groups[0].ID != walletNativeGroupID || server.calls != 0 {
			t.Fatalf("static ERC20 fallback survived: groups=%+v calls=%d err=%v", groups, server.calls, err)
		}
	}
}

func TestParseWalletBalanceAmountKeepsArbitraryPrecision(t *testing.T) {
	raw := new(big.Int).Exp(big.NewInt(10), big.NewInt(100), nil).String()
	amount, err := parseWalletBalanceAmount(raw)
	if err != nil {
		t.Fatal(err)
	}
	if amount.String() != raw {
		t.Fatalf("amount = %s, want %s", amount, raw)
	}
}

func TestValidatedWalletTokenSymbolBoundsUntrustedMetadata(t *testing.T) {
	for _, symbol := range []string{
		"",
		"   ",
		"BAD\nSYMBOL",
		"BAD\u202eSYMBOL",
		strings.Repeat("A", maxWalletTokenSymbolBytes+1),
		string([]byte{0xff}),
	} {
		if _, err := validatedWalletTokenSymbol(symbol); err == nil {
			t.Fatalf("unsafe symbol %q was accepted", symbol)
		}
	}
	if symbol, err := validatedWalletTokenSymbol("  USDC  "); err != nil || symbol != "USDC" {
		t.Fatalf("safe symbol = %q, %v", symbol, err)
	}
}
