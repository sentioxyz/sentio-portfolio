package portfolio

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

type liveAvailabilityAdapterSpy struct {
	Adapter
	calls atomic.Int64
}

func (s *liveAvailabilityAdapterSpy) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	s.calls.Add(1)
	return s.Adapter.Positions(ctx, client, block, account)
}

func installLiveAvailabilityAdapterSpy(
	t *testing.T,
	engine *Engine,
	protocolID string,
) *liveAvailabilityAdapterSpy {
	t.Helper()
	for index, adapter := range engine.adapters {
		if adapter.Info().ID != protocolID {
			continue
		}
		spy := &liveAvailabilityAdapterSpy{Adapter: adapter}
		engine.adapters[index] = spy
		registration, exists := engine.registrations[protocolID]
		if !exists {
			t.Fatalf("protocol %q has no validated registration", protocolID)
		}
		registration.Adapter = spy
		engine.registrations[protocolID] = registration
		return spy
	}
	t.Fatalf("protocol %q has no adapter", protocolID)
	return nil
}

type liveAvailabilityKey struct {
	protocolID string
	chainID    ChainID
}

// These adapters are the ones whose previously unconditional root/component reads
// motivated this migration. The complete 48-protocol/95-chain registry set is locked by
// TestProtocolAvailabilityMatchesVerifiedBoundaries; repeating every adapter call here
// would also require external indexer services and roughly two hundred archive scans.
// Each selected probe wraps the real adapter, so a clean response cannot be mistaken for
// a central skip.
var liveAdapterBoundaryProbes = map[liveAvailabilityKey]struct{}{
	{protocolID: "compound-v2", chainID: Ethereum}:       {},
	{protocolID: "moonwell", chainID: Base}:              {},
	{protocolID: "flux-finance", chainID: Ethereum}:      {},
	{protocolID: "sonne", chainID: Base}:                 {},
	{protocolID: "lodestar", chainID: Arbitrum}:          {},
	{protocolID: "venus", chainID: Ethereum}:             {},
	{protocolID: "venus", chainID: BSC}:                  {},
	{protocolID: "venus", chainID: Base}:                 {},
	{protocolID: "venus", chainID: Arbitrum}:             {},
	{protocolID: "fraxlend", chainID: Ethereum}:          {},
	{protocolID: "liquid-collective", chainID: Ethereum}: {},
	{protocolID: "makerdao", chainID: Ethereum}:          {},
	{protocolID: "rocketpool", chainID: Ethereum}:        {},
}

// TestProtocolAvailabilityBoundariesLive verifies the archive-RPC evidence for
// adapters that previously attempted calls before their contracts or mandatory
// views existed. It is opt-in because it requires four external archive RPCs.
func TestProtocolAvailabilityBoundariesLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEPLOYMENT_WINDOWS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEPLOYMENT_WINDOWS_LIVE_TEST=1 to run archive-RPC boundary probes")
	}
	rpcURLs := map[ChainID]string{
		Ethereum: os.Getenv("PORTFOLIO_ETH_RPC_URL"),
		BSC:      os.Getenv("PORTFOLIO_BSC_RPC_URL"),
		Base:     os.Getenv("PORTFOLIO_BASE_RPC_URL"),
		Arbitrum: os.Getenv("PORTFOLIO_ARB_RPC_URL"),
	}
	for chainID, endpoint := range rpcURLs {
		if endpoint == "" {
			t.Fatalf("archive RPC for chain %d is required", chainID)
		}
	}

	account := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	t.Run("all-protocols/pre-Instadapp", func(t *testing.T) {
		engine := NewEngine(rpcURLs, nil)
		spies := make(map[string]*liveAvailabilityAdapterSpy)
		for _, adapter := range engine.adapters {
			protocolID := adapter.Info().ID
			if protocolID != walletProtocolID {
				spies[protocolID] = installLiveAvailabilityAdapterSpy(t, engine, protocolID)
			}
		}
		const block = uint64(9_747_257)
		response := engine.ScanWithOptions(
			context.Background(),
			account,
			ScanOptions{
				ChainIDs:    map[ChainID]struct{}{Ethereum: {}},
				BlockNumber: map[ChainID]uint64{Ethereum: block},
				SkipPrices:  true,
			},
		)
		if len(response.Errors) != 0 {
			t.Fatalf("block %d errors: %+v", block, response.Errors)
		}
		for protocolID, spy := range spies {
			if got := spy.calls.Load(); got != 0 {
				t.Errorf("protocol %q Positions calls = %d before its Ethereum availability, want 0", protocolID, got)
			}
		}
	})
	for _, expected := range verifiedProtocolAvailabilityWindows {
		expected := expected
		t.Run(fmt.Sprintf("%s/chain-%d", expected.protocolID, expected.chainID), func(t *testing.T) {
			engine := NewEngine(rpcURLs, nil)
			registration, exists := engine.registrations[expected.protocolID]
			if !exists {
				t.Fatalf("protocol %q has no validated registration", expected.protocolID)
			}
			if expected.fromGenesis {
				if !registration.ActiveAt(expected.chainID, 0) {
					t.Fatal("genesis registration is inactive at block 0")
				}
				return
			}
			if registration.ActiveAt(expected.chainID, expected.activationBlock-1) {
				t.Fatalf("registration is active before expected block %d", expected.activationBlock)
			}
			if !registration.ActiveAt(expected.chainID, expected.activationBlock) {
				t.Fatalf("registration is inactive at expected block %d", expected.activationBlock)
			}

			key := liveAvailabilityKey{protocolID: expected.protocolID, chainID: expected.chainID}
			if _, probe := liveAdapterBoundaryProbes[key]; !probe {
				t.Log("registry boundary checked; adapter archive execution is outside the focused migration probe set")
				return
			}
			spy := installLiveAvailabilityAdapterSpy(t, engine, expected.protocolID)
			before := engine.ScanWithOptions(
				context.Background(),
				account,
				ScanOptions{
					ProtocolIDs: map[string]struct{}{expected.protocolID: {}},
					ChainIDs:    map[ChainID]struct{}{expected.chainID: {}},
					BlockNumber: map[ChainID]uint64{expected.chainID: expected.activationBlock - 1},
					SkipPrices:  true,
				},
			)
			if len(before.Errors) != 0 {
				t.Fatalf("block %d errors: %+v", expected.activationBlock-1, before.Errors)
			}
			if got := spy.calls.Load(); got != 0 {
				t.Fatalf("Positions calls before activation = %d, want 0", got)
			}

			at := engine.ScanWithOptions(
				context.Background(),
				account,
				ScanOptions{
					ProtocolIDs: map[string]struct{}{expected.protocolID: {}},
					ChainIDs:    map[ChainID]struct{}{expected.chainID: {}},
					BlockNumber: map[ChainID]uint64{expected.chainID: expected.activationBlock},
					SkipPrices:  true,
				},
			)
			if len(at.Errors) != 0 {
				t.Fatalf("block %d errors: %+v", expected.activationBlock, at.Errors)
			}
			if got := spy.calls.Load(); got == 0 {
				t.Fatal("Positions was not called at activation")
			}
		})
	}
}

// TestBeefyManifestDeploymentBoundariesLive replays the archive evidence stored
// in the v2 manifest. It is separate from the focused adapter probe because a
// manifest refresh should be able to verify every vault without making ordinary
// unit-test CI depend on an external RPC.
func TestBeefyManifestDeploymentBoundariesLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_BEEFY_DEPLOYMENT_WINDOWS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_BEEFY_DEPLOYMENT_WINDOWS_LIVE_TEST=1 to verify all Beefy vault boundaries")
	}
	rpcEnvironment := map[ChainID]string{
		Ethereum: "PORTFOLIO_ETH_RPC_URL",
		BSC:      "PORTFOLIO_BSC_RPC_URL",
		Base:     "PORTFOLIO_BASE_RPC_URL",
		Arbitrum: "PORTFOLIO_ARB_RPC_URL",
	}
	balanceCall, err := beefyVaultABI.Pack("balanceOf", common.Address{})
	if err != nil {
		t.Fatal(err)
	}
	priceCall, err := beefyVaultABI.Pack("getPricePerFullShare")
	if err != nil {
		t.Fatal(err)
	}

	for _, chainID := range SupportedChainIDs {
		chainID := chainID
		t.Run(fmt.Sprintf("chain-%d", chainID), func(t *testing.T) {
			endpoint := os.Getenv(rpcEnvironment[chainID])
			if endpoint == "" {
				t.Fatalf("archive RPC for chain %d is required", chainID)
			}
			client, err := DialRPC(context.Background(), chainID, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)

			vaults := make([]beefyManifestVault, 0)
			for _, vault := range beefyDeployments.Vaults {
				if vault.ChainID == chainID {
					vaults = append(vaults, vault)
				}
			}
			const batchSize = 16
			for start := 0; start < len(vaults); start += batchSize {
				end := min(start+batchSize, len(vaults))
				batchVaults := vaults[start:end]
				beforeCode := make([]hexutil.Bytes, len(batchVaults))
				atCode := make([]hexutil.Bytes, len(batchVaults))
				beforeBalances := make([]hexutil.Bytes, len(batchVaults))
				balances := make([]hexutil.Bytes, len(batchVaults))
				beforePrices := make([]hexutil.Bytes, len(batchVaults))
				prices := make([]hexutil.Bytes, len(batchVaults))

				beforeCalls := make([]rpc.BatchElem, len(batchVaults))
				atCalls := make([]rpc.BatchElem, len(batchVaults))
				beforeBalanceCalls := make([]rpc.BatchElem, len(batchVaults))
				balanceCalls := make([]rpc.BatchElem, len(batchVaults))
				beforePriceCalls := make([]rpc.BatchElem, len(batchVaults))
				priceCalls := make([]rpc.BatchElem, len(batchVaults))
				for index, vault := range batchVaults {
					beforeCalls[index] = rpc.BatchElem{
						Method: "eth_getCode",
						Args:   []any{vault.Vault, hexutil.EncodeUint64(vault.ActivationBlock - 1)},
						Result: &beforeCode[index],
					}
					atCalls[index] = rpc.BatchElem{
						Method: "eth_getCode",
						Args:   []any{vault.Vault, hexutil.EncodeUint64(vault.ActivationBlock)},
						Result: &atCode[index],
					}
					beforeBalanceCalls[index] = rpc.BatchElem{
						Method: "eth_call",
						Args: []any{
							map[string]any{"to": vault.Vault, "data": hexutil.Bytes(balanceCall)},
							hexutil.EncodeUint64(vault.ActivationBlock - 1),
						},
						Result: &beforeBalances[index],
					}
					balanceCalls[index] = rpc.BatchElem{
						Method: "eth_call",
						Args: []any{
							map[string]any{"to": vault.Vault, "data": hexutil.Bytes(balanceCall)},
							hexutil.EncodeUint64(vault.ActivationBlock),
						},
						Result: &balances[index],
					}
					beforePriceCalls[index] = rpc.BatchElem{
						Method: "eth_call",
						Args: []any{
							map[string]any{"to": vault.Vault, "data": hexutil.Bytes(priceCall)},
							hexutil.EncodeUint64(vault.ActivationBlock - 1),
						},
						Result: &beforePrices[index],
					}
					priceCalls[index] = rpc.BatchElem{
						Method: "eth_call",
						Args: []any{
							map[string]any{"to": vault.Vault, "data": hexutil.Bytes(priceCall)},
							hexutil.EncodeUint64(vault.ActivationBlock),
						},
						Result: &prices[index],
					}
				}
				if err := client.batchCall(context.Background(), beforeCalls); err != nil {
					t.Fatalf("vaults %d-%d pre-activation code: %v", start, end-1, err)
				}
				if err := client.batchCall(context.Background(), atCalls); err != nil {
					t.Fatalf("vaults %d-%d activation code: %v", start, end-1, err)
				}
				if err := client.batchCallTransport(context.Background(), beforeBalanceCalls); err != nil {
					t.Fatalf("vaults %d-%d pre-activation balanceOf: %v", start, end-1, err)
				}
				if err := client.batchCall(context.Background(), balanceCalls); err != nil {
					t.Fatalf("vaults %d-%d activation balanceOf: %v", start, end-1, err)
				}
				if err := client.batchCallTransport(context.Background(), beforePriceCalls); err != nil {
					t.Fatalf("vaults %d-%d pre-activation getPricePerFullShare: %v", start, end-1, err)
				}
				if err := client.batchCallTransport(context.Background(), priceCalls); err != nil {
					t.Fatalf("vaults %d-%d activation getPricePerFullShare: %v", start, end-1, err)
				}
				for index, vault := range batchVaults {
					if len(atCode[index]) == 0 {
						t.Errorf("vault %q has no code at activation block %d", vault.ID, vault.ActivationBlock)
					}
					beforePricePositive := false
					if beforePriceCalls[index].Error == nil {
						beforeValues, unpackErr := beefyVaultABI.Unpack(
							"getPricePerFullShare",
							beforePrices[index],
						)
						if unpackErr == nil {
							beforePrice, valueErr := BigIntAt(beforeValues, 0)
							beforePricePositive = valueErr == nil && beforePrice.Sign() > 0
						}
					}
					if len(beforeCode[index]) > 0 &&
						beforeBalanceCalls[index].Error == nil && len(beforeBalances[index]) == 32 &&
						beforePricePositive {
						t.Errorf("vault %q is already adapter-safe before activation block %d", vault.ID, vault.ActivationBlock)
					}
					if len(balances[index]) != 32 {
						t.Errorf(
							"vault %q balanceOf returned %d bytes at activation block %d, want 32",
							vault.ID,
							len(balances[index]),
							vault.ActivationBlock,
						)
					}
					if priceCalls[index].Error != nil {
						t.Errorf(
							"vault %q getPricePerFullShare failed at activation block %d: %v",
							vault.ID,
							vault.ActivationBlock,
							priceCalls[index].Error,
						)
						continue
					}
					priceValues, err := beefyVaultABI.Unpack("getPricePerFullShare", prices[index])
					if err != nil {
						t.Errorf(
							"vault %q getPricePerFullShare is not decodable at activation block %d: %v",
							vault.ID,
							vault.ActivationBlock,
							err,
						)
						continue
					}
					price, err := BigIntAt(priceValues, 0)
					if err != nil || price.Sign() <= 0 {
						t.Errorf(
							"vault %q getPricePerFullShare is not positive at activation block %d: value=%v error=%v",
							vault.ID,
							vault.ActivationBlock,
							price,
							err,
						)
					}
				}
			}
		})
	}
}
