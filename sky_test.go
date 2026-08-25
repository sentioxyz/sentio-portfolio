package portfolio

import (
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func skyAdapterForTest(t *testing.T) *SkyAdapter {
	t.Helper()
	adapter, ok := newSkyAdapter().(*SkyAdapter)
	if !ok {
		t.Fatal("newSkyAdapter did not return a *SkyAdapter")
	}
	return adapter
}

// TestSkyHistoricalVaultActivation pins the eth_getCode binary-search anchors for Sky's two
// savings vaults. sUSDS and stUSDS launched almost a year apart, so a shared activation block
// would have resurrected the Aave v4 defect: calls against stUSDS before it existed return
// empty data and drop Sky's whole surface for every fixed-block scan in the gap.
func TestSkyHistoricalVaultActivation(t *testing.T) {
	for _, test := range []struct {
		block uint64
		ids   []string
	}{
		{block: 20_677_433},
		{block: 20_677_434, ids: []string{"susds"}},
		{block: 23_219_534, ids: []string{"susds"}},
		{block: 23_219_535, ids: []string{"susds", "stusds"}},
	} {
		active := activeVaultsAt(skySavingsVaults, test.block)
		if len(active) != len(test.ids) {
			t.Fatalf(
				"active vault count at block %d = %d, want %d",
				test.block,
				len(active),
				len(test.ids),
			)
		}
		for index, want := range test.ids {
			if got := active[index].ID; got != want {
				t.Fatalf(
					"active vault %d at block %d = %q, want %q",
					index,
					test.block,
					got,
					want,
				)
			}
		}
	}
}

// TestSkyLockstakeDeploymentBoundary covers the engine's own window. The engine is the only
// hard-coded lockstake address — urns and farms are enumerated from it at the pinned block —
// so this boundary is the whole gate for that surface.
func TestSkyLockstakeDeploymentBoundary(t *testing.T) {
	window := skyLockstakeDeployment.Window
	if window.ActiveAt(window.ActivationBlock - 1) {
		t.Errorf("lockstake engine is active before deployment block %d", window.ActivationBlock)
	}
	if !window.ActiveAt(window.ActivationBlock) {
		t.Errorf("lockstake engine is inactive at deployment block %d", window.ActivationBlock)
	}
	if got, want := window.ActivationBlock, uint64(22_370_185); got != want {
		t.Errorf("lockstake activation block = %d, want %d", got, want)
	}
	if got, want := makerIlkName(skyLockstakeDeployment.Ilk), "LSEV2-SKY-A"; got != want {
		t.Errorf("lockstake ilk = %q, want %q", got, want)
	}
}

// TestSkyLockstakeSkippedBeforeDeployment proves the window short-circuits before any call is
// attempted: a nil client would panic if the gate let the read through.
func TestSkyLockstakeSkippedBeforeDeployment(t *testing.T) {
	block := BlockRef{ChainID: Ethereum, Number: skyLockstakeDeployment.Window.ActivationBlock - 1}
	groups, err := skyLockstakeGroups(t.Context(), nil, block, common.HexToAddress("0x1234"))
	if err != nil {
		t.Fatalf("pre-deployment lockstake read returned an error: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("pre-deployment lockstake read returned %d groups", len(groups))
	}
}

func TestSkyIsRegisteredOnEthereumOnly(t *testing.T) {
	engine := NewEngine(nil, nil)
	seen := 0
	for _, protocol := range engine.Protocols() {
		if protocol.ID != "sky" {
			continue
		}
		seen++
		if len(protocol.Chains) != 1 || protocol.Chains[0] != Ethereum {
			t.Fatalf("sky chains = %v, want [Ethereum]", protocol.Chains)
		}
		if protocol.Name != "Sky" {
			t.Fatalf("sky name = %q, want %q", protocol.Name, "Sky")
		}
	}
	if seen != 1 {
		t.Fatalf("sky is registered %d times, want exactly 1", seen)
	}
}

// TestSkyVaultsShareTheUSDSAsset keeps both vaults pointed at the USDS address the adapter
// asserts against at runtime; readVaultPositions fails the scan when asset() disagrees.
func TestSkyVaultsShareTheUSDSAsset(t *testing.T) {
	usds := common.HexToAddress("0xdC035D45d973E3EC169d2276DDab16f1e407384F")
	if len(skySavingsVaults) != 2 {
		t.Fatalf("sky vault count = %d, want 2", len(skySavingsVaults))
	}
	for _, vault := range skySavingsVaults {
		if vault.Asset.Address != usds {
			t.Errorf("%s asset = %s, want USDS %s", vault.ID, vault.Asset.Address, usds)
		}
		if vault.Asset.Decimals != 18 {
			t.Errorf("%s asset decimals = %d, want 18", vault.ID, vault.Asset.Decimals)
		}
		if vault.CooldownID != "" {
			t.Errorf(
				"%s declares cooldown %q; Sky vaults redeem synchronously",
				vault.ID,
				vault.CooldownID,
			)
		}
		if vault.ActivationBlock == 0 {
			t.Errorf("%s has no activation block", vault.ID)
		}
	}
}

// TestSkyOtherChainsReturnNothing guards the deliberate chain scope: the Base and Arbitrum
// sUSDS tokens are bridged ERC-20 representations with no asset() or convertToAssets(), so
// the vault read would fail against them rather than value them. A nil client proves no call
// is attempted.
func TestSkyOtherChainsReturnNothing(t *testing.T) {
	adapter := skyAdapterForTest(t)
	for _, chainID := range []ChainID{BSC, Base, Arbitrum} {
		groups, err := adapter.Positions(
			t.Context(),
			nil,
			BlockRef{ChainID: chainID, Number: 30_000_000},
			common.HexToAddress("0x1234"),
		)
		if err != nil {
			t.Fatalf("chain %d returned an error: %v", chainID, err)
		}
		if len(groups) != 0 {
			t.Fatalf("chain %d returned %d groups, want 0", chainID, len(groups))
		}
	}
}

// TestSkyLockstakeDebtMath pins the Vat convention the lockstake read shares with makerdao.go:
// the stored art is normalised debt and only art*rate/RAY is the USDS actually owed.
func TestSkyLockstakeDebtMath(t *testing.T) {
	ray, _ := new(big.Int).SetString("1000000000000000000000000000", 10)
	art, _ := new(big.Int).SetString("1000000000000000000000", 10) // 1,000 normalised
	rate := new(big.Int).Add(ray, new(big.Int).Div(ray, big.NewInt(10)))
	want, _ := new(big.Int).SetString("1100000000000000000000", 10) // 1,100 USDS owed
	if got := makerDebtRaw(art, rate); got.Cmp(want) != 0 {
		t.Fatalf("debt = %s, want %s", got, want)
	}
	if got := makerDebtRaw(big.NewInt(0), rate); got.Sign() != 0 {
		t.Fatalf("zero art produced debt %s", got)
	}
}

// TestSkyFarmDeploymentBoundaries covers the per-farm windows. The two farms launched 14,766
// blocks apart, so a shared anchor would repeat the Aave v4 defect on this surface too.
func TestSkyFarmDeploymentBoundaries(t *testing.T) {
	want := map[string]uint64{
		"farm:usds-sky": 20_692_595,
		"farm:usds-01":  20_677_829,
	}
	if len(skyUSDSFarms) != len(want) {
		t.Fatalf("farm count = %d, want %d", len(skyUSDSFarms), len(want))
	}
	for _, farm := range skyUSDSFarms {
		expected, known := want[farm.ID]
		if !known {
			t.Errorf("unexpected farm %q", farm.ID)
			continue
		}
		if farm.Window.ActivationBlock != expected {
			t.Errorf("%s activation = %d, want %d", farm.ID, farm.Window.ActivationBlock, expected)
		}
		if farm.Window.ActiveAt(expected - 1) {
			t.Errorf("%s is active before its deployment block %d", farm.ID, expected)
		}
		if !farm.Window.ActiveAt(expected) {
			t.Errorf("%s is inactive at its deployment block %d", farm.ID, expected)
		}
	}
}

// TestSkyExcludesFarmsOwnedByOtherProjects pins the attribution decision documented on
// skyUSDSFarms. The SPK farm belongs to DeBank's `spark` project and the GROVE farm to
// `makerdao`; reading either here would double-count and over-report against `sky`.
func TestSkyExcludesFarmsOwnedByOtherProjects(t *testing.T) {
	for _, excluded := range []struct {
		address common.Address
		project string
	}{
		{common.HexToAddress("0x173e314C7635B45322cd8Cb14f44b312e079F3af"), "spark"},
		{common.HexToAddress("0x4E41488C19cD35EB4de3083Fc3e204854c75c86a"), "makerdao"},
	} {
		for _, farm := range skyUSDSFarms {
			if farm.Address == excluded.address {
				t.Errorf(
					"farm %s belongs to DeBank project %q and must not be read as sky",
					excluded.address,
					excluded.project,
				)
			}
		}
	}
}

type skyStubRPC struct {
	t          *testing.T
	failVaults bool
	urn        common.Address
	ink        *big.Int
	art        *big.Int
	rate       *big.Int
}

type skyStubCall struct {
	ID     any               `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (s *skyStubRPC) dispatch(to common.Address, data []byte) (string, map[string]any) {
	fail := map[string]any{"code": 3, "message": "execution reverted"}
	if len(data) < 4 {
		return "", fail
	}
	selector := string(data[:4])
	switch to {
	case skySavingsVaults[0].Address, skySavingsVaults[1].Address:
		if s.failVaults {
			return "", fail
		}
		switch selector {
		case string(erc4626ABI.Methods["asset"].ID):
			out, _ := erc4626ABI.Methods["asset"].Outputs.Pack(skyUSDS.Address)
			return "0x" + common.Bytes2Hex(out), nil
		case string(erc4626ABI.Methods["balanceOf"].ID):
			out, _ := erc4626ABI.Methods["balanceOf"].Outputs.Pack(big.NewInt(0))
			return "0x" + common.Bytes2Hex(out), nil
		}
		return "", fail
	case skyUSDSFarms[0].Address, skyUSDSFarms[1].Address:
		switch selector {
		case string(skyFarmABI.Methods["stakingToken"].ID):
			out, _ := skyFarmABI.Methods["stakingToken"].Outputs.Pack(skyUSDS.Address)
			return "0x" + common.Bytes2Hex(out), nil
		case string(skyFarmABI.Methods["balanceOf"].ID),
			string(skyFarmABI.Methods["earned"].ID):
			out, _ := skyFarmABI.Methods["balanceOf"].Outputs.Pack(big.NewInt(0))
			return "0x" + common.Bytes2Hex(out), nil
		}
		return "", fail
	case skyLockstakeDeployment.Engine:
		switch selector {
		case string(skyLockstakeEngineABI.Methods["ownerUrnsCount"].ID):
			out, _ := skyLockstakeEngineABI.Methods["ownerUrnsCount"].Outputs.Pack(big.NewInt(1))
			return "0x" + common.Bytes2Hex(out), nil
		case string(skyLockstakeEngineABI.Methods["ownerUrns"].ID):
			out, _ := skyLockstakeEngineABI.Methods["ownerUrns"].Outputs.Pack(s.urn)
			return "0x" + common.Bytes2Hex(out), nil
		case string(skyLockstakeEngineABI.Methods["urnFarms"].ID):
			out, _ := skyLockstakeEngineABI.Methods["urnFarms"].Outputs.Pack(common.Address{})
			return "0x" + common.Bytes2Hex(out), nil
		}
		return "", fail
	case makerDeployment.Vat:
		switch selector {
		case string(makerVatABI.Methods["ilks"].ID):
			out, _ := makerVatABI.Methods["ilks"].Outputs.Pack(
				big.NewInt(0), s.rate, big.NewInt(0), big.NewInt(0), big.NewInt(0),
			)
			return "0x" + common.Bytes2Hex(out), nil
		case string(makerVatABI.Methods["urns"].ID):
			out, _ := makerVatABI.Methods["urns"].Outputs.Pack(s.ink, s.art)
			return "0x" + common.Bytes2Hex(out), nil
		}
		return "", fail
	}
	return "", map[string]any{"code": -32602, "message": "unexpected contract"}
}

func (s *skyStubRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	raw, err := io.ReadAll(request.Body)
	if err != nil {
		s.t.Errorf("read RPC body: %v", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	answer := func(call skyStubCall) map[string]any {
		response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
		switch call.Method {
		case "eth_chainId":
			response["result"] = "0x1"
			return response
		case "eth_call":
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unexpected method"}
			return response
		}
		var input struct {
			To   common.Address `json:"to"`
			Data string         `json:"data"`
		}
		if err := json.Unmarshal(call.Params[0], &input); err != nil {
			s.t.Errorf("decode eth_call input: %v", err)
			response["error"] = map[string]any{"code": -32602, "message": "bad input"}
			return response
		}
		result, failure := s.dispatch(input.To, common.FromHex(input.Data))
		if failure != nil {
			response["error"] = failure
		} else {
			response["result"] = result
		}
		return response
	}
	if len(raw) > 0 && raw[0] == '{' {
		var call skyStubCall
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Errorf("decode singleton RPC call: %v", err)
			return
		}
		_ = json.NewEncoder(writer).Encode(answer(call))
		return
	}
	var calls []skyStubCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Errorf("decode RPC batch: %v", err)
		return
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

// TestSkyVaultFailureKeepsLockstake is the regression for the surface-isolation contract in
// adapter.go: a savings-vault read that fails must not discard the lockstake groups the Vat
// and engine returned independently. Before the fix Positions returned (nil, err) on the vault
// error and never called the lockstake read at all.
func TestSkyVaultFailureKeepsLockstake(t *testing.T) {
	ray := new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)
	stub := &skyStubRPC{
		t:          t,
		failVaults: true,
		urn:        common.HexToAddress("0x00000000000000000000000000000000000000c1"),
		ink:        new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil),
		art:        big.NewInt(0),
		rate:       ray,
	}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	client, err := DialRPC(t.Context(), Ethereum, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	groups, err := newSkyAdapter().Positions(
		t.Context(),
		client,
		BlockRef{ChainID: Ethereum, Number: 25_000_000, Fixed: true},
		common.HexToAddress("0x00000000000000000000000000000000000000a1"),
	)
	if err == nil {
		t.Fatal("expected the savings-vault failure to be reported")
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want the lockstake group preserved alongside the error: %#v", len(groups), groups)
	}
	if groups[0].MarketID != "lockstake" {
		t.Fatalf("preserved group = %q, want lockstake", groups[0].MarketID)
	}
	if groups[0].NetValuePolicy != "floor-zero" {
		t.Errorf(
			"lockstake NetValuePolicy = %q, want floor-zero; an unpriced SKY leg would otherwise "+
				"let the USDS debt drive the whole snapshot negative",
			groups[0].NetValuePolicy,
		)
	}
	if len(groups[0].Components) != 1 || groups[0].Components[0].Token.Symbol != "SKY" {
		t.Fatalf("lockstake components = %#v, want one locked SKY leg", groups[0].Components)
	}
}

// TestSkyLockstakeVatPredatesTheEngine makes an implicit dependency explicit. skyLockstakeGroups
// reads makerDeployment.Vat without a window of its own; that is only safe because the Vat was
// created 13.4M blocks before the engine, so the engine's window already gates it.
func TestSkyLockstakeVatPredatesTheEngine(t *testing.T) {
	const vatCreationBlock = 8_928_152
	if skyLockstakeDeployment.Window.ActivationBlock <= vatCreationBlock {
		t.Fatalf(
			"lockstake window %d does not gate the Vat, created at %d; the Vat needs its own window",
			skyLockstakeDeployment.Window.ActivationBlock,
			vatCreationBlock,
		)
	}
}
