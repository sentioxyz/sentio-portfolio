package portfolio

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// shareTestAdapter reports a vault position on BSC that consumed the account's vault shares, and
// closes read once it has.
type shareTestAdapter struct {
	adapterBase
	share common.Address
	read  chan struct{}
}

func (a *shareTestAdapter) Positions(_ context.Context, _ *RPCClient, block BlockRef, _ common.Address) ([]Group, error) {
	defer close(a.read)
	return []Group{{
		ID:    "vault",
		Label: "vault",
		Components: []Component{{
			Kind: "supply",
			Token: Token{
				ChainID: block.ChainID, Address: common.HexToAddress("0x0000000000000000000000000000000000000a55"), Symbol: "A", Decimals: 18,
			},
			AmountRaw: "1000000000000000000",
			Source:    Source{Contract: a.share, Method: "convertToAssets", Holds: []common.Address{a.share}},
		}},
	}}, nil
}

// awaitingWalletProvider answers with result only once after closes, as a provider still
// discovering would, or fails when its context ends first. It gives up after a while, so a scan
// that waits for discovery before reading anything else fails rather than hangs.
type awaitingWalletProvider struct {
	after  <-chan struct{}
	result WalletBalanceResult
}

func (p awaitingWalletProvider) WalletBalances(ctx context.Context, _ WalletBalanceRequest) (WalletBalanceResult, error) {
	select {
	case <-p.after:
		return p.result, nil
	case <-ctx.Done():
		return WalletBalanceResult{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return WalletBalanceResult{}, errors.New("nothing was read while discovery ran")
	}
}

// pinnedBSCScan scans BSC alone, fixed at block.
func pinnedBSCScan(block uint64) ScanOptions {
	return ScanOptions{
		ChainIDs:    map[ChainID]struct{}{BSC: {}},
		BlockNumber: map[ChainID]uint64{BSC: block},
	}
}

// Wallet discovery runs beside the workers: a protocol is read while the provider has not answered
// yet, and the holdings it then reports are still reconciled with that protocol's position.
func TestScanReadsProtocolsWhileWalletDiscoveryRuns(t *testing.T) {
	const pin = uint64(1_000)
	owner := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	share := common.HexToAddress("0x0000000000000000000000000000000000005a5e")
	vault := &shareTestAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "vault", Name: "vault", Chains: []ChainID{BSC}}},
		share:       share,
		read:        make(chan struct{}),
	}
	// The block the RPC server pins, so the provider's amounts are used as they are.
	block := BlockRef{ChainID: BSC, Number: pin, Hash: common.BytesToHash([]byte{byte(pin & 0xff), 1}), Timestamp: 1}
	provider := awaitingWalletProvider{after: vault.read, result: WalletBalanceResult{Chains: []WalletBalanceChain{{
		Block: block,
		Accounts: []WalletBalanceAccount{{Account: owner, Balances: []WalletBalance{
			{Token: Token{ChainID: BSC, Address: share, Symbol: "SHARE", Decimals: 18}, AmountRaw: "5", MetadataComplete: true},
			{Token: Token{ChainID: BSC, Address: walletTestUSDC, Symbol: "USDC", Decimals: 6}, AmountRaw: "250000000", MetadataComplete: true},
		}}},
	}}}}
	recorder := &recordingObserver{}
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: pin}, &recordingPriceProvider{}, recorder,
		vault,
		newDeadlineTestAdapter(walletProtocolID, false),
	)
	engine.walletBalanceProvider = provider

	response := engine.ScanWithOptions(context.Background(), owner, pinnedBSCScan(pin))
	if len(response.Errors) != 0 {
		t.Fatalf("errors = %+v", response.Errors)
	}
	held := make(map[string][]string)
	for _, snapshot := range response.Snapshots {
		for _, group := range snapshot.Groups {
			held[snapshot.ProtocolID] = append(held[snapshot.ProtocolID], group.ID)
		}
	}
	// The vault counted the shares, so the wallet reports the USDC alone.
	if fmt.Sprint(held["vault"]) != "[vault]" || fmt.Sprint(held[walletProtocolID]) != fmt.Sprint([]string{walletTokenGroupID(walletTestUSDC)}) {
		t.Fatalf("groups = %v, want the vault and the wallet's USDC without the shares", held)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.scans) != 1 {
		t.Fatalf("scan observations = %d, want 1", len(recorder.scans))
	}
	scan := recorder.scans[0]
	if scan.Deployments != 2 || scan.WalletDiscovery <= 0 || scan.WalletDiscovery > scan.Protocols {
		t.Fatalf("observation deployments = %d, discovery = %s, protocols = %s; want 2 and discovery within the protocols",
			scan.Deployments, scan.WalletDiscovery, scan.Protocols)
	}
}

// Discovery that outlasts the collection deadline costs the wallet alone: the protocols were read
// meanwhile, and the wallet's deployment, never started, is reported as not read rather than
// failed.
func TestScanReportsTheWalletUnreadWhenDiscoveryOutlastsTheDeadline(t *testing.T) {
	recorder := &recordingObserver{}
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, &recordingPriceProvider{}, recorder,
		newDeadlineTestAdapter("quick", false),
		newDeadlineTestAdapter(walletProtocolID, false),
	)
	engine.walletBalanceProvider = awaitingWalletProvider{after: make(chan struct{})}
	options := pinnedBSCScan(1_000)
	startedAt := time.Now()
	options.CollectDeadline = startedAt.Add(150 * time.Millisecond)

	response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), options)
	if late := time.Since(options.CollectDeadline); late > 500*time.Millisecond {
		t.Fatalf("scan returned %s after its collection deadline", late)
	}
	if len(response.Snapshots) != 1 || response.Snapshots[0].ProtocolID != "quick" {
		t.Fatalf("snapshots = %+v, want the quick adapter's alone", response.Snapshots)
	}
	incomplete := scanErrorsIn(response, incompleteScope)
	if len(response.Errors) != 1 || len(incomplete) != 1 || incomplete[0].ProtocolID != walletProtocolID ||
		incomplete[0].ChainID != BSC || incomplete[0].Message != notReadMessage {
		t.Fatalf("errors = %+v, want the wallet reported as not read and nothing else", response.Errors)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.scans) != 1 || recorder.scans[0].Deployments != 1 || recorder.scans[0].Incomplete != 1 {
		t.Fatalf("scan observations = %+v, want one deployment read and the wallet missing", recorder.scans)
	}
}

// concurrencyTestAdapter holds each chain job until full closes, once as many jobs run at once as
// the test expects, and records the most it saw running together.
type concurrencyTestAdapter struct {
	adapterBase
	running *atomic.Int32
	most    *atomic.Int32
	want    int32
	full    chan struct{}
	fill    *sync.Once
}

func (a *concurrencyTestAdapter) Positions(ctx context.Context, _ *RPCClient, _ BlockRef, _ common.Address) ([]Group, error) {
	running := a.running.Add(1)
	defer a.running.Add(-1)
	for most := a.most.Load(); running > most && !a.most.CompareAndSwap(most, running); most = a.most.Load() {
	}
	if running == a.want {
		a.fill.Do(func() { close(a.full) })
	}
	select {
	case <-a.full:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("only %d deployments ran at once", a.most.Load())
	}
}

// Once discovery is in, the wallet reads every chain at once and on none of the workers: here the
// scan's one worker is held until all three of the wallet's chains are being read together.
func TestScanReadsTheWalletsChainsTogetherBesideTheWorkers(t *testing.T) {
	const pin = uint64(1_000)
	rpcs := []*scanTestChain{newScanTestChain(t, BSC, pin), newScanTestChain(t, Base, pin), newScanTestChain(t, Arbitrum, pin)}
	var running, most atomic.Int32
	full := make(chan struct{})
	wallet := &concurrencyTestAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: walletProtocolID, Name: "Wallet", Chains: []ChainID{BSC, Base, Arbitrum}}},
		running:     &running,
		most:        &most,
		want:        3,
		full:        full,
		fill:        &sync.Once{},
	}
	held := &waitingTestAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "held", Name: "held", Chains: []ChainID{BSC}}},
		wait: func(ctx context.Context) {
			select {
			case <-full:
			case <-ctx.Done():
			}
		},
	}
	everywhere := chainAvailability{BSC: {availableFromGenesis()}, Base: {availableFromGenesis()}, Arbitrum: {availableFromGenesis()}}
	engine := newPresenceTestEngine(t, rpcs, nil, map[string]chainAvailability{
		"held":           {BSC: {availableFromGenesis()}},
		walletProtocolID: everywhere,
	}, held, wallet)
	engine.scanWorkers = 1

	response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x00000000000000000000000000000000000000a1"), pinnedScan(rpcs))
	// Without a provider the wallet reports discovery as unavailable on each chain, and nothing
	// else may fail.
	for _, scanError := range response.Errors {
		if scanError.ProtocolID != walletProtocolID || scanError.Message != "wallet token discovery provider is not configured" {
			t.Fatalf("errors = %+v", response.Errors)
		}
	}
	if len(response.Errors) != 3 || most.Load() != 3 {
		t.Fatalf("errors = %d, wallet chains read together = %d; want 3 and 3", len(response.Errors), most.Load())
	}
}

func TestScanReadsAsManyDeploymentsAtOnceAsItHasWorkers(t *testing.T) {
	for _, workers := range []int{0, 1, 5} {
		want := workers
		if want == 0 {
			want = defaultScanWorkers
		}
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var running, most atomic.Int32
			full := make(chan struct{})
			var fill sync.Once
			adapters := make([]Adapter, 0, 8)
			for index := range 8 {
				id := fmt.Sprintf("held-%d", index)
				adapters = append(adapters, &concurrencyTestAdapter{
					adapterBase: adapterBase{info: ProtocolInfo{ID: id, Name: id, Chains: []ChainID{BSC}}},
					running:     &running,
					most:        &most,
					want:        int32(want),
					full:        full,
					fill:        &fill,
				})
			}
			engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, nil, nil, adapters...)
			engine.scanWorkers = workers

			response := engine.ScanWithOptions(context.Background(), common.HexToAddress("0x1"), pinnedBSCScan(1_000))
			if len(response.Errors) != 0 {
				t.Fatalf("errors = %+v", response.Errors)
			}
			if got := most.Load(); got != int32(want) {
				t.Fatalf("at most %d deployments ran at once, want %d", got, want)
			}
		})
	}
}

func TestEngineConfigScanWorkersDefaultAndOverride(t *testing.T) {
	if got := NewEngine(nil, nil).workers(); got != defaultScanWorkers {
		t.Fatalf("default workers = %d, want %d", got, defaultScanWorkers)
	}
	if got := NewEngineWithConfig(nil, nil, EngineConfig{ScanWorkers: 6}).workers(); got != 6 {
		t.Fatalf("configured workers = %d, want 6", got)
	}
}
