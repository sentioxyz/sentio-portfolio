package portfolio

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func testSentioIndexerConfig(name string) SentioIndexerConfig {
	return SentioIndexerConfig{
		GraphQLURL:       "https://example.invalid/" + name + "/graphql",
		StatusURL:        "https://example.invalid/" + name + "/status",
		ProcessorVersion: "test-version",
	}
}

func TestEngineRegistersAtLeastTwentyUniqueProtocols(t *testing.T) {
	protocols := NewEngine(nil, nil).Protocols()
	if len(protocols) < 20 {
		t.Fatalf("protocol count = %d, want at least 20", len(protocols))
	}
	seen := make(map[string]struct{}, len(protocols))
	for _, protocol := range protocols {
		if protocol.ID == "" || protocol.Name == "" {
			t.Fatalf("protocol has an empty identity: %+v", protocol)
		}
		if _, exists := seen[protocol.ID]; exists {
			t.Fatalf("duplicate protocol id %q", protocol.ID)
		}
		seen[protocol.ID] = struct{}{}
		if len(protocol.Chains) == 0 {
			t.Fatalf("protocol %q has no chains", protocol.ID)
		}
	}
	for _, required := range []string{
		"aster", "beefy", "etherfi", "euler-v2", "fluid", "frax-ether", "fxprotocol",
		"lista", "morpho-blue", "renzo", "stakewise",
	} {
		if _, exists := seen[required]; !exists {
			t.Errorf("protocol %q is not registered", required)
		}
	}
}

func TestEngineWiresRuntimeSentioIndexerConfig(t *testing.T) {
	configs := map[string]SentioIndexerConfig{
		"aster":         testSentioIndexerConfig("aster"),
		"etherfi":       testSentioIndexerConfig("etherfi"),
		"euler-v2":      testSentioIndexerConfig("euler-v2"),
		"frax-ether":    testSentioIndexerConfig("frax-ether"),
		"meth-protocol": testSentioIndexerConfig("meth"),
		"morpho-blue":   testSentioIndexerConfig("morpho"),
		"renzo":         testSentioIndexerConfig("renzo"),
		"uniswap-v3":    testSentioIndexerConfig("uniswap-v3"),
		"uniswap-v4":    testSentioIndexerConfig("uniswap-v4"),
	}
	engine := NewEngineWithConfig(nil, nil, EngineConfig{SentioIndexers: configs})
	found := make(map[string]bool)
	for _, adapter := range engine.adapters {
		switch typed := adapter.(type) {
		case *AsterAdapter:
			if typed.indexer.config != configs["aster"] {
				t.Fatalf("Aster indexer config was not wired")
			}
			found["aster"] = true
		case *EtherfiAdapter:
			if typed.indexer.config != configs["etherfi"] {
				t.Fatalf("Ether.fi indexer config was not wired")
			}
			found["etherfi"] = true
		case *EulerV2Adapter:
			indexer, ok := typed.indexer.(*eulerIndexer)
			if !ok || indexer.config != configs["euler-v2"] {
				t.Fatalf("Euler V2 indexer config was not wired")
			}
			found["euler-v2"] = true
		case *FraxEtherAdapter:
			if typed.indexer.config != configs["frax-ether"] {
				t.Fatalf("Frax Ether indexer config was not wired")
			}
			found["frax-ether"] = true
		case *MethAdapter:
			if typed.indexer != configs["meth-protocol"] {
				t.Fatalf("mETH indexer config = %+v, want %+v", typed.indexer, configs["meth-protocol"])
			}
			found["meth-protocol"] = true
		case *MorphoAdapter:
			indexer, ok := typed.indexer.(*morphoIndexer)
			if !ok || indexer.config != configs["morpho-blue"] {
				t.Fatalf("Morpho indexer config was not wired")
			}
			found["morpho-blue"] = true
		case *RenzoAdapter:
			if typed.indexer.config != configs["renzo"] {
				t.Fatalf("Renzo indexer config was not wired")
			}
			found["renzo"] = true
		case *UniswapV3Adapter:
			if typed.indexer.configs[uniswapV3] != configs["uniswap-v3"] {
				t.Fatalf("Uniswap V3 indexer config was not wired")
			}
			found["uniswap-v3"] = true
		case *UniswapV4Adapter:
			if typed.indexer.configs[uniswapV4] != configs["uniswap-v4"] {
				t.Fatalf("Uniswap V4 indexer config was not wired")
			}
			found["uniswap-v4"] = true
		}
	}
	for protocolID := range configs {
		if !found[protocolID] {
			t.Errorf("protocol %q did not receive indexer configuration", protocolID)
		}
	}
}

func TestEngineDisabledChainsLeaveChainsAndProtocols(t *testing.T) {
	engine := NewEngineWithConfig(nil, nil, EngineConfig{DisabledChains: []ChainID{Polygon, Plasma}})
	chains := engine.Chains()
	if len(chains) != len(SupportedChainIDs)-2 {
		t.Fatalf("chains = %v, want every supported chain but Polygon and Plasma", chains)
	}
	for _, chainID := range chains {
		if chainID == Polygon || chainID == Plasma {
			t.Fatalf("chains = %v, still lists a disabled chain", chains)
		}
	}
	all := make(map[string]ProtocolInfo)
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		all[protocol.ID] = protocol
	}
	var aaveV2 []ChainID
	for _, protocol := range engine.Protocols() {
		if protocol.ID == "aave-v2" {
			aaveV2 = protocol.Chains
		}
		if len(protocol.Chains) == 0 {
			t.Fatalf("protocol %q is listed with no chains", protocol.ID)
		}
		for _, chainID := range protocol.Chains {
			if chainID == Polygon || chainID == Plasma {
				t.Fatalf("protocol %q still lists disabled chain %d", protocol.ID, chainID)
			}
		}
		if want := len(all[protocol.ID].Chains); len(protocol.Chains) > want {
			t.Fatalf("protocol %q gained chains: %v", protocol.ID, protocol.Chains)
		}
	}
	// Aave v2 is deployed on Ethereum, Polygon and Avalanche.
	if len(aaveV2) != 2 || aaveV2[0] != Ethereum || aaveV2[1] != Avalanche {
		t.Fatalf("aave-v2 chains = %v, want Ethereum and Avalanche", aaveV2)
	}
}

func TestEngineDropsProtocolWithEveryChainDisabled(t *testing.T) {
	// Kinza is deployed on BSC alone.
	engine := NewEngineWithConfig(nil, nil, EngineConfig{DisabledChains: []ChainID{BSC}})
	for _, protocol := range engine.Protocols() {
		if protocol.ID == "kinza" {
			t.Fatalf("kinza is listed with chains %v although BSC is disabled", protocol.Chains)
		}
	}
}

func TestEngineScanSkipsDisabledChains(t *testing.T) {
	// No RPC URLs: every chain that is dialled fails during setup with a chain error, so the
	// errors name exactly the chains the scan tried.
	engine := NewEngineWithConfig(nil, nil, EngineConfig{DisabledChains: []ChainID{Polygon, Plasma}})
	response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), ScanOptions{
		ProtocolIDs: map[string]struct{}{"aave-v3": {}},
		SkipPrices:  true,
	})
	if len(response.Errors) != len(SupportedChainIDs)-2 {
		t.Fatalf("errors = %+v, want one per enabled chain", response.Errors)
	}
	for _, scanError := range response.Errors {
		if scanError.ChainID == Polygon || scanError.ChainID == Plasma {
			t.Fatalf("unfiltered scan touched disabled chain: %+v", scanError)
		}
		if scanError.Message != "RPC URL is not configured" {
			t.Fatalf("error = %+v, want the unconfigured RPC", scanError)
		}
	}
	for _, protocol := range response.SupportedProtocols {
		for _, chainID := range protocol.Chains {
			if chainID == Polygon || chainID == Plasma {
				t.Fatalf("supported protocol %q lists disabled chain %d", protocol.ID, chainID)
			}
		}
	}

	response = engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), ScanOptions{
		ChainIDs: map[ChainID]struct{}{Polygon: {}},
	})
	if len(response.Errors) != 1 || response.Errors[0].Scope != "chain" ||
		response.Errors[0].ChainID != Polygon || response.Errors[0].Message != "chain is disabled" {
		t.Fatalf("errors = %+v, want the named disabled chain", response.Errors)
	}
}

func TestEngineRejectsUnsupportedDisabledChain(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an unsupported disabled chain did not panic")
		}
	}()
	NewEngineWithConfig(nil, nil, EngineConfig{DisabledChains: []ChainID{ChainID(999_999)}})
}
