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
// motivated this migration. The complete 48-protocol registry set is locked by
// TestProtocolAvailabilityMatchesVerifiedBoundaries; repeating every adapter call here
// would also require external indexer services and roughly two hundred archive scans.
// Each selected probe wraps the real adapter, so a clean response cannot be mistaken for
// a central skip.
var liveAdapterBoundaryProbes = map[liveAvailabilityKey]struct{}{
	{protocolID: "compound-v2", chainID: Ethereum}:       {},
	{protocolID: "moonwell", chainID: Base}:              {},
	{protocolID: "flux-finance", chainID: Ethereum}:      {},
	{protocolID: "sonne", chainID: Base}:                 {},
	{protocolID: "sonne", chainID: Optimism}:             {},
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
// views existed. It is opt-in because it requires all nine external archive RPCs.
func TestProtocolAvailabilityBoundariesLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEPLOYMENT_WINDOWS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEPLOYMENT_WINDOWS_LIVE_TEST=1 to run archive-RPC boundary probes")
	}
	rpcURLs := map[ChainID]string{
		Ethereum:  os.Getenv("PORTFOLIO_ETH_RPC_URL"),
		BSC:       os.Getenv("PORTFOLIO_BSC_RPC_URL"),
		Base:      os.Getenv("PORTFOLIO_BASE_RPC_URL"),
		Arbitrum:  os.Getenv("PORTFOLIO_ARB_RPC_URL"),
		Polygon:   os.Getenv("PORTFOLIO_POLYGON_RPC_URL"),
		Monad:     os.Getenv("PORTFOLIO_MONAD_RPC_URL"),
		Plasma:    os.Getenv("PORTFOLIO_PLASMA_RPC_URL"),
		Avalanche: os.Getenv("PORTFOLIO_AVALANCHE_RPC_URL"),
		Optimism:  os.Getenv("PORTFOLIO_OPTIMISM_RPC_URL"),
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

// TestEtherfiOptimismVaultBoundariesLive pins both sides of every configured direct-vault
// boundary. It also proves that the other Ethereum Ether.fi vault pairs are not deployed on
// Optimism at the parity block, so a shared CREATE2 address is never mistaken for a deployment.
func TestEtherfiOptimismVaultBoundariesLive(t *testing.T) {
	if os.Getenv("PORTFOLIO_ETHERFI_OPTIMISM_BOUNDARIES_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_ETHERFI_OPTIMISM_BOUNDARIES_LIVE_TEST=1 to run Optimism archive-RPC probes")
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

	adapter := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter)
	positions := adapter.vaults[Optimism]
	if len(positions) != 6 {
		t.Fatalf("Ether.fi Optimism vault count = %d, want 6", len(positions))
	}
	firstCode := map[string]struct {
		vault      uint64
		accountant uint64
	}{
		"liquid-eth": {vault: 123_081_511, accountant: 123_081_511},
		"liquid-usd": {vault: 149_698_249, accountant: 149_698_252},
		"liquid-btc": {vault: 149_698_604, accountant: 149_698_606},
		"sethfi":     {vault: 149_699_005, accountant: 149_699_007},
		"ebtc":       {vault: 149_699_297, accountant: 149_699_299},
		"eusd":       {vault: 149_822_644, accountant: 149_822_646},
	}
	account := common.HexToAddress("0x000000000000000000000000000000000000dEaD")
	for _, position := range positions {
		position := position
		t.Run(position.ID, func(t *testing.T) {
			first, exists := firstCode[position.ID]
			if !exists {
				t.Fatalf("missing first-code evidence for %q", position.ID)
			}
			functional := first.vault
			if first.accountant > functional {
				functional = first.accountant
			}
			if position.ActivationBlock != functional {
				t.Fatalf("activation block = %d, want first-functional block %d", position.ActivationBlock, functional)
			}

			var beforeVaultCode, firstVaultCode, beforeAccountantCode, firstAccountantCode hexutil.Bytes
			codeCalls := []rpc.BatchElem{
				{
					Method: "eth_getCode",
					Args:   []any{position.Vault, hexutil.EncodeUint64(first.vault - 1)},
					Result: &beforeVaultCode,
				},
				{
					Method: "eth_getCode",
					Args:   []any{position.Vault, hexutil.EncodeUint64(first.vault)},
					Result: &firstVaultCode,
				},
				{
					Method: "eth_getCode",
					Args:   []any{position.Accountant, hexutil.EncodeUint64(first.accountant - 1)},
					Result: &beforeAccountantCode,
				},
				{
					Method: "eth_getCode",
					Args:   []any{position.Accountant, hexutil.EncodeUint64(first.accountant)},
					Result: &firstAccountantCode,
				},
			}
			if err := client.batchCall(context.Background(), codeCalls); err != nil {
				t.Fatalf("deployment code boundary: %v", err)
			}
			if len(beforeVaultCode) != 0 || len(firstVaultCode) == 0 ||
				len(beforeAccountantCode) != 0 || len(firstAccountantCode) == 0 {
				t.Fatalf(
					"first-code boundary: vault=%d/%d accountant=%d/%d",
					len(beforeVaultCode),
					len(firstVaultCode),
					len(beforeAccountantCode),
					len(firstAccountantCode),
				)
			}

			block := BlockRef{ChainID: Optimism, Number: position.ActivationBlock, Fixed: true}
			balanceRows, err := client.ParallelCalls(context.Background(), block, []ContractCall{{
				Contract: position.Vault,
				ABI:      erc20ABI,
				Method:   "balanceOf",
				Args:     []any{account},
			}})
			if err != nil || len(balanceRows) != 1 {
				t.Fatalf("vault balanceOf at activation: rows=%d error=%v", len(balanceRows), err)
			}
			if _, err := BigIntAt(balanceRows[0], 0); err != nil {
				t.Fatalf("decode vault balanceOf at activation: %v", err)
			}

			headerRows, err := client.ParallelCallsAllowFailure(context.Background(), block, []ContractCall{
				{Contract: position.Accountant, ABI: etherfiAccountantABI, Method: "vault"},
				{Contract: position.Accountant, ABI: etherfiAccountantABI, Method: "base"},
				{Contract: position.Accountant, ABI: etherfiAccountantABI, Method: "decimals"},
				{Contract: position.Accountant, ABI: etherfiAccountantABI, Method: "getRateSafe"},
				{Contract: position.Accountant, ABI: etherfiAccountantABI, Method: "getRate"},
			})
			if err != nil {
				t.Fatalf("accountant state at activation: %v", err)
			}
			for index, method := range []string{"vault", "base", "decimals"} {
				if headerRows[index].Error != nil {
					t.Fatalf("accountant %s at activation: %v", method, headerRows[index].Error)
				}
			}
			boundVault, err := AddressAt(headerRows[0].Values, 0)
			if err != nil || boundVault != position.Vault {
				t.Fatalf("accountant vault binding = %s, error=%v", boundVault, err)
			}
			base, err := AddressAt(headerRows[1].Values, 0)
			if err != nil || base == (common.Address{}) {
				t.Fatalf("accountant base = %s, error=%v", base, err)
			}
			decimals, err := Uint8At(headerRows[2].Values, 0)
			if err != nil || decimals > 77 {
				t.Fatalf("accountant decimals = %d, error=%v", decimals, err)
			}
			rateRow := headerRows[3]
			if rateRow.Error != nil {
				rateRow = headerRows[4]
			}
			if rateRow.Error != nil {
				t.Fatalf("accountant rate at activation: %v", rateRow.Error)
			}
			rate, err := BigIntAt(rateRow.Values, 0)
			if err != nil || rate.Sign() <= 0 {
				t.Fatalf("accountant rate = %v, error=%v", rate, err)
			}
			if _, err := readERC20Token(context.Background(), client, block, base); err != nil {
				t.Fatalf("base token metadata at activation: %v", err)
			}
		})
	}

	t.Run("pinned-parity-account", func(t *testing.T) {
		groups, err := readEtherfiVaultPositions(
			context.Background(),
			client,
			BlockRef{ChainID: Optimism, Number: 156_472_259, Fixed: true},
			common.HexToAddress("0x4acb6c4321253548a7d4bb9c84032cc4ee04bfd7"),
			positions,
		)
		if err != nil {
			t.Fatal(err)
		}
		components := make(map[string]Component, len(groups))
		for _, group := range groups {
			if len(group.Components) != 1 {
				t.Fatalf("group %q components = %d, want 1", group.ID, len(group.Components))
			}
			components[group.ID] = group.Components[0]
		}
		for _, want := range []struct {
			id          string
			numerator   string
			denominator string
			shares      string
			rate        string
		}{
			{
				id: "sethfi", numerator: "6219401721925815387269409211370666684",
				denominator: "1000000000000000000", shares: "5167382909661579358",
				rate: "1203588321333271298",
			},
			{
				id: "ebtc", numerator: "13596154585440", denominator: "100000000",
				shares: "135436", rate: "100388040",
			},
		} {
			component, exists := components[want.id]
			if !exists {
				t.Fatalf("missing pinned Ether.fi Optimism group %q: %+v", want.id, groups)
			}
			if component.AmountRaw != want.numerator || component.AmountDenominatorRaw != want.denominator {
				t.Errorf(
					"group %q amount = %s/%s, want %s/%s",
					want.id,
					component.AmountRaw,
					component.AmountDenominatorRaw,
					want.numerator,
					want.denominator,
				)
			}
			if component.Metadata["sharesRaw"] != want.shares || component.Metadata["rateRaw"] != want.rate {
				t.Errorf("group %q metadata = %+v", want.id, component.Metadata)
			}
		}
		if got := len(components); got != 2 {
			t.Fatalf("pinned Ether.fi Optimism groups = %d, want 2", got)
		}
	})

	undeployed := make([]etherfiVaultPosition, 0, 9)
	for _, position := range adapter.vaults[Ethereum] {
		if _, deployedOnOptimism := firstCode[position.ID]; !deployedOnOptimism {
			undeployed = append(undeployed, position)
		}
	}
	if len(undeployed) != 9 {
		t.Fatalf("Ether.fi vault pairs absent from Optimism = %d, want 9", len(undeployed))
	}
	const parityBlock = uint64(156_472_259)
	code := make([]hexutil.Bytes, len(undeployed)*2)
	calls := make([]rpc.BatchElem, len(code))
	for index, deployment := range undeployed {
		calls[index*2] = rpc.BatchElem{
			Method: "eth_getCode", Args: []any{deployment.Vault, hexutil.EncodeUint64(parityBlock)},
			Result: &code[index*2],
		}
		calls[index*2+1] = rpc.BatchElem{
			Method: "eth_getCode", Args: []any{deployment.Accountant, hexutil.EncodeUint64(parityBlock)},
			Result: &code[index*2+1],
		}
	}
	if err := client.batchCall(context.Background(), calls); err != nil {
		t.Fatalf("undeployed Optimism vault code: %v", err)
	}
	for index, deployment := range undeployed {
		if len(code[index*2]) != 0 || len(code[index*2+1]) != 0 {
			t.Errorf(
				"Ether.fi Optimism %q unexpectedly has code at block %d: vault=%d accountant=%d",
				deployment.ID,
				parityBlock,
				len(code[index*2]),
				len(code[index*2+1]),
			)
		}
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
		Ethereum:  "PORTFOLIO_ETH_RPC_URL",
		BSC:       "PORTFOLIO_BSC_RPC_URL",
		Base:      "PORTFOLIO_BASE_RPC_URL",
		Arbitrum:  "PORTFOLIO_ARB_RPC_URL",
		Polygon:   "PORTFOLIO_POLYGON_RPC_URL",
		Monad:     "PORTFOLIO_MONAD_RPC_URL",
		Plasma:    "PORTFOLIO_PLASMA_RPC_URL",
		Avalanche: "PORTFOLIO_AVALANCHE_RPC_URL",
		Optimism:  "PORTFOLIO_OPTIMISM_RPC_URL",
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
					if vault.DeactivationBlock == 0 {
						continue
					}
					var finalBalance, finalPrice, afterBalance, afterPrice hexutil.Bytes
					finalCalls := []rpc.BatchElem{
						{
							Method: "eth_call",
							Args: []any{
								map[string]any{"to": vault.Vault, "data": hexutil.Bytes(balanceCall)},
								hexutil.EncodeUint64(vault.DeactivationBlock),
							},
							Result: &finalBalance,
						},
						{
							Method: "eth_call",
							Args: []any{
								map[string]any{"to": vault.Vault, "data": hexutil.Bytes(priceCall)},
								hexutil.EncodeUint64(vault.DeactivationBlock),
							},
							Result: &finalPrice,
						},
						{
							Method: "eth_call",
							Args: []any{
								map[string]any{"to": vault.Vault, "data": hexutil.Bytes(balanceCall)},
								hexutil.EncodeUint64(vault.DeactivationBlock + 1),
							},
							Result: &afterBalance,
						},
						{
							Method: "eth_call",
							Args: []any{
								map[string]any{"to": vault.Vault, "data": hexutil.Bytes(priceCall)},
								hexutil.EncodeUint64(vault.DeactivationBlock + 1),
							},
							Result: &afterPrice,
						},
					}
					if err := client.batchCallTransport(context.Background(), finalCalls); err != nil {
						t.Fatalf("vault %q deactivation boundary: %v", vault.ID, err)
					}
					finalPriceValues, finalPriceErr := beefyVaultABI.Unpack("getPricePerFullShare", finalPrice)
					finalPricePositive := false
					if finalPriceErr == nil && finalCalls[1].Error == nil {
						finalPriceValue, valueErr := BigIntAt(finalPriceValues, 0)
						finalPricePositive = valueErr == nil && finalPriceValue.Sign() > 0
					}
					if finalCalls[0].Error != nil || len(finalBalance) != 32 || !finalPricePositive {
						t.Errorf("vault %q is not adapter-safe at final block %d", vault.ID, vault.DeactivationBlock)
					}
					afterPriceValues, afterPriceErr := beefyVaultABI.Unpack("getPricePerFullShare", afterPrice)
					afterPricePositive := false
					if afterPriceErr == nil && finalCalls[3].Error == nil {
						afterPriceValue, valueErr := BigIntAt(afterPriceValues, 0)
						afterPricePositive = valueErr == nil && afterPriceValue.Sign() > 0
					}
					if finalCalls[2].Error == nil && len(afterBalance) == 32 && afterPricePositive {
						t.Errorf("vault %q remains adapter-safe after final block %d", vault.ID, vault.DeactivationBlock)
					}
				}
			}
		})
	}
}
