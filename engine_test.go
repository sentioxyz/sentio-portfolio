package portfolio

import "testing"

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
	for _, required := range []string{"beefy", "fluid", "lista", "morpho-blue", "stakewise"} {
		if _, exists := seen[required]; !exists {
			t.Errorf("protocol %q is not registered", required)
		}
	}
}

func TestEngineWiresRuntimeSentioIndexerConfig(t *testing.T) {
	configs := map[string]SentioIndexerConfig{
		"meth-protocol": testSentioIndexerConfig("meth"),
		"morpho-blue":   testSentioIndexerConfig("morpho"),
		"uniswap-v3":    testSentioIndexerConfig("uniswap-v3"),
		"uniswap-v4":    testSentioIndexerConfig("uniswap-v4"),
	}
	engine := NewEngineWithConfig(nil, nil, EngineConfig{SentioIndexers: configs})
	found := make(map[string]bool)
	for _, adapter := range engine.adapters {
		switch typed := adapter.(type) {
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
