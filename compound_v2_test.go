package portfolio

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type compoundDistributorModuleFixture struct {
	stakedAmount      *big.Int
	pendingWithdrawal *big.Int
	releaseTime       *big.Int
	stakedToken       common.Address
	rewardTokens      []common.Address
	rewardAmounts     map[common.Address]*big.Int
	claimFailures     map[common.Address]bool
	failPrincipal     bool
}

type compoundDistributorRPCFixture struct {
	t            *testing.T
	comptroller  common.Address
	modules      map[common.Address]compoundDistributorModuleFixture
	tokens       map[common.Address]Token
	mu           sync.Mutex
	moduleCalls  int
	marketCalls  int
	tokenProbes  int
	metadataRead int
}

func (s *compoundDistributorRPCFixture) pack(method string, values ...any) string {
	s.t.Helper()
	var outputs = compoundDistributorStakingABI.Methods[method].Outputs
	if method == "decimals" || method == "symbol" {
		outputs = erc20ABI.Methods[method].Outputs
	}
	encoded, err := outputs.Pack(values...)
	if err != nil {
		s.t.Fatalf("pack %s output: %v", method, err)
	}
	return hexutil.Encode(encoded)
}

func (s *compoundDistributorRPCFixture) answer(call rpcTestRequest) map[string]any {
	s.t.Helper()
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	if call.Method == "eth_chainId" {
		response["result"] = "0xa"
		return response
	}
	if call.Method != "eth_call" || len(call.Params) == 0 {
		response["error"] = map[string]any{"code": -32601, "message": "unexpected RPC call"}
		return response
	}
	var input struct {
		To   common.Address `json:"to"`
		Data hexutil.Bytes  `json:"data"`
	}
	if err := json.Unmarshal(call.Params[0], &input); err != nil {
		s.t.Errorf("decode eth_call: %v", err)
		response["error"] = map[string]any{"code": -32602, "message": "invalid eth_call"}
		return response
	}
	if len(input.Data) < 4 {
		response["error"] = map[string]any{"code": -32602, "message": "missing selector"}
		return response
	}
	if input.To == s.comptroller {
		s.mu.Lock()
		s.marketCalls++
		s.mu.Unlock()
		response["error"] = map[string]any{
			"code": 3, "message": "comptroller must not be called before its window",
		}
		return response
	}
	if module, exists := s.modules[input.To]; exists {
		s.mu.Lock()
		s.moduleCalls++
		s.mu.Unlock()
		selector := string(input.Data[:4])
		if module.failPrincipal && (selector == string(compoundDistributorStakingABI.Methods["balanceOf"].ID) ||
			selector == string(compoundDistributorStakingABI.Methods["withdrawal"].ID)) {
			response["error"] = map[string]any{"code": 3, "message": "stale module"}
			return response
		}
		switch selector {
		case string(compoundDistributorStakingABI.Methods["balanceOf"].ID):
			response["result"] = s.pack("balanceOf", module.stakedAmount)
		case string(compoundDistributorStakingABI.Methods["withdrawal"].ID):
			response["result"] = s.pack(
				"withdrawal",
				module.pendingWithdrawal,
				module.releaseTime,
			)
		case string(compoundDistributorStakingABI.Methods["sonne"].ID):
			response["result"] = s.pack("sonne", module.stakedToken)
		case string(compoundDistributorStakingABI.Methods["tokens"].ID):
			s.mu.Lock()
			s.tokenProbes++
			s.mu.Unlock()
			arguments, err := compoundDistributorStakingABI.Methods["tokens"].Inputs.Unpack(input.Data[4:])
			if err != nil {
				s.t.Errorf("decode tokens input: %v", err)
				response["error"] = map[string]any{"code": -32602, "message": "invalid index"}
				break
			}
			index, ok := arguments[0].(*big.Int)
			if !ok || !index.IsUint64() || index.Uint64() >= uint64(len(module.rewardTokens)) {
				response["error"] = map[string]any{"code": 3, "message": "array out of bounds"}
				break
			}
			response["result"] = s.pack("tokens", module.rewardTokens[index.Uint64()])
		case string(compoundDistributorStakingABI.Methods["getClaimable"].ID):
			arguments, err := compoundDistributorStakingABI.Methods["getClaimable"].Inputs.Unpack(input.Data[4:])
			if err != nil {
				s.t.Errorf("decode getClaimable input: %v", err)
				response["error"] = map[string]any{"code": -32602, "message": "invalid reward query"}
				break
			}
			rewardToken, ok := arguments[0].(common.Address)
			if !ok {
				s.t.Errorf("getClaimable token type = %T", arguments[0])
				response["error"] = map[string]any{"code": -32602, "message": "invalid reward token"}
				break
			}
			if module.claimFailures[rewardToken] {
				response["error"] = map[string]any{"code": 3, "message": "reward call reverted"}
				break
			}
			amount := module.rewardAmounts[rewardToken]
			if amount == nil {
				amount = new(big.Int)
			}
			response["result"] = s.pack("getClaimable", amount)
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected module selector"}
		}
		return response
	}
	if token, exists := s.tokens[input.To]; exists {
		s.mu.Lock()
		s.metadataRead++
		s.mu.Unlock()
		switch string(input.Data[:4]) {
		case string(erc20ABI.Methods["decimals"].ID):
			response["result"] = s.pack("decimals", token.Decimals)
		case string(erc20ABI.Methods["symbol"].ID):
			response["result"] = s.pack("symbol", token.Symbol)
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected token selector"}
		}
		return response
	}
	response["error"] = map[string]any{"code": -32601, "message": "unexpected contract"}
	return response
}

func (s *compoundDistributorRPCFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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
			s.t.Errorf("decode singleton RPC call: %v", err)
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(s.answer(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode batch RPC calls: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = s.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func TestCompoundDistributorStakingIncludesPendingWithdrawalAndRewards(t *testing.T) {
	moduleAddress := common.HexToAddress("0x0000000000000000000000000000000000000100")
	comptroller := common.HexToAddress("0x0000000000000000000000000000000000000200")
	sonne := common.HexToAddress("0x0000000000000000000000000000000000000300")
	reward := common.HexToAddress("0x0000000000000000000000000000000000000400")
	rewardV2 := common.HexToAddress("0x0000000000000000000000000000000000000401")
	fixture := &compoundDistributorRPCFixture{
		t:           t,
		comptroller: comptroller,
		modules: map[common.Address]compoundDistributorModuleFixture{
			moduleAddress: {
				stakedAmount:      big.NewInt(100),
				pendingWithdrawal: big.NewInt(25),
				releaseTime:       big.NewInt(1_700_000_000),
				stakedToken:       sonne,
				rewardTokens:      []common.Address{{}, sonne, reward, rewardV2},
				rewardAmounts: map[common.Address]*big.Int{
					sonne:    big.NewInt(7),
					reward:   big.NewInt(11),
					rewardV2: big.NewInt(13),
				},
			},
		},
		tokens: map[common.Address]Token{
			sonne:    {ChainID: Optimism, Address: sonne, Symbol: "SONNE", Decimals: 18},
			reward:   {ChainID: Optimism, Address: reward, Symbol: "VELO", Decimals: 18},
			rewardV2: {ChainID: Optimism, Address: rewardV2, Symbol: "VELO", Decimals: 18},
		},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Optimism, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	adapter := &CompoundV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "sonne", Name: "Sonne", Chains: []ChainID{Optimism}}},
		deployments: map[ChainID]compoundV2Deployment{
			Optimism: {
				Comptroller:       comptroller,
				ComptrollerWindow: availableFrom(200),
				DistributorStaking: []compoundDistributorStakingModule{
					{Module: moduleAddress, Window: availableFrom(100)},
				},
			},
		},
	}
	groups, err := adapter.Positions(
		context.Background(),
		client,
		BlockRef{ChainID: Optimism, Number: 100},
		common.HexToAddress("0x0000000000000000000000000000000000000500"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want one staking group", groups)
	}
	if got, want := len(groups[0].Components), 4; got != want {
		t.Fatalf("components = %+v, want %d", groups[0].Components, want)
	}
	asset := groups[0].Components[0]
	if asset.Kind != "asset" || asset.Token.Address != sonne || asset.AmountRaw != "125" {
		t.Fatalf("principal = %+v, want 125 SONNE", asset)
	}
	if got := asset.Metadata["stakedAmountRaw"]; got != "100" {
		t.Fatalf("staked metadata = %v, want 100", got)
	}
	if got := asset.Metadata["pendingWithdrawalAmountRaw"]; got != "25" {
		t.Fatalf("pending withdrawal metadata = %v, want 25", got)
	}
	if got := asset.Metadata["withdrawalReleaseTime"]; got != "1700000000" {
		t.Fatalf("release metadata = %v, want 1700000000", got)
	}
	if got := groups[0].Components[1].AmountRaw; got != "7" {
		t.Fatalf("SONNE reward = %s, want 7", got)
	}
	if component := groups[0].Components[2]; component.Token.Address != reward ||
		component.Token.Symbol != "VELO" || component.AmountRaw != "11" {
		t.Fatalf("first VELO reward = %+v, want 11", component)
	}
	if component := groups[0].Components[3]; component.Token.Address != rewardV2 ||
		component.Token.Symbol != "VELO" || component.AmountRaw != "13" {
		t.Fatalf("second VELO reward = %+v, want 13", component)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.marketCalls != 0 {
		t.Fatalf("pre-window comptroller calls = %d, want zero", fixture.marketCalls)
	}
	if fixture.tokenProbes > compoundDistributorRewardProbeSize {
		t.Fatalf("reward-token probes = %d, want at most %d", fixture.tokenProbes, compoundDistributorRewardProbeSize)
	}
	if fixture.metadataRead != 6 {
		t.Fatalf("metadata calls = %d, want 6 (cached SONNE plus two VELO addresses)", fixture.metadataRead)
	}
}

func TestCompoundDistributorStakingIsolatesStaleModuleAndBadRewards(t *testing.T) {
	staleModule := common.HexToAddress("0x0000000000000000000000000000000000000100")
	healthyModule := common.HexToAddress("0x0000000000000000000000000000000000000101")
	sonne := common.HexToAddress("0x0000000000000000000000000000000000000300")
	revertingReward := common.HexToAddress("0x0000000000000000000000000000000000000400")
	badMetadataReward := common.HexToAddress("0x0000000000000000000000000000000000000500")
	healthyReward := common.HexToAddress("0x0000000000000000000000000000000000000600")
	fixture := &compoundDistributorRPCFixture{
		t: t,
		modules: map[common.Address]compoundDistributorModuleFixture{
			staleModule: {failPrincipal: true},
			healthyModule: {
				stakedAmount:      big.NewInt(50),
				pendingWithdrawal: new(big.Int),
				releaseTime:       new(big.Int),
				stakedToken:       sonne,
				rewardTokens: []common.Address{
					{}, revertingReward, badMetadataReward, healthyReward,
				},
				rewardAmounts: map[common.Address]*big.Int{
					revertingReward:   big.NewInt(9),
					badMetadataReward: big.NewInt(10),
					healthyReward:     big.NewInt(11),
				},
				claimFailures: map[common.Address]bool{revertingReward: true},
			},
		},
		tokens: map[common.Address]Token{
			sonne:         {ChainID: Optimism, Address: sonne, Symbol: "SONNE", Decimals: 18},
			healthyReward: {ChainID: Optimism, Address: healthyReward, Symbol: "OP", Decimals: 18},
		},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Optimism, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	adapter := &CompoundV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "sonne", Name: "Sonne", Chains: []ChainID{Optimism}}},
		deployments: map[ChainID]compoundV2Deployment{
			Optimism: {
				ComptrollerWindow: availableFrom(200),
				DistributorStaking: []compoundDistributorStakingModule{
					{Module: staleModule, Window: availableFrom(1)},
					{Module: healthyModule, Window: availableFrom(1)},
				},
			},
		},
	}
	groups, err := adapter.Positions(
		context.Background(),
		client,
		BlockRef{ChainID: Optimism, Number: 100},
		common.HexToAddress("0x0000000000000000000000000000000000000700"),
	)
	if err == nil || !strings.Contains(err.Error(), "stale module") {
		t.Fatalf("partial module error = %v, want stale-module error", err)
	}
	if len(groups) != 1 {
		t.Fatalf("isolated groups = %+v, want only healthy module", groups)
	}
	if got, want := groups[0].ID, "staking:0x0000000000000000000000000000000000000101"; got != want {
		t.Fatalf("healthy group ID = %q, want %q", got, want)
	}
	if got, want := len(groups[0].Components), 2; got != want {
		t.Fatalf("healthy components = %+v, want principal and one reward", groups[0].Components)
	}
	if component := groups[0].Components[0]; component.Kind != "asset" || component.AmountRaw != "50" {
		t.Fatalf("principal = %+v, want 50", component)
	}
	if component := groups[0].Components[1]; component.Kind != "reward" ||
		component.Token.Address != healthyReward || component.AmountRaw != "11" {
		t.Fatalf("isolated reward = %+v, want 11 healthy reward", component)
	}
}

func TestCompoundDistributorRewardTokenSafetyBoundCapsList(t *testing.T) {
	moduleAddress := common.HexToAddress("0x0000000000000000000000000000000000000100")
	rewardTokens := make([]common.Address, compoundDistributorMaxRewards+2)
	for index := 1; index < len(rewardTokens); index++ {
		rewardTokens[index] = common.BigToAddress(big.NewInt(int64(index)))
	}
	fixture := &compoundDistributorRPCFixture{
		t: t,
		modules: map[common.Address]compoundDistributorModuleFixture{
			moduleAddress: {rewardTokens: rewardTokens},
		},
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	client, err := DialRPC(context.Background(), Optimism, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	tokens, err := compoundDistributorRewardTokens(
		context.Background(),
		client,
		BlockRef{ChainID: Optimism, Number: 1},
		moduleAddress,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(tokens), compoundDistributorMaxRewards; got != want {
		t.Fatalf("capped reward list length = %d, want %d", got, want)
	}
}

func TestCompoundV2SupplyComponentPreservesSubUnitUnderlyingAmount(t *testing.T) {
	market := common.HexToAddress("0x0000000000000000000000000000000000000100")
	underlying := Token{
		ChainID:  Optimism,
		Address:  common.HexToAddress("0x0000000000000000000000000000000000000200"),
		Symbol:   "USDC",
		Decimals: 6,
	}
	// 0.52636 of the smallest underlying unit used to floor to zero.
	component := compoundV2SupplyComponent(underlying, big.NewInt(526_360_000_000_000_000), market)
	if got, want := component.AmountRaw, "526360000000000000"; got != want {
		t.Fatalf("supply numerator = %s, want %s", got, want)
	}
	if got, want := component.AmountDenominatorRaw, "1000000000000000000"; got != want {
		t.Fatalf("supply denominator = %s, want %s", got, want)
	}
	amount := new(big.Rat)
	if _, ok := amount.SetString(component.AmountRaw + "/" + component.AmountDenominatorRaw); !ok {
		t.Fatal("component amount is not a valid rational")
	}
	if amount.Sign() <= 0 || amount.Cmp(new(big.Rat).SetFrac64(1, 1)) >= 0 {
		t.Fatalf("sub-unit supply = %s, want 0 < amount < 1", amount.RatString())
	}
}

func TestSonneOptimismStakingParityLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_SONNE_STAKING_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_SONNE_STAKING_LIVE_TEST=1 to run pinned Sonne staking parity")
	}
	endpoint := os.Getenv("PORTFOLIO_OPTIMISM_RPC_URL")
	if endpoint == "" {
		t.Fatal("PORTFOLIO_OPTIMISM_RPC_URL is required")
	}
	client, err := DialRPC(context.Background(), Optimism, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	var sonne *CompoundV2Adapter
	for _, candidate := range compoundV2Adapters() {
		if candidate.Info().ID == "sonne" {
			sonne = candidate.(*CompoundV2Adapter)
			break
		}
	}
	if sonne == nil {
		t.Fatal("Sonne adapter is missing")
	}
	deployment := sonne.deployments[Optimism]
	// Exact DeBank capture at Optimism block 156472614. The account holds both
	// official wrappers; zero-valued retired rewards are intentionally omitted.
	groups, err := compoundDistributorStakingGroups(
		context.Background(),
		client,
		BlockRef{ChainID: Optimism, Number: 156_472_614},
		deployment.DistributorStaking,
		common.HexToAddress("0x32e3df7bc12770fd56cf984ecfa50e4892ee00a3"),
	)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]map[string]string)
	gotDecimals := make(map[string]map[string]uint8)
	for _, group := range groups {
		components := make(map[string]string)
		decimals := make(map[string]uint8)
		for _, component := range group.Components {
			key := component.Kind + ":" + strings.ToLower(component.Token.Address.Hex())
			components[key] = component.AmountRaw
			decimals[key] = component.Token.Decimals
		}
		groupID := strings.ToLower(group.ID)
		got[groupID] = components
		gotDecimals[groupID] = decimals
	}
	want := map[string]map[string]string{
		"staking:0xdc05d85069dc4aba65954008ff99f2d73ff12618": {
			"asset:0x1db2466d9f5e10d7090e7152b68d62703a2245f0":  "180680012532556753503",
			"reward:0x1db2466d9f5e10d7090e7152b68d62703a2245f0": "5644223748145173489",
			"reward:0x4200000000000000000000000000000000000042": "95975471556609730",
			"reward:0x9560e827af36c94d2ac33a39bce1fe78631088db": "1450002560016631074",
		},
		"staking:0x41279e29586eb20f9a4f65e031af09fced171166": {
			"asset:0x1db2466d9f5e10d7090e7152b68d62703a2245f0":  "1045756329672889495668",
			"reward:0x7f5c764cbc14f9669b88837ca1490cca17c31607": "5514390",
			"reward:0x4200000000000000000000000000000000000042": "628264929279813211",
			"reward:0x9560e827af36c94d2ac33a39bce1fe78631088db": "18725708883430400270",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("staking groups = %+v, want %+v", got, want)
	}
	for groupID, expectedComponents := range want {
		actualComponents, exists := got[groupID]
		if !exists {
			t.Errorf("missing group %s", groupID)
			continue
		}
		if len(actualComponents) != len(expectedComponents) {
			t.Errorf("group %s components = %+v, want %+v", groupID, actualComponents, expectedComponents)
			continue
		}
		for key, amount := range expectedComponents {
			if actualComponents[key] != amount {
				t.Errorf("group %s %s = %s, want %s", groupID, key, actualComponents[key], amount)
			}
		}
	}
	debankWant := map[string]map[string]string{
		"staking:0xdc05d85069dc4aba65954008ff99f2d73ff12618": {
			"asset:0x1db2466d9f5e10d7090e7152b68d62703a2245f0":  "180.68001253255676",
			"reward:0x1db2466d9f5e10d7090e7152b68d62703a2245f0": "5.644223748145174",
			"reward:0x4200000000000000000000000000000000000042": "0.09597547155660972",
			"reward:0x9560e827af36c94d2ac33a39bce1fe78631088db": "1.450002560016631",
		},
		"staking:0x41279e29586eb20f9a4f65e031af09fced171166": {
			"asset:0x1db2466d9f5e10d7090e7152b68d62703a2245f0":  "1045.7563296728895",
			"reward:0x7f5c764cbc14f9669b88837ca1490cca17c31607": "5.51439",
			"reward:0x4200000000000000000000000000000000000042": "0.6282649292798133",
			"reward:0x9560e827af36c94d2ac33a39bce1fe78631088db": "18.7257088834304",
		},
	}
	tolerance := new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(1_000_000_000_000))
	for groupID, expectedComponents := range debankWant {
		for key, expectedText := range expectedComponents {
			raw, ok := new(big.Int).SetString(got[groupID][key], 10)
			if !ok {
				t.Fatalf("invalid raw amount %q for %s %s", got[groupID][key], groupID, key)
			}
			scale := new(big.Int).Exp(
				big.NewInt(10),
				big.NewInt(int64(gotDecimals[groupID][key])),
				nil,
			)
			actual := new(big.Rat).SetFrac(raw, scale)
			expected, ok := new(big.Rat).SetString(expectedText)
			if !ok {
				t.Fatalf("invalid DeBank amount %q", expectedText)
			}
			delta := new(big.Rat).Sub(actual, expected)
			if delta.Sign() < 0 {
				delta.Neg(delta)
			}
			if delta.Cmp(tolerance) > 0 {
				t.Errorf(
					"%s %s amount = %s, DeBank = %s, delta %s exceeds %s",
					groupID,
					key,
					actual.FloatString(18),
					expectedText,
					delta.FloatString(18),
					tolerance.FloatString(18),
				)
			}
		}
	}
}

func TestSonneOptimismComponentBoundariesLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_SONNE_STAKING_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_SONNE_STAKING_LIVE_TEST=1 to run Sonne component boundary probes")
	}
	endpoint := os.Getenv("PORTFOLIO_OPTIMISM_RPC_URL")
	if endpoint == "" {
		t.Fatal("PORTFOLIO_OPTIMISM_RPC_URL is required")
	}
	client, err := DialRPC(context.Background(), Optimism, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	var sonne *CompoundV2Adapter
	for _, candidate := range compoundV2Adapters() {
		if candidate.Info().ID == "sonne" {
			sonne = candidate.(*CompoundV2Adapter)
			break
		}
	}
	if sonne == nil {
		t.Fatal("Sonne adapter is missing")
	}
	deployment := sonne.deployments[Optimism]
	tests := []struct {
		name               string
		address            common.Address
		block              uint64
		noCodeBeforeWindow bool
		probe              func(BlockRef) error
	}{
		{
			name:               "sSONNE",
			address:            deployment.DistributorStaking[0].Module,
			block:              25_840_175,
			noCodeBeforeWindow: true,
			probe: func(block BlockRef) error {
				_, callErr := client.Call(
					context.Background(),
					block,
					deployment.DistributorStaking[0].Module,
					compoundDistributorStakingABI,
					"sonne",
				)
				return callErr
			},
		},
		{
			name:               "uSONNE",
			address:            deployment.DistributorStaking[1].Module,
			block:              25_840_274,
			noCodeBeforeWindow: true,
			probe: func(block BlockRef) error {
				_, callErr := client.Call(
					context.Background(),
					block,
					deployment.DistributorStaking[1].Module,
					compoundDistributorStakingABI,
					"sonne",
				)
				return callErr
			},
		},
		{
			name:    "comptroller",
			address: deployment.Comptroller,
			block:   26_050_163,
			probe: func(block BlockRef) error {
				_, callErr := client.Call(
					context.Background(),
					block,
					deployment.Comptroller,
					comptrollerABI,
					"getAllMarkets",
				)
				return callErr
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var beforeCode, atCode hexutil.Bytes
			if err := client.call(
				context.Background(),
				&beforeCode,
				"eth_getCode",
				test.address,
				hexutil.EncodeUint64(test.block-1),
			); err != nil {
				t.Fatal(err)
			}
			if err := client.call(
				context.Background(),
				&atCode,
				"eth_getCode",
				test.address,
				hexutil.EncodeUint64(test.block),
			); err != nil {
				t.Fatal(err)
			}
			if test.noCodeBeforeWindow && len(beforeCode) != 0 {
				t.Fatalf("code exists before activation block %d", test.block)
			}
			if len(atCode) == 0 {
				t.Fatalf("code is missing at activation block %d", test.block)
			}
			if err := test.probe(BlockRef{ChainID: Optimism, Number: test.block}); err != nil {
				t.Fatalf("activation view failed at block %d: %v", test.block, err)
			}
			if err := test.probe(BlockRef{ChainID: Optimism, Number: test.block - 1}); err == nil {
				t.Fatalf("view unexpectedly succeeded before activation block %d", test.block)
			}
		})
	}
}
