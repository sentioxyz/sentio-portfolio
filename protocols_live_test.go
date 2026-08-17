package portfolio

import (
	"context"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type fixedMorphoLiveIndexer struct {
	refs morphoPositionRefs
}

func liveSentioIndexerConfig(t *testing.T, environmentPrefix string) SentioIndexerConfig {
	t.Helper()
	config := SentioIndexerConfig{
		GraphQLURL:       os.Getenv(environmentPrefix + "_GRAPHQL_URL"),
		StatusURL:        os.Getenv(environmentPrefix + "_STATUS_URL"),
		ProcessorVersion: os.Getenv(environmentPrefix + "_PROCESSOR_VERSION"),
	}
	if err := config.validate(); err != nil {
		t.Fatalf("%s runtime configuration: %v", environmentPrefix, err)
	}
	return config
}

func (i fixedMorphoLiveIndexer) PositionRefs(
	context.Context,
	*RPCClient,
	BlockRef,
	common.Address,
	morphoDeployment,
) (morphoPositionRefs, error) {
	return i.refs, nil
}

func runProtocolLiveProbe(
	t *testing.T,
	chainID ChainID,
	rpcEnvironmentVariable string,
	adapter Adapter,
	account string,
) []Group {
	t.Helper()
	endpoint := os.Getenv(rpcEnvironmentVariable)
	if endpoint == "" {
		t.Fatalf("%s is required", rpcEnvironmentVariable)
	}
	ctx := context.Background()
	client, err := DialRPC(ctx, chainID, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	block, err := client.LatestBlock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := adapter.Positions(ctx, client, block, common.HexToAddress(account))
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

func TestNewProtocolsLiveNonZeroPositions(t *testing.T) {
	if os.Getenv("PORTFOLIO_NEW_PROTOCOLS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_NEW_PROTOCOLS_LIVE_TEST=1 to run archive-RPC probes")
	}
	tests := []struct {
		name        string
		chainID     ChainID
		rpcEnv      string
		adapter     Adapter
		account     string
		wantToken   string
		wantGroupID string
		wantRoles   map[string]int
	}{
		{
			name:      "Beefy BSC single-asset vault",
			chainID:   BSC,
			rpcEnv:    "PORTFOLIO_BSC_RPC_URL",
			adapter:   newBeefyAdapter(),
			account:   "0x1eca251dd41ae2f0671758d265b44ca5a9ef7bcd",
			wantToken: "WBNB",
		},
		{
			name:        "StakeWise Ethereum verified vault",
			chainID:     Ethereum,
			rpcEnv:      "PORTFOLIO_ETH_RPC_URL",
			adapter:     newStakeWiseAdapter(),
			account:     "0x721cbfcd682a49886207716098fdd4759ba39493",
			wantToken:   "ETH",
			wantGroupID: "0xe6d8d8ac54461b1c5ed15740eee322043f696c08",
		},
		{
			name:    "Lista BSC Moolah",
			chainID: BSC,
			rpcEnv:  "PORTFOLIO_BSC_RPC_URL",
			adapter: newListaAdapter(),
			account: "0x0458edb2418e83157f470f38c9875df10fc731e8",
		},
		{
			name:      "Vesper Ethereum pool and rewards",
			chainID:   Ethereum,
			rpcEnv:    "PORTFOLIO_ETH_RPC_URL",
			adapter:   newVesperAdapter(),
			account:   "0x45ff0e3bd649a1d4b78982c8eeae0839aaa7f84f",
			wantToken: "VSP",
		},
		{
			name:      "Spark Base Savings",
			chainID:   Base,
			rpcEnv:    "PORTFOLIO_BASE_RPC_URL",
			adapter:   newAaveAdapter("spark", "Spark", nil, sparkSavingsVaults),
			account:   "0x93442c29746ed5a8de6a781f55eec0266d289ad4",
			wantToken: "USDC",
		},
		{
			name:      "Spark Arbitrum Savings",
			chainID:   Arbitrum,
			rpcEnv:    "PORTFOLIO_ARB_RPC_URL",
			adapter:   newAaveAdapter("spark", "Spark", nil, sparkSavingsVaults),
			account:   "0x11bdf98925a04f9338989c4dd065b2e1b20dc03d",
			wantToken: "USDC",
		},
		{
			name:      "Stader BSC BNBx",
			chainID:   BSC,
			rpcEnv:    "PORTFOLIO_BSC_RPC_URL",
			adapter:   newStaderAdapter(),
			account:   "0x76a820fc831b859a20b511b5a17f8a73bc0874ea",
			wantToken: "BNBx",
		},
		{
			name:      "Yearn V3 Base",
			chainID:   Base,
			rpcEnv:    "PORTFOLIO_BASE_RPC_URL",
			adapter:   newYearnV3Adapter(),
			account:   "0x26c60b38fe7e55d699c8102c18cc5d7152e0762e",
			wantToken: "USDC",
		},
		{
			name:      "Yearn V3 Arbitrum",
			chainID:   Arbitrum,
			rpcEnv:    "PORTFOLIO_ARB_RPC_URL",
			adapter:   newYearnV3Adapter(),
			account:   "0x8ee796309494a10b4170f8912613ee78c75a3430",
			wantToken: "USDe",
		},
		{
			name:      "Curve LlamaLend Arbitrum",
			chainID:   Arbitrum,
			rpcEnv:    "PORTFOLIO_ARB_RPC_URL",
			adapter:   newCurveLendingAdapter(),
			account:   "0xe8b191f29f17d3ced5118a16192eeace66d3d00f",
			wantToken: "crvUSD",
		},
		{
			name:    "Morpho Blue Ethereum core",
			chainID: Ethereum,
			rpcEnv:  "PORTFOLIO_ETH_RPC_URL",
			adapter: newMorphoAdapterWithIndexer(fixedMorphoLiveIndexer{refs: morphoPositionRefs{
				MarketIDs: []common.Hash{common.HexToHash("0x3a85e619751152991742810df6ec69ce473daef99e28a64ab2340d7b7ccfee49")},
			}}),
			account:   "0x90c69eba57d2ae2eeb8a1361fc9b88522de24867",
			wantToken: "USDC",
		},
		{
			name:    "Fluid Ethereum vault",
			chainID: Ethereum,
			rpcEnv:  "PORTFOLIO_ETH_RPC_URL",
			adapter: newFluidAdapter(),
			account: "0x77c38a0049822634a1a379db10f7b03367724fec",
		},
		{
			name:      "Fluid Ethereum fToken",
			chainID:   Ethereum,
			rpcEnv:    "PORTFOLIO_ETH_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0x273da948aca9261043fbdb2a857bc255ecc29012",
			wantToken: "USDC",
		},
		{
			name:      "Fluid Ethereum Lite vault",
			chainID:   Ethereum,
			rpcEnv:    "PORTFOLIO_ETH_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0x196a5888d5603a4363cfaf9d75abf1bc961cd37d",
			wantToken: "USDC",
		},
		{
			name:      "Fluid Ethereum staking rewards",
			chainID:   Ethereum,
			rpcEnv:    "PORTFOLIO_ETH_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0x83f51629e1533f372e0ebff4e65ee99ad509b91c",
			wantRoles: map[string]int{"staked-supply": 1, "reward": 1},
		},
		{
			name:      "Fluid Ethereum smart collateral and debt vault",
			chainID:   Ethereum,
			rpcEnv:    "PORTFOLIO_ETH_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0xff3cdd28a0d17998e09de3d9d07945e6fc706b51",
			wantRoles: map[string]int{"smart-collateral": 2, "smart-debt": 2},
		},
		{
			name:    "Fluid BSC vault",
			chainID: BSC,
			rpcEnv:  "PORTFOLIO_BSC_RPC_URL",
			adapter: newFluidAdapter(),
			account: "0xf0a6e66b4396a70ee0620064da847821bee70731",
		},
		{
			name:    "Fluid Base vault",
			chainID: Base,
			rpcEnv:  "PORTFOLIO_BASE_RPC_URL",
			adapter: newFluidAdapter(),
			account: "0xdfaff7b83125efbc356c5f980a9010e7f28ba50a",
		},
		{
			name:      "Fluid Base fToken",
			chainID:   Base,
			rpcEnv:    "PORTFOLIO_BASE_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0x3548cdd5222a415fb35ac65cfa9dde7f4a210efd",
			wantToken: "WETH",
		},
		{
			name:    "Fluid Arbitrum vault",
			chainID: Arbitrum,
			rpcEnv:  "PORTFOLIO_ARB_RPC_URL",
			adapter: newFluidAdapter(),
			account: "0x5e1cacdad47d95b932757c7961e23c004977aa33",
		},
		{
			name:      "Fluid Arbitrum fToken",
			chainID:   Arbitrum,
			rpcEnv:    "PORTFOLIO_ARB_RPC_URL",
			adapter:   newFluidAdapter(),
			account:   "0xf5211d07b36a356fcc49684728121be6979433e3",
			wantToken: "USDC",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			groups := runProtocolLiveProbe(t, test.chainID, test.rpcEnv, test.adapter, test.account)
			if len(groups) == 0 {
				t.Fatal("adapter returned no position groups")
			}
			matchedToken := test.wantToken == ""
			matchedGroup := test.wantGroupID == ""
			roleCounts := make(map[string]int)
			for _, group := range groups {
				if group.ID == test.wantGroupID {
					matchedGroup = true
				}
				for _, component := range group.Components {
					if component.Token.Symbol == test.wantToken && component.AmountRaw != "0" {
						matchedToken = true
					}
					if role, ok := component.Metadata["role"].(string); ok {
						roleCounts[role]++
					}
				}
			}
			if !matchedGroup {
				t.Fatalf("position groups do not contain %s: %+v", test.wantGroupID, groups)
			}
			if !matchedToken {
				t.Fatalf("position groups do not contain a non-zero %s component: %+v", test.wantToken, groups)
			}
			for role, want := range test.wantRoles {
				if roleCounts[role] != want {
					t.Fatalf("role %s count = %d, want %d: %+v", role, roleCounts[role], want, groups)
				}
			}
		})
	}
}
