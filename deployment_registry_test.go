package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type availabilityRPCRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

type availabilityRPCServer struct {
	t     *testing.T
	block uint64
}

func (s availabilityRPCServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s.t.Helper()
	var call availabilityRPCRequest
	if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
		s.t.Fatalf("decode RPC request: %v", err)
	}
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = "0x38"
	case "eth_getBlockByNumber":
		response["result"] = map[string]any{
			"number":    fmt.Sprintf("0x%x", s.block),
			"hash":      common.BytesToHash([]byte{byte(s.block), 1}).Hex(),
			"timestamp": "0x1",
		}
	default:
		s.t.Fatalf("unexpected RPC method %q", call.Method)
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		s.t.Fatalf("encode RPC response: %v", err)
	}
}

type availabilityTestAdapter struct {
	adapterBase
	calls int
}

func (a *availabilityTestAdapter) Positions(
	context.Context,
	*RPCClient,
	BlockRef,
	common.Address,
) ([]Group, error) {
	a.calls++
	return nil, nil
}

func TestAvailabilityWindowConstructorsHaveExplicitInclusiveBounds(t *testing.T) {
	tests := []struct {
		name   string
		window availabilityWindow
		blocks map[uint64]bool
	}{
		{
			name:   "from genesis",
			window: availableFromGenesis(),
			blocks: map[uint64]bool{0: true, 1: true, 1_000_000: true},
		},
		{
			name:   "from activation",
			window: availableFrom(100),
			blocks: map[uint64]bool{99: false, 100: true, 101: true},
		},
		{
			name:   "bounded",
			window: availableBetween(100, 199),
			blocks: map[uint64]bool{99: false, 100: true, 199: true, 200: false},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for block, want := range test.blocks {
				if got := test.window.ActiveAt(block); got != want {
					t.Errorf("ActiveAt(%d) = %v, want %v", block, got, want)
				}
			}
		})
	}
}

func TestAvailabilityWindowConstructorsRejectAmbiguousOrInvalidBounds(t *testing.T) {
	tests := []struct {
		name      string
		construct func()
	}{
		{name: "from zero", construct: func() { availableFrom(0) }},
		{name: "bounded from zero", construct: func() { availableBetween(0, 100) }},
		{name: "bounded without end", construct: func() { availableBetween(100, 0) }},
		{name: "bounded in reverse", construct: func() { availableBetween(100, 99) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			panicked := false
			func() {
				defer func() {
					panicked = recover() != nil
				}()
				test.construct()
			}()
			if !panicked {
				t.Fatal("constructor accepted invalid availability bounds")
			}
		})
	}
}

func TestRegisterAdaptersSupportsMultipleAvailabilityWindowsPerChain(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum},
	}}}
	registrations, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol": {
				Ethereum: {availableBetween(100, 199), availableFrom(300)},
			},
		},
	)
	if err != nil {
		t.Fatalf("register adapters: %v", err)
	}
	registration := registrations["test-protocol"]
	for block, want := range map[uint64]bool{
		99: false, 100: true, 199: true, 200: false, 299: false, 300: true,
	} {
		if got := registration.ActiveAt(Ethereum, block); got != want {
			t.Errorf("ActiveAt(Ethereum, %d) = %v, want %v", block, got, want)
		}
	}
}

func TestRegisterAdaptersRejectsInvalidMultipleAvailabilityWindows(t *testing.T) {
	tests := []struct {
		name      string
		windows   []availabilityWindow
		wantError string
	}{
		{
			name: "unordered",
			windows: []availabilityWindow{
				availableBetween(300, 399),
				availableBetween(100, 199),
			},
			wantError: "not ordered",
		},
		{
			name: "overlap at inclusive boundary",
			windows: []availabilityWindow{
				availableBetween(100, 200),
				availableBetween(200, 300),
			},
			wantError: "overlap",
		},
		{
			name: "window after open ended interval",
			windows: []availabilityWindow{
				availableFrom(100),
				availableFrom(300),
			},
			wantError: "overlap",
		},
		{
			name: "malformed direct window",
			windows: []availabilityWindow{{
				configured: true,
				deploymentWindow: deploymentWindow{
					ActivationBlock:   200,
					DeactivationBlock: 100,
				},
			}},
			wantError: "ends before it starts",
		},
		{
			name: "implicit bounded genesis",
			windows: []availabilityWindow{{
				configured: true,
				deploymentWindow: deploymentWindow{
					DeactivationBlock: 100,
				},
			}},
			wantError: "implicit genesis start",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
				ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum},
			}}}
			_, err := registerAdapters(
				[]Adapter{adapter},
				map[string]chainAvailability{
					"test-protocol": {Ethereum: test.windows},
				},
			)
			if err == nil {
				t.Fatal("registration accepted invalid availability windows")
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("registration error = %q, want it to contain %q", err, test.wantError)
			}
		})
	}
}

func TestRegisterAdaptersRejectsMissingAdvertisedChain(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum, Base},
	}}}
	_, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol": {Ethereum: {availableFromGenesis()}},
		},
	)
	if err == nil {
		t.Fatal("registration accepted a protocol with no Base availability")
	}
}

func TestRegisterAdaptersRejectsUnadvertisedChain(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum},
	}}}
	_, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol": {
				Ethereum: {availableFromGenesis()},
				Base:     {availableFromGenesis()},
			},
		},
	)
	if err == nil {
		t.Fatal("registration accepted availability for unadvertised Base chain")
	}
}

func TestRegisterAdaptersRejectsZeroValueAvailabilityWindow(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum},
	}}}
	_, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol": {Ethereum: {availabilityWindow{}}},
		},
	)
	if err == nil {
		t.Fatal("registration accepted a zero-value availability window as genesis")
	}
}

func TestRegisterAdaptersRejectsUnknownProtocolAvailability(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum},
	}}}
	_, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol":    {Ethereum: {availableFromGenesis()}},
			"removed-protocol": {Ethereum: {availableFromGenesis()}},
		},
	)
	if err == nil {
		t.Fatal("registration accepted availability for an unknown protocol")
	}
}

func TestRegisterAdaptersRejectsDuplicateProtocolIDs(t *testing.T) {
	info := ProtocolInfo{ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum}}
	_, err := registerAdapters(
		[]Adapter{
			&availabilityTestAdapter{adapterBase: adapterBase{info: info}},
			&availabilityTestAdapter{adapterBase: adapterBase{info: info}},
		},
		map[string]chainAvailability{
			"test-protocol": {Ethereum: {availableFromGenesis()}},
		},
	)
	if err == nil {
		t.Fatal("registration accepted duplicate protocol IDs")
	}
}

func TestRegisterAdaptersRejectsDuplicateAdvertisedChains(t *testing.T) {
	adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{Ethereum, Ethereum},
	}}}
	_, err := registerAdapters(
		[]Adapter{adapter},
		map[string]chainAvailability{
			"test-protocol": {Ethereum: {availableFromGenesis()}},
		},
	)
	if err == nil {
		t.Fatal("registration accepted a duplicate advertised chain")
	}
}

func TestEngineSkipsAdapterOutsideCentralAvailability(t *testing.T) {
	for _, test := range []struct {
		name      string
		block     uint64
		wantCalls int
	}{
		{name: "before activation", block: 99, wantCalls: 0},
		{name: "at activation", block: 100, wantCalls: 1},
		{name: "at deactivation", block: 199, wantCalls: 1},
		{name: "after deactivation", block: 200, wantCalls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(availabilityRPCServer{t: t, block: test.block})
			t.Cleanup(server.Close)
			adapter := &availabilityTestAdapter{adapterBase: adapterBase{info: ProtocolInfo{
				ID: "test-protocol", Name: "Test Protocol", Chains: []ChainID{BSC},
			}}}
			registrations, err := registerAdapters(
				[]Adapter{adapter},
				map[string]chainAvailability{
					"test-protocol": {BSC: {availableBetween(100, 199)}},
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			engine := &Engine{
				rpcURLs:       map[ChainID]string{BSC: server.URL},
				adapters:      []Adapter{adapter},
				registrations: registrations,
				headLagBlocks: defaultHeadLagBlocks,
			}
			response := engine.ScanWithOptions(
				context.Background(),
				common.HexToAddress("0x1234"),
				ScanOptions{
					ChainIDs:    map[ChainID]struct{}{BSC: {}},
					BlockNumber: map[ChainID]uint64{BSC: test.block},
					SkipPrices:  true,
				},
			)
			if len(response.Errors) != 0 {
				t.Fatalf("scan errors = %v", response.Errors)
			}
			if adapter.calls != test.wantCalls {
				t.Fatalf("Positions calls = %d, want %d", adapter.calls, test.wantCalls)
			}
		})
	}
}

func TestNewEngineHasValidatedAvailabilityForEveryAdapter(t *testing.T) {
	engine := NewEngine(nil, nil)
	if len(engine.registrations) != len(engine.adapters) {
		t.Fatalf(
			"validated registrations = %d, adapters = %d",
			len(engine.registrations),
			len(engine.adapters),
		)
	}
	for _, adapter := range engine.adapters {
		info := adapter.Info()
		registration, exists := engine.registrations[info.ID]
		if !exists {
			t.Errorf("protocol %q has no validated availability", info.ID)
			continue
		}
		for _, chainID := range info.Chains {
			if len(registration.availability[chainID]) == 0 {
				t.Errorf("protocol %q chain %d has no availability", info.ID, chainID)
			}
		}
	}
}

type expectedProtocolAvailabilityWindow struct {
	protocolID        string
	chainID           ChainID
	activationBlock   uint64
	deactivationBlock uint64
	fromGenesis       bool
}

// verifiedProtocolAvailabilityWindows intentionally duplicates the archive-RPC
// boundaries in protocol_availability.go. Deriving these expectations from the
// production map would only prove availabilityWindow.ActiveAt, not that a deployment
// block was entered correctly.
var verifiedProtocolAvailabilityWindows = []expectedProtocolAvailabilityWindow{
	{protocolID: "aave-v3", chainID: Ethereum, activationBlock: 16_291_078},
	{protocolID: "aave-v3", chainID: BSC, activationBlock: 46_367_909},
	{protocolID: "aave-v3", chainID: Base, activationBlock: 25_954_709},
	{protocolID: "aave-v3", chainID: Arbitrum, activationBlock: 302_650_382},
	{protocolID: "aave-v2", chainID: Ethereum, activationBlock: 10_927_018},
	{protocolID: "spark", chainID: Ethereum, activationBlock: 16_776_391},
	{protocolID: "spark", chainID: Base, activationBlock: 27_123_520},
	{protocolID: "spark", chainID: Arbitrum, activationBlock: 311_940_473},
	{protocolID: "kinza", chainID: BSC, activationBlock: 29_232_063},
	{protocolID: "seamless", chainID: Base, activationBlock: 3_318_562},
	{protocolID: "compound-v2", chainID: Ethereum, activationBlock: 10_271_924},
	{protocolID: "moonwell", chainID: Base, activationBlock: 2_162_402},
	{protocolID: "flux-finance", chainID: Ethereum, activationBlock: 16_520_940},
	{protocolID: "sonne", chainID: Base, activationBlock: 2_492_954},
	{protocolID: "lodestar", chainID: Arbitrum, activationBlock: 111_013_008},
	{protocolID: "venus", chainID: Ethereum, activationBlock: 18_890_246},
	{protocolID: "venus", chainID: BSC, activationBlock: 2_471_694},
	{protocolID: "venus", chainID: Base, activationBlock: 23_341_263},
	{protocolID: "venus", chainID: Arbitrum, activationBlock: 215_551_349},
	{protocolID: "compound-v3", chainID: Ethereum, activationBlock: 15_331_586},
	{protocolID: "compound-v3", chainID: Base, activationBlock: 2_197_588},
	{protocolID: "compound-v3", chainID: Arbitrum, activationBlock: 87_335_214},
	{protocolID: "fluid-lite", chainID: Ethereum, activationBlock: 16_609_585},
	{protocolID: "cap", chainID: Ethereum, activationBlock: 22_874_057},
	{protocolID: "ethena", chainID: Ethereum, activationBlock: 18_571_359},
	{protocolID: "usdd", chainID: Ethereum, activationBlock: 23_275_147},
	{protocolID: "usdd", chainID: BSC, activationBlock: 63_887_220},
	{protocolID: "unitas", chainID: BSC, activationBlock: 69_059_010},
	{protocolID: "liquid-collective", chainID: Ethereum, activationBlock: 15_676_402},
	{protocolID: "lido", chainID: Ethereum, activationBlock: 11_473_216},
	{protocolID: "meth-protocol", chainID: Ethereum, activationBlock: 18_290_599},
	{protocolID: "etherfi", chainID: Ethereum, activationBlock: 17_664_324},
	{protocolID: "etherfi", chainID: BSC, activationBlock: 38_098_558},
	{protocolID: "etherfi", chainID: Base, activationBlock: 13_524_685},
	{protocolID: "etherfi", chainID: Arbitrum, activationBlock: 156_547_814},
	{protocolID: "frax-ether", chainID: Ethereum, activationBlock: 15_686_046},
	{protocolID: "renzo", chainID: Ethereum, activationBlock: 18_722_779},
	{protocolID: "renzo", chainID: BSC, activationBlock: 36_596_546},
	{protocolID: "renzo", chainID: Base, activationBlock: 12_682_160},
	{protocolID: "renzo", chainID: Arbitrum, activationBlock: 185_410_162},
	{protocolID: "aster", chainID: BSC, activationBlock: 43_713_424},
	{protocolID: "fxprotocol", chainID: Ethereum, activationBlock: 17_818_955},
	{protocolID: "rocketpool", chainID: Ethereum, activationBlock: 13_325_532},
	{protocolID: "stader", chainID: Ethereum, activationBlock: 17_416_153},
	{protocolID: "stader", chainID: BSC, activationBlock: 19_907_065},
	{protocolID: "olympus", chainID: Ethereum, activationBlock: 13_803_969},
	{protocolID: "fraxlend", chainID: Ethereum, activationBlock: 15_993_000},
	{protocolID: "aave-v4", chainID: Ethereum, activationBlock: 24_720_887},
	{protocolID: "makerdao", chainID: Ethereum, activationBlock: 10_091_068},
	{protocolID: "sky", chainID: Ethereum, activationBlock: 20_677_434},
	{protocolID: "maple", chainID: Ethereum, activationBlock: 16_162_315},
	{protocolID: "liquity-v1", chainID: Ethereum, activationBlock: 12_178_557},
	{protocolID: "crvusd", chainID: Ethereum, activationBlock: 17_257_955},
	{protocolID: "curve-lending", chainID: Ethereum, activationBlock: 19_422_660},
	{protocolID: "curve-lending", chainID: Arbitrum, activationBlock: 193_652_535},
	{protocolID: "vesper", chainID: Ethereum, activationBlock: 11_407_993},
	{protocolID: "vesper", chainID: Base, activationBlock: 15_153_629},
	{protocolID: "yearn-v3", chainID: Ethereum, activationBlock: 18_817_046},
	{protocolID: "yearn-v3", chainID: Base, activationBlock: 17_834_110},
	{protocolID: "yearn-v3", chainID: Arbitrum, activationBlock: 173_129_408},
	{protocolID: "beefy", chainID: Ethereum, activationBlock: 15_982_782},
	{protocolID: "beefy", chainID: BSC, activationBlock: 1_174_856},
	{protocolID: "beefy", chainID: Base, activationBlock: 2_572_135},
	{protocolID: "beefy", chainID: Arbitrum, activationBlock: 3_005_534},
	{protocolID: "stakewise", chainID: Ethereum, activationBlock: 18_470_152},
	{protocolID: "lista", chainID: Ethereum, activationBlock: 23_445_769},
	{protocolID: "lista", chainID: BSC, activationBlock: 20_324_823},
	{protocolID: "euler-v2", chainID: Ethereum, activationBlock: 20_529_207},
	{protocolID: "euler-v2", chainID: BSC, activationBlock: 46_370_645},
	{protocolID: "euler-v2", chainID: Base, activationBlock: 22_282_353},
	{protocolID: "euler-v2", chainID: Arbitrum, activationBlock: 300_690_886},
	{protocolID: "morpho-blue", chainID: Ethereum, activationBlock: 18_883_124},
	{protocolID: "morpho-blue", chainID: BSC, activationBlock: 54_344_680},
	{protocolID: "morpho-blue", chainID: Base, activationBlock: 13_977_148},
	{protocolID: "morpho-blue", chainID: Arbitrum, activationBlock: 296_446_593},
	{protocolID: "pendle", chainID: Ethereum, activationBlock: 16_032_048},
	{protocolID: "pendle", chainID: BSC, activationBlock: 29_484_198},
	{protocolID: "pendle", chainID: Base, activationBlock: 22_350_319},
	{protocolID: "pendle", chainID: Arbitrum, activationBlock: 62_977_844},
	{protocolID: "fluid", chainID: Ethereum, activationBlock: 19_245_687},
	{protocolID: "fluid", chainID: BSC, activationBlock: 71_737_128},
	{protocolID: "fluid", chainID: Base, activationBlock: 38_678_564},
	{protocolID: "fluid", chainID: Arbitrum, activationBlock: 228_709_698},
	{protocolID: "uniswap-v3", chainID: Ethereum, activationBlock: 12_369_651},
	{protocolID: "uniswap-v3", chainID: BSC, activationBlock: 26_324_045},
	{protocolID: "uniswap-v3", chainID: Base, activationBlock: 1_371_714},
	{protocolID: "uniswap-v3", chainID: Arbitrum, activationBlock: 173},
	{protocolID: "uniswap-v4", chainID: Ethereum, activationBlock: 21_689_089},
	{protocolID: "uniswap-v4", chainID: BSC, activationBlock: 45_970_613},
	{protocolID: "uniswap-v4", chainID: Base, activationBlock: 25_350_993},
	{protocolID: "uniswap-v4", chainID: Arbitrum, activationBlock: 297_842_893},
	{protocolID: "wallet", chainID: Ethereum, fromGenesis: true},
	{protocolID: "wallet", chainID: BSC, fromGenesis: true},
	{protocolID: "wallet", chainID: Base, fromGenesis: true},
	{protocolID: "wallet", chainID: Arbitrum, fromGenesis: true},
}

func TestProtocolAvailabilityMatchesVerifiedBoundaries(t *testing.T) {
	type availabilityKey struct {
		protocolID string
		chainID    ChainID
	}
	expectedProtocols := make(map[string]struct{})
	expectedWindowCountByKey := make(map[availabilityKey]int)
	for _, expected := range verifiedProtocolAvailabilityWindows {
		expectedProtocols[expected.protocolID] = struct{}{}
		key := availabilityKey{protocolID: expected.protocolID, chainID: expected.chainID}
		windowIndex := expectedWindowCountByKey[key]
		expectedWindowCountByKey[key]++

		availability, exists := protocolAvailabilityByID[expected.protocolID]
		if !exists {
			t.Errorf("protocol %q is missing", expected.protocolID)
			continue
		}
		windows, exists := availability[expected.chainID]
		if !exists {
			t.Errorf("protocol %q chain %d is missing", expected.protocolID, expected.chainID)
			continue
		}
		if windowIndex >= len(windows) {
			t.Errorf(
				"protocol %q chain %d has %d windows, want at least %d",
				expected.protocolID,
				expected.chainID,
				len(windows),
				windowIndex+1,
			)
			continue
		}
		window := windows[windowIndex]
		if !window.configured {
			t.Errorf(
				"protocol %q chain %d window %d is not configured",
				expected.protocolID,
				expected.chainID,
				windowIndex,
			)
		}
		if expected.fromGenesis != (window.deploymentWindow.ActivationBlock == 0) {
			t.Errorf(
				"protocol %q chain %d window %d genesis = %v, want %v",
				expected.protocolID,
				expected.chainID,
				windowIndex,
				window.deploymentWindow.ActivationBlock == 0,
				expected.fromGenesis,
			)
		}
		if got := window.deploymentWindow.ActivationBlock; got != expected.activationBlock {
			t.Errorf(
				"protocol %q chain %d window %d activation = %d, want %d",
				expected.protocolID,
				expected.chainID,
				windowIndex,
				got,
				expected.activationBlock,
			)
		}
		if got := window.deploymentWindow.DeactivationBlock; got != expected.deactivationBlock {
			t.Errorf(
				"protocol %q chain %d window %d deactivation = %d, want %d",
				expected.protocolID,
				expected.chainID,
				windowIndex,
				got,
				expected.deactivationBlock,
			)
		}
	}

	actualChainCount := 0
	actualWindowCount := 0
	for _, availability := range protocolAvailabilityByID {
		actualChainCount += len(availability)
		for _, windows := range availability {
			actualWindowCount += len(windows)
		}
	}
	if got, want := len(protocolAvailabilityByID), len(expectedProtocols); got != want {
		t.Errorf("protocol availability count = %d, want %d", got, want)
	}
	if got, want := actualChainCount, len(expectedWindowCountByKey); got != want {
		t.Errorf("protocol-chain availability count = %d, want %d", got, want)
	}
	if got, want := actualWindowCount, len(verifiedProtocolAvailabilityWindows); got != want {
		t.Errorf("availability window count = %d, want %d", got, want)
	}
	for key, want := range expectedWindowCountByKey {
		if got := len(protocolAvailabilityByID[key.protocolID][key.chainID]); got != want {
			t.Errorf("protocol %q chain %d windows = %d, want %d", key.protocolID, key.chainID, got, want)
		}
	}
}

func requireExactComponentAvailability(
	t *testing.T,
	name string,
	window availabilityWindow,
	activationBlock uint64,
) {
	t.Helper()
	if !window.configured {
		t.Fatalf("%s availability is not configured", name)
	}
	if got := window.deploymentWindow.ActivationBlock; got != activationBlock {
		t.Fatalf("%s activation block = %d, want %d", name, got, activationBlock)
	}
	if got := window.deploymentWindow.DeactivationBlock; got != 0 {
		t.Fatalf("%s deactivation block = %d, want open-ended", name, got)
	}
}

func TestComponentAvailabilityMatchesVerifiedBoundaries(t *testing.T) {
	compoundByID := make(map[string]*CompoundV2Adapter)
	for _, candidate := range compoundV2Adapters() {
		adapter := candidate.(*CompoundV2Adapter)
		compoundByID[adapter.Info().ID] = adapter
	}
	compoundDeployment := func(protocolID string, chainID ChainID) compoundV2Deployment {
		t.Helper()
		adapter, exists := compoundByID[protocolID]
		if !exists {
			t.Fatalf("compound-v2 family adapter %q is missing", protocolID)
		}
		deployment, exists := adapter.deployments[chainID]
		if !exists {
			t.Fatalf("compound-v2 family adapter %q chain %d is missing", protocolID, chainID)
		}
		return deployment
	}

	compound := compoundDeployment("compound-v2", Ethereum)
	requireExactComponentAvailability(t, "Compound v2 comptroller", compound.ComptrollerWindow, 10_271_924)
	requireExactComponentAvailability(t, "Compound v2 rewards", compound.RewardLensWindow, 13_468_648)
	moonwell := compoundDeployment("moonwell", Base)
	requireExactComponentAvailability(t, "Moonwell comptroller", moonwell.ComptrollerWindow, 2_162_402)
	requireExactComponentAvailability(t, "Moonwell rewards", moonwell.MultiRewardWindow, 2_162_417)
	if got, want := len(moonwell.StakingModules), 1; got != want {
		t.Fatalf("Moonwell staking modules = %d, want %d", got, want)
	}
	requireExactComponentAvailability(t, "Moonwell staking", moonwell.StakingModules[0].Window, 12_187_715)
	requireExactComponentAvailability(
		t,
		"Flux Finance comptroller",
		compoundDeployment("flux-finance", Ethereum).ComptrollerWindow,
		16_520_940,
	)
	requireExactComponentAvailability(
		t,
		"Sonne comptroller",
		compoundDeployment("sonne", Base).ComptrollerWindow,
		2_492_954,
	)
	requireExactComponentAvailability(
		t,
		"Lodestar comptroller",
		compoundDeployment("lodestar", Arbitrum).ComptrollerWindow,
		111_013_008,
	)

	venus := newVenusAdapter().(*VenusAdapter)
	for _, expected := range []struct {
		chainID         ChainID
		poolRegistry    uint64
		poolLens        uint64
		xvsVault        uint64
		coreComptroller uint64
		coreRewards     uint64
	}{
		{chainID: Ethereum, poolRegistry: 18_968_019, poolLens: 22_886_599, xvsVault: 18_890_246},
		{
			chainID: BSC, poolRegistry: 29_335_016, poolLens: 70_345_534,
			xvsVault: 13_019_089, coreComptroller: 2_471_694, coreRewards: 105_725_871,
		},
		{chainID: Base, poolRegistry: 23_344_365, poolLens: 23_344_435, xvsVault: 23_341_263},
		{
			chainID: Arbitrum, poolRegistry: 216_184_381,
			poolLens: 216_184_982, xvsVault: 215_551_349,
		},
	} {
		deployment, exists := venus.deployments[expected.chainID]
		if !exists {
			t.Fatalf("Venus chain %d deployment is missing", expected.chainID)
		}
		prefix := fmt.Sprintf("Venus chain %d", expected.chainID)
		requireExactComponentAvailability(t, prefix+" pool registry", deployment.PoolRegistryWindow, expected.poolRegistry)
		requireExactComponentAvailability(t, prefix+" pool lens", deployment.PoolLensWindow, expected.poolLens)
		requireExactComponentAvailability(t, prefix+" XVS vault", deployment.XVSVaultWindow, expected.xvsVault)
		if expected.coreComptroller != 0 {
			if deployment.Core == nil {
				t.Fatalf("%s core deployment is missing", prefix)
			}
			requireExactComponentAvailability(t, prefix+" core comptroller", deployment.Core.ComptrollerWindow, expected.coreComptroller)
		}
		if expected.coreRewards != 0 {
			requireExactComponentAvailability(t, prefix+" core rewards", deployment.CoreRewardsWindow, expected.coreRewards)
		}
	}

	for _, expected := range []struct {
		name       string
		window     availabilityWindow
		activation uint64
	}{
		{name: "Fraxlend registry", window: fraxlendRegistryWindow, activation: 15_993_000},
		{name: "Maker savings", window: makerSavingsWindow, activation: 10_091_068},
		{name: "Maker vaults", window: makerVaultWindow, activation: 12_251_955},
		{name: "Rocket Pool tokens", window: rocketTokenWindow, activation: 13_325_532},
		{name: "Rocket Pool nodes", window: rocketNodeWindow, activation: 24_479_994},
	} {
		requireExactComponentAvailability(t, expected.name, expected.window, expected.activation)
	}

	lst := lstAdapters()[0].(*ConvertedBalanceAdapter)
	if got, want := lst.positions[Ethereum][0].ActivationBlock, uint64(15_676_402); got != want {
		t.Fatalf("Liquid Collective activation block = %d, want %d", got, want)
	}
}

type compoundZeroAccountRPCServer struct {
	t                *testing.T
	comptroller      common.Address
	rewardLens       common.Address
	comptrollerCalls int
	rewardLensCalls  int
}

func (s *compoundZeroAccountRPCServer) answer(call availabilityRPCRequest) map[string]any {
	s.t.Helper()
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = "0x1"
	case "eth_call":
		if len(call.Params) == 0 {
			s.t.Fatal("eth_call has no transaction parameter")
		}
		var transaction struct {
			To common.Address `json:"to"`
		}
		if err := json.Unmarshal(call.Params[0], &transaction); err != nil {
			s.t.Fatalf("decode eth_call transaction: %v", err)
		}
		switch transaction.To {
		case s.comptroller:
			s.comptrollerCalls++
			encoded, err := comptrollerABI.Methods["getAllMarkets"].Outputs.Pack([]common.Address{})
			if err != nil {
				s.t.Fatalf("encode empty market list: %v", err)
			}
			response["result"] = "0x" + common.Bytes2Hex(encoded)
		case s.rewardLens:
			s.rewardLensCalls++
			response["error"] = map[string]any{
				"code": 3, "message": "zero-account reward query must be skipped",
			}
		default:
			s.t.Fatalf("unexpected eth_call target %s", transaction.To)
		}
	default:
		s.t.Fatalf("unexpected RPC method %q", call.Method)
	}
	return response
}

func (s *compoundZeroAccountRPCServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil {
		s.t.Fatalf("decode RPC request: %v", err)
	}
	writer.Header().Set("Content-Type", "application/json")
	if len(raw) > 0 && raw[0] == '{' {
		var call availabilityRPCRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			s.t.Fatalf("decode RPC call: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(s.answer(call))
		return
	}
	var calls []availabilityRPCRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		s.t.Fatalf("decode RPC batch: %v", err)
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = s.answer(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func TestCompoundV2ZeroAccountSkipsRewardLens(t *testing.T) {
	var adapter *CompoundV2Adapter
	for _, candidate := range compoundV2Adapters() {
		if candidate.Info().ID == "compound-v2" {
			adapter = candidate.(*CompoundV2Adapter)
			break
		}
	}
	if adapter == nil {
		t.Fatal("Compound v2 adapter is missing")
	}
	deployment := adapter.deployments[Ethereum]
	server := &compoundZeroAccountRPCServer{
		t:           t,
		comptroller: deployment.Comptroller,
		rewardLens:  deployment.RewardLens,
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := DialRPC(context.Background(), Ethereum, httpServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	groups, err := adapter.Positions(
		context.Background(),
		client,
		BlockRef{ChainID: Ethereum, Number: 13_468_648},
		common.Address{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("zero-account groups = %+v, want none", groups)
	}
	if server.comptrollerCalls != 1 {
		t.Fatalf("comptroller calls = %d, want 1", server.comptrollerCalls)
	}
	if server.rewardLensCalls != 0 {
		t.Fatalf("reward lens calls = %d, want 0", server.rewardLensCalls)
	}
}

func TestEveryProtocolAvailabilityWindowHasInclusiveBoundaries(t *testing.T) {
	for protocolID, availability := range protocolAvailabilityByID {
		for chainID, windows := range availability {
			for index, window := range windows {
				start := window.deploymentWindow.ActivationBlock
				end := window.deploymentWindow.DeactivationBlock
				if !window.ActiveAt(start) {
					t.Errorf(
						"protocol %q chain %d window %d is inactive at start block %d",
						protocolID,
						chainID,
						index,
						start,
					)
				}
				if start > 0 && window.ActiveAt(start-1) {
					t.Errorf(
						"protocol %q chain %d window %d is active before start block %d",
						protocolID,
						chainID,
						index,
						start,
					)
				}
				if end > 0 {
					if !window.ActiveAt(end) {
						t.Errorf(
							"protocol %q chain %d window %d is inactive at end block %d",
							protocolID,
							chainID,
							index,
							end,
						)
					}
					if window.ActiveAt(end + 1) {
						t.Errorf(
							"protocol %q chain %d window %d is active after end block %d",
							protocolID,
							chainID,
							index,
							end,
						)
					}
				}
			}
		}
	}
}
