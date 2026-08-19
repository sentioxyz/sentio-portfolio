package portfolio

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// Every gated component block must sit at or after its chain's activation block, and must be set
// wherever the contract really is deployed later. These numbers were measured by binary search
// over eth_getCode; if a config gains a chain without them, the scan silently regains the abort.
func TestEulerComponentBlocksAreConsistent(t *testing.T) {
	for chainID, chain := range eulerV2ChainConfigs {
		if chain.RewardEULBlock == 0 {
			t.Errorf("chain %d has no RewardEULBlock; rEUL postdates activation on every known chain", chainID)
			continue
		}
		if chain.RewardEULBlock < chain.ActivationBlock {
			t.Errorf("chain %d RewardEULBlock %d precedes activation %d",
				chainID, chain.RewardEULBlock, chain.ActivationBlock)
		}
		if chain.TrackingRewardsBlock != 0 && chain.TrackingRewardsBlock < chain.ActivationBlock {
			t.Errorf("chain %d TrackingRewardsBlock %d precedes activation %d",
				chainID, chain.TrackingRewardsBlock, chain.ActivationBlock)
		}
	}
}

// A gated read must not touch the RPC client at all. Passing a nil client makes that provable
// without a network: if the gate is removed, the call panics instead of returning cleanly.
func TestEulerGatedReadsNeverDialBeforeDeployment(t *testing.T) {
	adapter := newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{})
	ctx := context.Background()
	for chainID, chain := range eulerV2ChainConfigs {
		owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")

		block := BlockRef{ChainID: chainID, Number: chain.RewardEULBlock - 1, Fixed: true}
		vestings, err := adapter.readVestings(ctx, nil, block, owner, chain)
		if err != nil {
			t.Errorf("chain %d readVestings at %d: %v", chainID, block.Number, err)
		}
		if len(vestings) != 0 {
			t.Errorf("chain %d reported %d vestings before rEUL existed", chainID, len(vestings))
		}

		if chain.TrackingRewardsBlock > 0 {
			rewardBlock := BlockRef{ChainID: chainID, Number: chain.TrackingRewardsBlock - 1, Fixed: true}
			states := []eulerPositionState{{
				ref:    eulerPositionRef{Account: owner, Vault: chain.EVaultFactory, Kind: eulerEVault},
				shares: big.NewInt(1),
			}}
			rewards, rewardErr := adapter.readRewards(ctx, nil, rewardBlock, states, chain)
			if rewardErr != nil {
				t.Errorf("chain %d readRewards at %d: %v", chainID, rewardBlock.Number, rewardErr)
			}
			if len(rewards) != 0 {
				t.Errorf("chain %d reported %d rewards before the tracker existed", chainID, len(rewards))
			}
		}
	}
}

// The boundary is inclusive: the deployment block itself must read, not be skipped. Only the gate
// is asserted here — that the call is attempted — so a nil client is expected to panic, which is
// caught and treated as proof the read was reached.
func TestEulerGatedReadsRunFromTheDeploymentBlock(t *testing.T) {
	adapter := newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{})
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	for chainID, chain := range eulerV2ChainConfigs {
		reached := func() (reached bool) {
			defer func() {
				if recover() != nil {
					reached = true
				}
			}()
			block := BlockRef{ChainID: chainID, Number: chain.RewardEULBlock, Fixed: true}
			_, _ = adapter.readVestings(context.Background(), nil, block, owner, chain)
			return false
		}()
		if !reached {
			t.Errorf("chain %d skipped the rEUL read at its deployment block %d",
				chainID, chain.RewardEULBlock)
		}
	}
}

// Archive-RPC proof against the real chain: a fixed-block scan inside each chain's
// activation→rEUL interval must succeed. Before the gate, every one of these aborted with
// "rEUL locks: decode ...: abi: attempting to unmarshal an empty string".
func TestEulerHistoricalWindowLiveScan(t *testing.T) {
	if os.Getenv("PORTFOLIO_EXPANDED_PROTOCOLS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_EXPANDED_PROTOCOLS_LIVE_TEST=1 to run archive-RPC probes")
	}
	rpcEnvironments := map[ChainID]string{
		Ethereum: "PORTFOLIO_ETH_RPC_URL",
		BSC:      "PORTFOLIO_BSC_RPC_URL",
		Base:     "PORTFOLIO_BASE_RPC_URL",
		Arbitrum: "PORTFOLIO_ARB_RPC_URL",
	}
	owner := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	for chainID, chain := range eulerV2ChainConfigs {
		url := os.Getenv(rpcEnvironments[chainID])
		if url == "" {
			t.Logf("chain %d skipped: %s is unset", chainID, rpcEnvironments[chainID])
			continue
		}
		ctx := context.Background()
		client, err := DialRPC(ctx, chainID, url)
		if err != nil {
			t.Fatalf("chain %d dial: %v", chainID, err)
		}
		adapter := newEulerV2AdapterWithIndexer(fixedEulerLiveIndexer{})
		// activation, mid-interval, and the last block before rEUL exists
		for _, number := range []uint64{
			chain.ActivationBlock,
			chain.ActivationBlock + (chain.RewardEULBlock-chain.ActivationBlock)/2,
			chain.RewardEULBlock - 1,
		} {
			block, blockErr := client.BlockByNumber(ctx, number)
			if blockErr != nil {
				t.Fatalf("chain %d block %d: %v", chainID, number, blockErr)
			}
			block.Fixed = true
			if _, posErr := adapter.Positions(ctx, client, block, owner); posErr != nil {
				t.Errorf("chain %d block %d still fails inside the window: %v", chainID, number, posErr)
			}
		}
		client.Close()
	}
}
