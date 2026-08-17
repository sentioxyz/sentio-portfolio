package portfolio

import (
	"context"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func liveUniswapAmounts(groups []Group, groupID string) map[string]string {
	result := make(map[string]string)
	for _, group := range groups {
		if group.ID != groupID {
			continue
		}
		for _, component := range group.Components {
			result[component.Kind+":"+component.Token.Symbol] = component.AmountRaw
		}
	}
	return result
}

func TestUniswapLiveFixedBlockDeBankVectors(t *testing.T) {
	if os.Getenv("PORTFOLIO_UNISWAP_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_UNISWAP_LIVE_TEST=1 to run archive-RPC and Sentio checks")
	}
	if os.Getenv("PORTFOLIO_SENTIO_API_KEY") == "" {
		t.Fatal("PORTFOLIO_SENTIO_API_KEY is required")
	}
	v3Config := liveSentioIndexerConfig(t, "PORTFOLIO_UNISWAP_V3_INDEXER")
	v4Config := liveSentioIndexerConfig(t, "PORTFOLIO_UNISWAP_V4_INDEXER")
	tests := []struct {
		name    string
		chainID ChainID
		rpcEnv  string
		block   uint64
		account string
		adapter func(*uniswapIndexer) Adapter
		groupID string
		want    map[string]string
	}{
		{
			name:    "v3 bsc",
			chainID: BSC,
			rpcEnv:  "PORTFOLIO_BSC_RPC_URL",
			block:   112_468_168,
			account: "0x8E0a14666a68B7AeB604eE81C6A32fE6dEb376C1",
			adapter: func(indexer *uniswapIndexer) Adapter {
				return &UniswapV3Adapter{adapterBase: adapterBase{info: ProtocolInfo{
					ID: "uniswap-v3", Name: "Uniswap V3", Chains: SupportedChainIDs,
				}}, indexer: indexer}
			},
			groupID: "nft:2532058",
			want: map[string]string{
				"asset:人生K线":  "8525113240580340167854",
				"reward:人生K线": "543970535582508825118",
				"reward:USDT": "493201150371537412",
			},
		},
		{
			name:    "v3 ethereum",
			chainID: Ethereum,
			rpcEnv:  "PORTFOLIO_ETH_RPC_URL",
			block:   25_629_960,
			account: "0x93E78A9c0eE1Bd46a9f175536d420BF5a0B338F9",
			adapter: func(indexer *uniswapIndexer) Adapter {
				return &UniswapV3Adapter{adapterBase: adapterBase{info: ProtocolInfo{
					ID: "uniswap-v3", Name: "Uniswap V3", Chains: SupportedChainIDs,
				}}, indexer: indexer}
			},
			groupID: "nft:1341323",
			want: map[string]string{
				"asset:USDC":  "825709836",
				"asset:WETH":  "5954988183713713356",
				"reward:USDC": "240998",
				"reward:WETH": "107338971758860",
			},
		},
		{
			name:    "v4 bsc",
			chainID: BSC,
			rpcEnv:  "PORTFOLIO_BSC_RPC_URL",
			block:   112_476_030,
			account: "0xcF87B24510f91D73e508122ad08BEeA7A976EC62",
			adapter: func(indexer *uniswapIndexer) Adapter {
				return &UniswapV4Adapter{adapterBase: adapterBase{info: ProtocolInfo{
					ID: "uniswap-v4", Name: "Uniswap V4", Chains: SupportedChainIDs,
				}}, indexer: indexer}
			},
			groupID: "nft:954043",
			want: map[string]string{
				"asset:USDT":  "9612289693477912210",
				"asset:USDC":  "10460314654014603890",
				"reward:USDC": "494635722357",
			},
		},
		{
			name:    "v4 base",
			chainID: Base,
			rpcEnv:  "PORTFOLIO_BASE_RPC_URL",
			block:   49_220_530,
			account: "0xeD63b78c56B948b9A21565e4c2940F316f70ecE2",
			adapter: func(indexer *uniswapIndexer) Adapter {
				return &UniswapV4Adapter{adapterBase: adapterBase{info: ProtocolInfo{
					ID: "uniswap-v4", Name: "Uniswap V4", Chains: SupportedChainIDs,
				}}, indexer: indexer}
			},
			groupID: "nft:2832648",
			want: map[string]string{
				"asset:WETH":  "42939842008681350",
				"asset:USDC":  "921213426",
				"reward:WETH": "1385571819277831",
				"reward:USDC": "2852109",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := os.Getenv(test.rpcEnv)
			if endpoint == "" {
				t.Fatalf("%s is required", test.rpcEnv)
			}
			ctx := context.Background()
			client, err := DialRPC(ctx, test.chainID, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			block, err := client.BlockByNumber(ctx, test.block)
			if err != nil {
				t.Fatal(err)
			}
			block.Fixed = true
			groups, err := test.adapter(newUniswapIndexer(v3Config, v4Config)).Positions(
				ctx,
				client,
				block,
				common.HexToAddress(test.account),
			)
			if err != nil {
				t.Fatal(err)
			}
			got := liveUniswapAmounts(groups, test.groupID)
			for key, want := range test.want {
				if got[key] != want {
					t.Errorf("%s = %q, want %q (all amounts: %v)", key, got[key], want, got)
				}
			}
		})
	}

	t.Run("fresh empty-owner lookup on every chain", func(t *testing.T) {
		for _, chain := range []struct {
			id     ChainID
			rpcEnv string
		}{
			{id: Ethereum, rpcEnv: "PORTFOLIO_ETH_RPC_URL"},
			{id: BSC, rpcEnv: "PORTFOLIO_BSC_RPC_URL"},
			{id: Base, rpcEnv: "PORTFOLIO_BASE_RPC_URL"},
			{id: Arbitrum, rpcEnv: "PORTFOLIO_ARBITRUM_RPC_URL"},
		} {
			endpoint := os.Getenv(chain.rpcEnv)
			if endpoint == "" {
				t.Fatalf("%s is required", chain.rpcEnv)
			}
			ctx := context.Background()
			client, err := DialRPC(ctx, chain.id, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			block, err := client.LatestBlock(ctx)
			if err != nil {
				client.Close()
				t.Fatal(err)
			}
			for _, adapter := range newUniswapAdapters(v3Config, v4Config) {
				groups, positionErr := adapter.Positions(ctx, client, block, common.Address{})
				if positionErr != nil {
					client.Close()
					t.Fatalf("chain %d %s: %v", chain.id, adapter.Info().ID, positionErr)
				}
				if len(groups) != 0 {
					client.Close()
					t.Fatalf("chain %d %s zero owner returned %d groups", chain.id, adapter.Info().ID, len(groups))
				}
			}
			client.Close()
		}
	})
}
