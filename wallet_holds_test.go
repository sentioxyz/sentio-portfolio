package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// The tests in this file check that an adapter declares every ERC-20 balance of the account it
// consumed, whatever contract its Source names: the wallet reports each of those tokens too,
// and drops only the ones a protocol component declares in Source.Holds.

var holdsTestAccount = common.HexToAddress("0x00000000000000000000000000000000000000a1")

// contractStub answers eth_call for exact calldata, the way a node pinned to one block would,
// and records each balanceOf of the account it answers.
type contractStub struct {
	t       *testing.T
	chainID ChainID
	account common.Address
	results map[contractStubCall]hexutil.Bytes

	mu       sync.Mutex
	balances map[common.Address]*big.Int
}

type contractStubCall struct {
	contract common.Address
	data     string
}

func newContractStub(t *testing.T, chainID ChainID, account common.Address) *contractStub {
	return &contractStub{
		t:        t,
		chainID:  chainID,
		account:  account,
		results:  make(map[contractStubCall]hexutil.Bytes),
		balances: make(map[common.Address]*big.Int),
	}
}

// answer makes contract return outputs when it is called with method(args...).
func (s *contractStub) answer(
	contract common.Address,
	contractABI abi.ABI,
	method string,
	args []any,
	outputs ...any,
) {
	s.t.Helper()
	data, err := contractABI.Pack(method, args...)
	if err != nil {
		s.t.Fatalf("encode %s call: %v", method, err)
	}
	result, err := contractABI.Methods[method].Outputs.Pack(outputs...)
	if err != nil {
		s.t.Fatalf("encode %s result: %v", method, err)
	}
	s.results[contractStubCall{contract: contract, data: hexutil.Encode(data)}] = result
}

// balance makes token report amount as the account's balance.
func (s *contractStub) balance(token common.Address, amount *big.Int) {
	s.t.Helper()
	s.answer(token, erc20ABI, "balanceOf", []any{s.account}, amount)
}

func (s *contractStub) dial() *RPCClient {
	s.t.Helper()
	server := httptest.NewServer(s)
	s.t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), s.chainID, server.URL)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(client.Close)
	return client
}

func (s *contractStub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode RPC call: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(s.respond(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode RPC batch: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = s.respond(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func (s *contractStub) respond(call rpcTestRequest) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = hexutil.EncodeUint64(uint64(s.chainID))
		return response
	case "eth_call":
		var input struct {
			To   common.Address `json:"to"`
			Data hexutil.Bytes  `json:"data"`
		}
		if len(call.Params) > 0 && json.Unmarshal(call.Params[0], &input) == nil {
			key := contractStubCall{contract: input.To, data: hexutil.Encode(input.Data)}
			if result, exists := s.results[key]; exists {
				s.recordBalanceRead(input.To, input.Data, result)
				response["result"] = result
				return response
			}
		}
		s.t.Errorf("unexpected eth_call %s", call.Params)
	default:
		s.t.Errorf("unexpected RPC method %q", call.Method)
	}
	response["error"] = map[string]any{"code": 3, "message": "execution reverted"}
	return response
}

func (s *contractStub) recordBalanceRead(contract common.Address, data, result hexutil.Bytes) {
	if len(data) != 36 || !bytes.Equal(data[:4], erc20ABI.Methods["balanceOf"].ID) ||
		common.BytesToAddress(data[4:]) != s.account {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[contract] = new(big.Int).SetBytes(result)
}

// assertHolds checks that the groups make the wallet drop exactly want, and that want covers
// every balance of the account the adapter read and found non-zero.
func (s *contractStub) assertHolds(groups []Group, want ...common.Address) {
	s.t.Helper()
	held := assertHeldExactly(s.t, s.chainID, s.account, groups, want...)
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, amount := range s.balances {
		if _, declared := held[token]; amount.Sign() > 0 && !declared {
			s.t.Errorf("the adapter read the account's %s balance but declares no holding of it", token)
		}
	}
}

// assertHeldExactly checks that the groups make the wallet drop exactly want: every token the
// account held through them, and nothing else it holds.
func assertHeldExactly(
	t *testing.T,
	chainID ChainID,
	account common.Address,
	groups []Group,
	want ...common.Address,
) map[common.Address]struct{} {
	t.Helper()
	held := walletHeldContracts([]Snapshot{{
		ProtocolID: "protocol", ChainID: chainID, Account: account, Groups: groups,
	}})[holdingScope{ChainID: chainID, Account: account}]
	wanted := make(map[common.Address]struct{}, len(want))
	for _, token := range want {
		wanted[token] = struct{}{}
		if _, exists := held[token]; !exists {
			t.Errorf("no component declares holding %s, so the wallet counts it a second time", token)
		}
	}
	for token := range held {
		if _, exists := wanted[token]; !exists {
			t.Errorf("a component declares holding %s, so the wallet would drop a balance nothing reports", token)
		}
	}
	return held
}

func TestMethDeclaresTheMETHItConverts(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 23_000_000, Timestamp: 1_752_000_000, Fixed: true}
	shares := big.NewInt(1_709_061_383_420_883_701)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(methTokenAddress, methABI, "stakingContract", nil, methStakingAddress)
	stub.answer(methTokenAddress, methABI, "unstakeRequestsManagerContract", nil, methManagerAddress)
	stub.answer(methStakingAddress, methStakingABI, "mETH", nil, methTokenAddress)
	stub.answer(methStakingAddress, methStakingABI, "unstakeRequestsManager", nil, methManagerAddress)
	stub.answer(methManagerAddress, methManagerABI, "mETH", nil, methTokenAddress)
	stub.answer(methManagerAddress, methManagerABI, "stakingContract", nil, methStakingAddress)
	stub.balance(methTokenAddress, shares)
	stub.answer(methStakingAddress, methStakingABI, "mETHToETH", []any{shares},
		big.NewInt(1_879_048_934_696_632_998))

	// The withdrawal index has caught up with the scanned block and holds no request.
	indexer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/status":
			_, _ = fmt.Fprintf(writer, `{"processors":[{"version":1,"versionState":"ACTIVE",`+
				`"processorStatus":{"state":"PROCESSING"},"states":[{"chainId":"1",`+
				`"processedBlockNumber":"%d","status":{"state":"PROCESSING_LATEST"}}]}]}`, block.Number)
		case "/graphql":
			_, _ = fmt.Fprintf(writer, `{"data":{"indexerCheckpoints":[{"blockNumber":"%d",`+
				`"timestampMs":"%d"}],"withdrawalRequests":[]}}`, block.Number, block.Timestamp*1_000)
		default:
			t.Errorf("unexpected indexer request %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(indexer.Close)
	adapter := newMethAdapter(SentioIndexerConfig{
		GraphQLURL: indexer.URL + "/graphql", StatusURL: indexer.URL + "/status", ProcessorVersion: "1",
	})

	groups, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "meth" {
		t.Fatalf("groups = %+v, want the staked mETH position", groups)
	}
	stub.assertHolds(groups, methTokenAddress)
}

func TestRenzoDeclaresTheEzETHItRedeems(t *testing.T) {
	// pzETH does not exist yet at this block, so ezETH is the only receipt read.
	block := BlockRef{ChainID: Ethereum, Number: 19_000_000, Fixed: true}
	oracle := common.HexToAddress("0x0000000000000000000000000000000000000c01")
	balance, _ := new(big.Int).SetString("623816091140255778080", 10)
	totalSupply, _ := new(big.Int).SetString("40731182043490096493965", 10)
	totalTVL, _ := new(big.Int).SetString("44252487827094962084324", 10)
	redeemed, _ := new(big.Int).SetString("677746448655844846214", 10)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.balance(renzoEZETH.Address, balance)
	stub.answer(renzoRestakeManager, renzoRestakeManagerABI, "ezETH", nil, renzoEZETH.Address)
	stub.answer(renzoRestakeManager, renzoRestakeManagerABI, "renzoOracle", nil, oracle)
	stub.answer(renzoRestakeManager, renzoRestakeManagerABI, "calculateTVLs", nil,
		[][]*big.Int{}, []*big.Int{}, totalTVL)
	stub.answer(renzoEZETH.Address, renzoSupplyABI, "totalSupply", nil, totalSupply)
	stub.answer(oracle, renzoOracleABI, "calculateRedeemAmount", []any{balance, totalSupply, totalTVL}, redeemed)

	groups, err := newRenzoAdapter(SentioIndexerConfig{}).(*RenzoAdapter).readMainnetReceipts(
		context.Background(), stub.dial(), block, holdsTestAccount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "ezeth" {
		t.Fatalf("groups = %+v, want the ezETH position", groups)
	}
	stub.assertHolds(groups, renzoEZETH.Address)
}

func TestStaderDeclaresTheETHxItConverts(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 20_000_000, Fixed: true}
	shares := big.NewInt(2_000_000_000_000_000_000)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	for method, address := range map[string]common.Address{
		"getETHxToken":             staderETHxAddress,
		"getStakePoolManager":      staderStakePoolManager,
		"getUserWithdrawManager":   staderWithdrawalManager,
		"getSDUtilityPool":         staderSDUtilityPool,
		"getSDIncentiveController": staderSDIncentiveController,
		"getSDCollateral":          staderSDCollateralAddress,
		"getPoolUtils":             staderPoolUtilsAddress,
		"getStaderToken":           staderSDAddress,
	} {
		stub.answer(staderConfigAddress, staderConfigABI, method, nil, address)
	}
	stub.balance(staderETHxAddress, shares)
	stub.answer(staderWithdrawalManager, staderWithdrawalABI, "getRequestIdsByUser",
		[]any{holdsTestAccount}, []*big.Int{})
	stub.answer(staderSDCollateralAddress, staderSDCollateralABI, "operatorSDBalance",
		[]any{holdsTestAccount}, new(big.Int))
	stub.answer(staderPoolUtilsAddress, staderPoolUtilsABI, "isExistingOperator",
		[]any{holdsTestAccount}, false)
	stub.answer(staderSDUtilityPool, staderSDUtilityPoolABI, "getDelegatorLatestSDBalance",
		[]any{holdsTestAccount}, new(big.Int))
	stub.answer(staderSDUtilityPool, staderSDUtilityPoolABI, "getRequestIdsByDelegator",
		[]any{holdsTestAccount}, []*big.Int{})
	stub.answer(staderStakePoolManager, staderStakePoolManagerABI, "convertToAssets", []any{shares},
		big.NewInt(2_150_000_000_000_000_000))

	groups, err := newStaderAdapter().Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "ethx" {
		t.Fatalf("groups = %+v, want the ETHx position", groups)
	}
	stub.assertHolds(groups, staderETHxAddress)
}

// MaticX is read three ways over its history; each reports the same wallet balance.
func TestStaderDeclaresTheMaticXItConverts(t *testing.T) {
	deployment := staderPolygonDeployment
	shares := big.NewInt(1_250_000_000_000_000_000)
	for _, test := range []struct {
		name   string
		block  uint64
		answer func(stub *contractStub)
	}{
		{name: "before the rate provider", block: deployment.rateActivationBlock - 1},
		{
			name:  "through the rate provider",
			block: deployment.rateActivationBlock,
			answer: func(stub *contractStub) {
				stub.answer(deployment.rateProvider, staderPolygonRateProviderABI, "getRate", nil,
					big.NewInt(1_100_000_000_000_000_000))
			},
		},
		{
			name:  "through the child pool",
			block: deployment.conversionActivationBlock,
			answer: func(stub *contractStub) {
				stub.answer(deployment.childPool, staderPolygonChildPoolABI, "convertMaticXToMatic",
					[]any{shares}, big.NewInt(1_380_000_000_000_000_000), big.NewInt(10), big.NewInt(11))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := newContractStub(t, Polygon, holdsTestAccount)
			stub.balance(deployment.liquidToken.Address, shares)
			if test.answer != nil {
				test.answer(stub)
			}
			groups, err := newStaderAdapter().Positions(
				context.Background(),
				stub.dial(),
				BlockRef{ChainID: Polygon, Number: test.block, Fixed: true},
				holdsTestAccount,
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(groups) != 1 || groups[0].ID != "maticx" {
				t.Fatalf("groups = %+v, want the MaticX position", groups)
			}
			stub.assertHolds(groups, deployment.liquidToken.Address)
		})
	}
}

func TestAsterDeclaresTheReceiptsItConverts(t *testing.T) {
	block := BlockRef{ChainID: BSC, Number: 45_000_000, Fixed: true}
	slisBNB := common.HexToAddress("0x00000000000000000000000000000000000000b1")
	cake := common.HexToAddress("0x00000000000000000000000000000000000000b2")
	btcb := common.HexToAddress("0x00000000000000000000000000000000000000b3")
	usdf := common.HexToAddress("0x00000000000000000000000000000000000000b4")
	shares := big.NewInt(1_000_000_000_000_000_000)
	stub := newContractStub(t, BSC, holdsTestAccount)
	for _, receipt := range []Token{asterAsBTC, asterAsBNB, asterAsUSDF, asterAsCAKE} {
		stub.balance(receipt.Address, shares)
	}
	stub.answer(asterAsBNBMinter, asterMinterABI, "token", nil, slisBNB)
	stub.answer(asterAsBNBMinter, asterMinterABI, "asBnb", nil, asterAsBNB.Address)
	stub.answer(asterAsBNBMinter, asterMinterABI, "convertToTokens", []any{shares},
		big.NewInt(1_020_000_000_000_000_000))
	stub.answer(asterAsCAKEMinter, asterMinterABI, "token", nil, cake)
	stub.answer(asterAsCAKEMinter, asterMinterABI, "assToken", nil, asterAsCAKE.Address)
	stub.answer(asterAsCAKEMinter, asterMinterABI, "convertToTokens", []any{shares},
		big.NewInt(1_030_000_000_000_000_000))
	stub.answer(asterAsBTCMinter, asterEarnRateABI, "supportAssToken", []any{asterAsBTC.Address},
		asterAsBTC.Address, btcb, uint8(18), big.NewInt(100_000_000), new(big.Int),
		false, false, common.Address{}, false, false)
	stub.answer(asterAsUSDFMinter, asterUSDFRateABI, "USDF", nil, usdf)
	stub.answer(asterAsUSDFMinter, asterUSDFRateABI, "asUSDF", nil, asterAsUSDF.Address)
	stub.answer(asterAsUSDFMinter, asterUSDFRateABI, "exchangePrice", nil,
		big.NewInt(1_050_000_000_000_000_000))
	for address, symbol := range map[common.Address]string{
		slisBNB: "slisBNB", cake: "CAKE", btcb: "BTCB", usdf: "USDF",
	} {
		stub.answer(address, erc20ABI, "symbol", nil, symbol)
		stub.answer(address, erc20ABI, "decimals", nil, uint8(18))
	}

	groups, err := newAsterAdapter(SentioIndexerConfig{}).(*AsterAdapter).readYieldPositions(
		context.Background(), stub.dial(), block, holdsTestAccount,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("groups = %+v, want one position per receipt", groups)
	}
	stub.assertHolds(groups,
		asterAsBTC.Address, asterAsBNB.Address, asterAsUSDF.Address, asterAsCAKE.Address)
}

// Supply and debt are read from the data provider rather than from the aToken and the debt
// token, so only declared holdings can name the tokens the account holds them as. A reserve
// without a position costs no extra call: the stub fails any lookup it was not told to expect.
// The stable debt token is zero, as it is on every market since stable borrowing was removed.
func TestAaveDeclaresTheTokensBehindSupplyAndDebt(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 23_000_000, Fixed: true}
	dataProvider := common.HexToAddress("0x00000000000000000000000000000000000000d1")
	pool := common.HexToAddress("0x00000000000000000000000000000000000000d2")
	aToken := common.HexToAddress("0x00000000000000000000000000000000000000e1")
	variableDebtToken := common.HexToAddress("0x00000000000000000000000000000000000000e3")
	zero := new(big.Int)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(dataProvider, aaveDataProviderABI, "getAllReservesTokens", nil, []aaveReserve{
		{Symbol: "USDC", TokenAddress: walletTestUSDC},
		{Symbol: "WBTC", TokenAddress: walletTestWBTC},
	})
	stub.answer(dataProvider, aaveDataProviderABI, "getUserReserveData",
		[]any{walletTestUSDC, holdsTestAccount},
		big.NewInt(100_000_000), zero, big.NewInt(40_000_000), zero, zero, zero, zero, zero, true)
	stub.answer(walletTestUSDC, erc20ABI, "decimals", nil, uint8(6))
	stub.answer(dataProvider, aaveDataProviderABI, "getUserReserveData",
		[]any{walletTestWBTC, holdsTestAccount},
		zero, zero, zero, zero, zero, zero, zero, zero, false)
	stub.answer(walletTestWBTC, erc20ABI, "decimals", nil, uint8(8))
	stub.answer(dataProvider, aaveDataProviderABI, "getReserveTokensAddresses", []any{walletTestUSDC},
		aToken, common.Address{}, variableDebtToken)
	adapter := NewAaveAdapter("aave-test", "Aave test", map[ChainID][]aaveMarket{
		Ethereum: {{Label: "Core", Pool: pool, DataProvider: dataProvider}},
	})

	groups, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 2 {
		t.Fatalf("groups = %+v, want one market with the USDC supply and debt", groups)
	}
	stub.assertHolds(groups, aToken, variableDebtToken)
}

// A position whose token the data provider cannot name is a broken market, not a position to
// report without its holding: the wallet would count the aToken a second time.
func TestAaveRejectsAPositionWithoutItsToken(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 23_000_000, Fixed: true}
	dataProvider := common.HexToAddress("0x00000000000000000000000000000000000000d1")
	zero := new(big.Int)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(dataProvider, aaveDataProviderABI, "getAllReservesTokens", nil, []aaveReserve{
		{Symbol: "USDC", TokenAddress: walletTestUSDC},
	})
	stub.answer(dataProvider, aaveDataProviderABI, "getUserReserveData",
		[]any{walletTestUSDC, holdsTestAccount},
		big.NewInt(100_000_000), zero, zero, zero, zero, zero, zero, zero, true)
	stub.answer(walletTestUSDC, erc20ABI, "decimals", nil, uint8(6))
	stub.answer(dataProvider, aaveDataProviderABI, "getReserveTokensAddresses", []any{walletTestUSDC},
		common.Address{}, common.Address{}, common.Address{})
	adapter := NewAaveAdapter("aave-test", "Aave test", map[ChainID][]aaveMarket{
		Ethereum: {{Label: "Core", DataProvider: dataProvider}},
	})

	_, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err == nil || !strings.Contains(err.Error(), "zero aToken") {
		t.Fatalf("error = %v, want the missing aToken reported", err)
	}
}

// A Curve lending deposit sums the vault shares held directly and those staked in the gauge.
func TestCurveLendingDeclaresTheVaultAndGaugeShares(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 23_000_000, Fixed: true}
	market := curveLendingMarket{
		generation:      "one-way",
		vault:           common.HexToAddress("0x00000000000000000000000000000000000000f1"),
		controller:      common.HexToAddress("0x00000000000000000000000000000000000000f2"),
		collateralToken: walletTestWBTC,
		borrowedToken:   common.HexToAddress("0x00000000000000000000000000000000000000f3"),
		gauge:           common.HexToAddress("0x00000000000000000000000000000000000000f4"),
	}
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.balance(market.vault, big.NewInt(3_000))
	stub.answer(market.controller, curveControllerABI, "loan_exists", []any{holdsTestAccount}, false)
	stub.balance(market.gauge, big.NewInt(4_000))
	stub.answer(market.vault, curveVaultABI, "convertToAssets", []any{big.NewInt(7_000)}, big.NewInt(7))
	client := stub.dial()

	active, err := activeCurveLendingMarkets(
		context.Background(), client, block, holdsTestAccount, []curveLendingMarket{market},
	)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := curveLendingPrincipalGroups(
		context.Background(), client, block, holdsTestAccount, active, map[common.Address]Token{
			market.borrowedToken: {ChainID: Ethereum, Address: market.borrowedToken, Symbol: "crvUSD", Decimals: 18},
			market.collateralToken: {
				ChainID: Ethereum, Address: market.collateralToken, Symbol: "WBTC", Decimals: 8,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %+v, want the lent crvUSD", groups)
	}
	stub.assertHolds(groups, market.vault, market.gauge)
}

func TestLidoDeclaresTheEarnETHSharesItConverts(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 24_500_000, Fixed: true}
	oracle := common.HexToAddress("0x0000000000000000000000000000000000000c02")
	shares := big.NewInt(4_000_000_000_000_000_000)
	price := big.NewInt(800_000_000_000_000_000)
	wstETH := new(big.Int).Quo(new(big.Int).Mul(shares, big.NewInt(1_000_000_000_000_000_000)), price)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(lidoEarnETHAddress, lidoEarnShareManagerABI, "vault", nil, lidoEarnETHVaultAddress)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "shareManager", nil, lidoEarnETHAddress)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "oracle", nil, oracle)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "getAssetCount", nil, big.NewInt(1))
	stub.balance(lidoEarnETHAddress, shares)
	stub.answer(lidoEarnETHVaultAddress, lidoEarnVaultABI, "assetAt", []any{big.NewInt(0)}, lidoWstETHAddress)
	stub.answer(oracle, lidoEarnOracleABI, "getReport", []any{lidoWstETHAddress},
		lidoEarnReport{PriceD18: price, Timestamp: 1_752_000_000})
	stub.answer(lidoWstETHAddress, lidoWstETHABI, "getStETHByWstETH", []any{wstETH},
		big.NewInt(6_100_000_000_000_000_000))

	group, err := readLidoEarnETH(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if group == nil {
		t.Fatal("the earnETH position is missing")
	}
	stub.assertHolds([]Group{*group}, lidoEarnETHAddress)
}

// An Umbrella stake over a static aToken unwraps twice. The account holds the stake token; the
// wrapper in between is a balance of the stake token's, not of the account's.
func TestAaveUmbrellaDeclaresTheStakeTokenOfANestedVault(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 23_000_000, Fixed: true}
	rewards := common.HexToAddress("0x0000000000000000000000000000000000000c03")
	stakeToken := common.HexToAddress("0x00000000000000000000000000000000000000c4")
	wrapper := common.HexToAddress("0x00000000000000000000000000000000000000c5")
	shares := big.NewInt(1_000_000)
	wrapped := big.NewInt(1_010_000)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.balance(stakeToken, shares)
	stub.answer(rewards, aaveUmbrellaRewardsABI, "calculateCurrentUserRewards",
		[]any{stakeToken, holdsTestAccount}, []common.Address{}, []*big.Int{})
	stub.answer(stakeToken, erc4626ABI, "asset", nil, wrapper)
	stub.answer(stakeToken, erc4626ABI, "convertToAssets", []any{shares}, wrapped)
	stub.answer(wrapper, erc4626ABI, "asset", nil, walletTestUSDC)
	stub.answer(wrapper, erc4626ABI, "convertToAssets", []any{wrapped}, big.NewInt(1_030_000))
	stub.answer(walletTestUSDC, erc20ABI, "decimals", nil, uint8(6))
	stub.answer(walletTestUSDC, erc20ABI, "symbol", nil, "USDC")

	groups, err := readAaveUmbrellaPositions(
		context.Background(), stub.dial(), block, holdsTestAccount, aaveUmbrellaDeployment{
			RewardsController: rewards,
			StakeAssets: []aaveUmbrellaStakeAsset{{
				Label: "waUSDC", StakeToken: stakeToken, Underlying: wrapper, NestedERC4626: true,
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %+v, want the staked USDC", groups)
	}
	stub.assertHolds(groups, stakeToken)
}

// Both reserve legs of a Pendle liquidity position come out of the one LP balance.
func TestPendleLiquidityDeclaresTheLPTokenBehindEachLeg(t *testing.T) {
	fixture := newPendlePricingFixture(t)
	fixture.stub.balances[fixture.market] = big.NewInt(111_565)
	groups := fixture.run(t, &stubPendleIndexer{refs: []pendlePositionRef{{
		Token: fixture.market, Kind: pendleLP, PT: fixture.principal, CreatedBlock: 25_098_960,
	}}})
	if len(groups) != 1 || len(groups[0].Components) != 2 {
		t.Fatalf("groups = %#v, want one liquidity group with a SY and a PT leg", groups)
	}
	for _, component := range groups[0].Components {
		if held := heldContracts(component.Source); len(held) != 1 || held[0] != fixture.market {
			t.Errorf("%s leg declares %v, want the LP token %s", component.Token.Symbol, held, fixture.market)
		}
	}
	assertHeldExactly(t, fixture.block.ChainID, fixture.account, groups, fixture.market)
}

// Renzo's EigenLayer vaults are the ERC-20 of their shares; userUnderlying converts that balance.
func TestRenzoDeclaresTheEigenVaultSharesItConverts(t *testing.T) {
	// ezREZ and ezEIGEN exist at this block, ezBTC does not.
	block := BlockRef{ChainID: Ethereum, Number: 21_000_000, Fixed: true}
	adapter := newRenzoAdapter(SentioIndexerConfig{}).(*RenzoAdapter)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	var held common.Address
	for _, vault := range adapter.vaults {
		if block.Number < vault.ActivationBlock {
			continue
		}
		amount := new(big.Int)
		if vault.ID == "ezeigen" {
			amount = big.NewInt(2_500_000_000_000_000_000)
			held = vault.Address
		}
		stub.answer(vault.Address, renzoEigenVaultABI, "underlying", nil, vault.Underlying.Address)
		stub.answer(vault.Address, renzoEigenVaultABI, "userUnderlying", []any{holdsTestAccount}, amount)
	}

	groups, err := adapter.readEigenVaults(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "ezeigen" {
		t.Fatalf("groups = %+v, want the ezEIGEN position", groups)
	}
	stub.assertHolds(groups, held)
}

// A Fraxlend pair is also the ERC-20 of its lenders' shares; getUserSnapshot reads that balance.
func TestFraxlendDeclaresThePairSharesBehindASupply(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 20_000_000, Fixed: true}
	pair := common.HexToAddress("0x00000000000000000000000000000000000000f5")
	asset := common.HexToAddress("0x00000000000000000000000000000000000000f6")
	collateral := common.HexToAddress("0x00000000000000000000000000000000000000f7")
	shares := big.NewInt(1_000_000_000_000_000_000)
	zero := new(big.Int)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(fraxlendRegistry, fraxlendRegistryABI, "deployedPairsLength", nil, big.NewInt(1))
	stub.answer(fraxlendRegistry, fraxlendRegistryABI, "getAllPairAddresses", nil, []common.Address{pair})
	stub.answer(pair, fraxlendPairABI, "getUserSnapshot", []any{holdsTestAccount}, shares, zero, zero)
	stub.answer(pair, fraxlendPairABI, "asset", nil, asset)
	stub.answer(pair, fraxlendPairABI, "collateralContract", nil, collateral)
	stub.answer(pair, fraxlendPairABI, "symbol", nil, "fFRAX(sfrxETH)-5")
	for address, symbol := range map[common.Address]string{asset: "FRAX", collateral: "sfrxETH"} {
		stub.answer(address, erc20ABI, "decimals", nil, uint8(18))
		stub.answer(address, erc20ABI, "symbol", nil, symbol)
	}
	stub.answer(pair, fraxlendPairABI, "toAssetAmount", []any{shares, false, false},
		big.NewInt(1_040_000_000_000_000_000))
	stub.answer(pair, fraxlendPairABI, "toBorrowAmount", []any{zero, true, false}, zero)

	groups, err := newFraxlendAdapter().Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %+v, want the FRAX supply", groups)
	}
	stub.assertHolds(groups, pair)
}

// A StakeWise ERC-20 vault is the token of its shares; getShares reads the same balance. Vaults
// without a token declare their shares too, which no wallet lists.
func TestStakeWiseDeclaresTheVaultSharesItConverts(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 20_000_000, Fixed: true}
	vault := common.HexToAddress("0x00000000000000000000000000000000000000f8")
	shares := big.NewInt(3_000_000_000_000_000_000)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(vault, stakeWiseVaultABI, "getShares", []any{holdsTestAccount}, shares)
	stub.answer(vault, stakeWiseVaultABI, "convertToAssets", []any{shares}, big.NewInt(3_090_000_000_000_000_000))
	adapter := &StakeWiseAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "stakewise", Name: "StakeWise", Chains: []ChainID{Ethereum},
		}},
		vaults: []stakeWiseVault{{Address: vault, ActivationBlock: 1}},
	}

	groups, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want the staked ETH", groups)
	}
	stub.assertHolds(groups, vault)
}

// A VeYFI gauge is an ERC-4626 over vault shares whose own shares are an ERC-20 the account holds,
// so a deposit that sums direct and staked shares consumed both balances.
func TestYearnDeclaresTheVaultAndGaugeShares(t *testing.T) {
	vault, exists := yearnV3AdapterForTestVault(common.HexToAddress("0x028eC7330ff87667b6dfb0D94b954c820195336c"))
	if !exists || vault.Staking == nil || vault.Staking.Source != "VeYFI" {
		t.Fatalf("Yearn manifest vault = %+v, want one staked in a VeYFI gauge", vault)
	}
	block := BlockRef{ChainID: Ethereum, Number: vault.Staking.ActivationBlock, Fixed: true}
	gaugeShares := big.NewInt(2_000_000_000_000_000_000)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.balance(vault.Address, big.NewInt(1_000_000_000_000_000_000))
	stub.balance(vault.Staking.Address, gaugeShares)
	stub.answer(vault.Staking.Address, yearnV3ABI, "earned", []any{holdsTestAccount}, new(big.Int))
	stub.answer(vault.Staking.Address, yearnV3ABI, "asset", nil, vault.Address)
	stub.answer(vault.Staking.Address, yearnV3ABI, "convertToAssets", []any{gaugeShares}, gaugeShares)
	stub.answer(vault.Address, yearnV3ABI, "pricePerShare", nil, big.NewInt(1_050_000_000_000_000_000))
	stub.answer(vault.Address, yearnV3ABI, "decimals", nil, uint8(18))
	stub.answer(vault.Address, yearnV3ABI, "asset", nil, vault.Token.Address)
	adapter := &yearnV3Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "yearn-v3", Name: "Yearn V3", Chains: []ChainID{Ethereum},
		}},
		vaults:  map[ChainID][]yearnManifestVault{Ethereum: {vault}},
		byVault: map[ChainID]map[common.Address]yearnManifestVault{Ethereum: {vault.Address: vault}},
	}

	groups, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %+v, want one vault position", groups)
	}
	stub.assertHolds(groups, vault.Address, vault.Staking.Address)
}

// Locking VSP mints esVSP, the lock's escrow token: its wallet balance is the locked position.
func TestVesperDeclaresTheEscrowTokenOfLockedVSP(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 20_000_000, Fixed: true}
	pool := vesperManifestPool{
		ChainID: Ethereum, Name: "Test pool", Version: 4, ActivationBlock: 1,
		Address: common.HexToAddress("0x00000000000000000000000000000000000000f9"),
	}
	rewards := common.HexToAddress("0x00000000000000000000000000000000000000fa")
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.balance(pool.Address, new(big.Int))
	stub.answer(vesperLockedVSP, vesperLockedVSPABI, "locked", []any{holdsTestAccount},
		big.NewInt(5_000_000_000_000_000_000))
	stub.answer(vesperLockedVSP, vesperLockedVSPABI, "rewards", nil, rewards)
	stub.answer(rewards, vesperLockedRewardsABI, "claimableRewards", []any{holdsTestAccount},
		[]common.Address{}, []*big.Int{})
	adapter := &VesperAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "vesper", Name: "Vesper", Chains: []ChainID{Ethereum}}},
		pools:       map[ChainID][]vesperManifestPool{Ethereum: {pool}},
	}

	groups, err := adapter.Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Components) != 1 {
		t.Fatalf("groups = %+v, want the locked VSP", groups)
	}
	stub.assertHolds(groups, vesperLockedVSP)
}

// An EVault's debt is also the balance of its DToken, a read-only ERC-20 that wallets list, and
// rEUL keeps every vesting lock as the account's own rEUL balance.
func TestEulerDeclaresDebtTokensAndRewardLocks(t *testing.T) {
	chain := eulerV2ChainConfigs[Ethereum]
	block := BlockRef{ChainID: Ethereum, Number: chain.RewardEULBlock, Fixed: true}
	vault := common.HexToAddress("0x00000000000000000000000000000000000000fb")
	debtToken := common.HexToAddress("0x00000000000000000000000000000000000000fc")
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(vault, eulerVaultABI, "EVC", nil, chain.EVC)
	stub.answer(vault, eulerVaultABI, "asset", nil, walletTestUSDC)
	stub.balance(vault, new(big.Int))
	stub.answer(vault, eulerVaultABI, "debtOf", []any{holdsTestAccount}, big.NewInt(40_000_000))
	stub.answer(vault, eulerVaultABI, "balanceForwarderEnabled", []any{holdsTestAccount}, false)
	stub.answer(vault, eulerVaultABI, "dToken", nil, debtToken)
	stub.answer(walletTestUSDC, erc20ABI, "symbol", nil, "USDC")
	stub.answer(walletTestUSDC, erc20ABI, "decimals", nil, uint8(6))
	client := stub.dial()
	adapter := newEulerV2AdapterWithIndexer(nil)

	states, err := adapter.readStates(context.Background(), client, block, []eulerPositionRef{{
		Account: holdsTestAccount, Vault: vault, Kind: eulerEVault,
	}}, chain)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := adapter.buildGroups(
		context.Background(), client, block, holdsTestAccount, states, nil,
		[]eulerVestingState{{
			Timestamp: big.NewInt(1_750_000_000), Amount: big.NewInt(7),
			Claimable: big.NewInt(3), Remainder: big.NewInt(4),
		}},
		chain,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v, want the USDC debt and the vesting lock", groups)
	}
	stub.assertHolds(groups, debtToken, chain.RewardEUL)
}

// Liquity's Unipool keeps staked LP as its own internal balance, so the stake consumed no ERC-20
// of the account's. The inference from its source would claim the LP token and hide any unstaked
// LP sitting in the wallet.
func TestLiquityUnipoolLeavesTheWalletLPAlone(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: 20_000_000, Fixed: true}
	zero := new(big.Int)
	stub := newContractStub(t, Ethereum, holdsTestAccount)
	stub.answer(liquityTroveManager, liquityV1ABI, "getEntireDebtAndColl", []any{holdsTestAccount},
		zero, zero, zero, zero)
	for _, method := range []string{"getCompoundedLUSDDeposit", "getDepositorETHGain", "getDepositorLQTYGain"} {
		stub.answer(liquityStabilityPool, liquityV1ABI, method, []any{holdsTestAccount}, zero)
	}
	for _, method := range []string{"stakes", "getPendingETHGain", "getPendingLUSDGain"} {
		stub.answer(liquityLQTYStaking, liquityV1ABI, method, []any{holdsTestAccount}, zero)
	}
	stub.balance(liquityUniPool, big.NewInt(1_000_000_000_000_000_000))
	stub.answer(liquityUniPool, liquityV1ABI, "earned", []any{holdsTestAccount}, zero)
	stub.answer(liquityPair, liquityV1PairABI, "token0", nil, liquityLUSD.Address)
	stub.answer(liquityPair, liquityV1PairABI, "token1", nil, liquityETH.Address)
	stub.answer(liquityPair, liquityV1PairABI, "getReserves", nil,
		big.NewInt(3_000_000_000_000_000_000), big.NewInt(1_000_000_000_000_000), uint32(0))
	stub.answer(liquityPair, liquityV1PairABI, "totalSupply", nil, new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil))

	groups, err := newLiquityV1Adapter().Positions(context.Background(), stub.dial(), block, holdsTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].ID != "unipool" {
		t.Fatalf("groups = %+v, want the Unipool stake", groups)
	}
	assertHeldExactly(t, Ethereum, holdsTestAccount, groups)
}
