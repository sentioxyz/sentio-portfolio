package portfolio

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

type fixedEulerLiveIndexer struct {
	refs []eulerPositionRef
}

func (i fixedEulerLiveIndexer) PositionRefs(
	context.Context,
	*RPCClient,
	BlockRef,
	common.Address,
) ([]eulerPositionRef, error) {
	return append([]eulerPositionRef(nil), i.refs...), nil
}

func TestExpandedProtocolsLiveNonZeroPositions(t *testing.T) {
	if os.Getenv("PORTFOLIO_EXPANDED_PROTOCOLS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_EXPANDED_PROTOCOLS_LIVE_TEST=1 to run archive-RPC probes")
	}
	tests := []struct {
		name          string
		chainID       ChainID
		rpcEnv        string
		adapter       Adapter
		account       string
		wantToken     string
		wantGroupPart string
		partialError  bool
	}{
		{
			name: "Ether.fi Ethereum weETH", chainID: Ethereum, rpcEnv: "PORTFOLIO_ETH_RPC_URL",
			adapter: newEtherfiAdapter(SentioIndexerConfig{}),
			account: "0x3cfd5c0d4acaa8faee335842e4f31159fc76b008", wantToken: "ETH",
			partialError: true,
		},
		{
			name: "Ether.fi Ethereum sETHFI accountant conversion", chainID: Ethereum,
			rpcEnv: "PORTFOLIO_ETH_RPC_URL", adapter: newEtherfiAdapter(SentioIndexerConfig{}),
			account: "0x795a4ed1a7e726280830d7130a43a830bc8225d4", wantToken: "ETHFI",
			wantGroupPart: "sethfi", partialError: true,
		},
		{
			name: "Frax Ether sfrxETH", chainID: Ethereum, rpcEnv: "PORTFOLIO_ETH_RPC_URL",
			adapter: newFraxEtherAdapter(SentioIndexerConfig{}),
			account: "0xc396e325afef0e49d7712a223208c8440c1b9afe", wantToken: "frxETH",
			partialError: true,
		},
		{
			name: "Renzo Ethereum ezETH", chainID: Ethereum, rpcEnv: "PORTFOLIO_ETH_RPC_URL",
			adapter: newRenzoAdapter(SentioIndexerConfig{}),
			account: "0xf047ab4c75cebf0eb9ed34ae2c186f3611aeafa6", wantToken: "ETH",
			partialError: true,
		},
		{
			// The asBNB minter converts through slisBNB and holds slisBNB, so that is what the
			// adapter reports; it deliberately does not rewrite the token to BNB.
			name: "Aster BSC asBNB", chainID: BSC, rpcEnv: "PORTFOLIO_BSC_RPC_URL",
			adapter: newAsterAdapter(SentioIndexerConfig{}),
			account: "0xb6e80081610f99757cc910fb31b0a3311f6c3a1c", wantToken: "slisBNB",
			partialError: true,
		},
		{
			name: "f(x) Protocol legacy rebalance pool", chainID: Ethereum,
			rpcEnv: "PORTFOLIO_ETH_RPC_URL", adapter: newFxProtocolAdapter(),
			account:       "0x6beac4c266db8017f30800e1af12ceb7ded75be0",
			wantGroupPart: "0xc6dee5913e010895f3702bc43a40d661b13a40bd",
		},
		{
			name: "f(x) Protocol shareable rebalance pool", chainID: Ethereum,
			rpcEnv: "PORTFOLIO_ETH_RPC_URL", adapter: newFxProtocolAdapter(),
			account:       "0x4a036ab673722468a8e1fcc0f74a2dd5914fd1c1",
			wantGroupPart: "0xf58c499417e36714e99803cb135f507a95ae7169",
		},
		{
			name: "Euler V2 subaccount", chainID: Ethereum, rpcEnv: "PORTFOLIO_ETH_RPC_URL",
			adapter: newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{refs: []eulerPositionRef{{
				Account: common.HexToAddress("0xcf7e7c56614b6e22b0043895a37e3858971ec905"),
				Vault:   common.HexToAddress("0xe846ca062ab869b66ae8dcd811973f628ba82eaf"),
				Kind:    eulerEVault,
			}}}),
			account:       "0xcf7e7c56614b6e22b0043895a37e3858971ec904",
			wantGroupPart: "euler-v2:lending:",
		},
		{
			name: "Euler V2 BSC subaccount", chainID: BSC, rpcEnv: "PORTFOLIO_BSC_RPC_URL",
			adapter: newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{refs: []eulerPositionRef{{
				Account: common.HexToAddress("0x08e4164c948bafb3514bcc639dba6ce7977d7dc7"),
				Vault:   common.HexToAddress("0x0b126af75849d6a517cc98c6f6346ab41629ad87"),
				Kind:    eulerEVault,
			}}}),
			account:       "0x08e4164c948bafb3514bcc639dba6ce7977d7dc6",
			wantToken:     "INTCon",
			wantGroupPart: "euler-v2:lending:",
		},
		{
			name: "Euler V2 Base primary account", chainID: Base, rpcEnv: "PORTFOLIO_BASE_RPC_URL",
			adapter: newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{refs: []eulerPositionRef{{
				Account: common.HexToAddress("0x8bf41ad2b816f7c220b22f4bcd63fc2a35ab4247"),
				Vault:   common.HexToAddress("0xeaa709fdb7cccfbbf5185febf183f0138cde5983"),
				Kind:    eulerEVault,
			}}}),
			account:       "0x8bf41ad2b816f7c220b22f4bcd63fc2a35ab4247",
			wantToken:     "USDC",
			wantGroupPart: "euler-v2:lending:",
		},
		{
			name: "Euler V2 Arbitrum primary account", chainID: Arbitrum,
			rpcEnv: "PORTFOLIO_ARB_RPC_URL",
			adapter: newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{refs: []eulerPositionRef{{
				Account: common.HexToAddress("0x4de00423607130bec8d3ea1f8e2155c008a893a5"),
				Vault:   common.HexToAddress("0xa8616e4d9f3f0aa01aff1d7c3b66249f8a5f1a58"),
				Kind:    eulerEVault,
			}}}),
			account:       "0x4de00423607130bec8d3ea1f8e2155c008a893a5",
			wantGroupPart: "euler-v2:lending:",
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
			block, err := client.LatestBlock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			groups, positionErr := test.adapter.Positions(
				ctx, client, block, common.HexToAddress(test.account),
			)
			if test.partialError {
				if positionErr == nil || !strings.Contains(positionErr.Error(), "not configured") {
					t.Fatalf("partial indexer error = %v, want explicit configuration error", positionErr)
				}
			} else if positionErr != nil {
				t.Fatal(positionErr)
			}
			if len(groups) == 0 {
				t.Fatal("adapter returned no verified position groups")
			}
			matchedToken := test.wantToken == ""
			matchedGroup := test.wantGroupPart == ""
			for _, group := range groups {
				if strings.Contains(strings.ToLower(group.ID), test.wantGroupPart) {
					matchedGroup = true
				}
				for _, component := range group.Components {
					if component.Token.Symbol == test.wantToken && component.AmountRaw != "0" {
						matchedToken = true
					}
				}
			}
			if !matchedToken || !matchedGroup {
				t.Fatalf("position mismatch: groups=%+v", groups)
			}
		})
	}
}
