package portfolio

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type Engine struct {
	rpcURLs               map[ChainID]string
	adapters              []Adapter
	registrations         map[string]registeredAdapter
	priceProvider         PriceProvider
	walletBalanceProvider WalletBalanceProvider
	headLagBlocks         uint64
	observer              Observer
	indexerLane           *indexerLane
	disabledChains        map[ChainID]struct{}
	scanWorkers           int
	// sentioIndexers is the host's index configuration, kept for the Sui engine built from this one.
	sentioIndexers map[string]SentioIndexerConfig
}

// defaultHeadLagBlocks is how far behind the advertised head a live scan pins itself. Four blocks
// was enough to remove the failure on every chain measured; see LatestSettledBlock for why the
// margin is needed at all.
const defaultHeadLagBlocks = 4

// defaultScanWorkers is how many deployments a scan reads at once.
const defaultScanWorkers = 3

// EngineConfig contains deployment-specific integrations owned by the host.
// Public source must not provide defaults for private indexer projects.
type EngineConfig struct {
	SentioIndexers map[string]SentioIndexerConfig
	// WalletBalanceProvider is the only ERC-20 discovery source for live and
	// historical scans. Amounts from another block are re-read at the settled RPC pin.
	WalletBalanceProvider WalletBalanceProvider
	// HeadLagBlocks overrides how far behind the advertised head a live scan pins itself. Zero
	// selects defaultHeadLagBlocks; a deployment whose RPC pool is in lockstep may set it to 1.
	HeadLagBlocks uint64
	// Observer receives per-scan, per-protocol, per-RPC and per-indexer-request timings. Nil
	// observes nothing.
	Observer Observer
	// IndexerConcurrency bounds the Sentio indexer requests this engine keeps in flight across
	// every protocol, chain and concurrent scan. Zero selects defaultIndexerConcurrency.
	IndexerConcurrency int
	// DisabledChains are supported chains this deployment does not serve. The engine dials
	// nothing on them, Chains omits them, and Protocols drops them from every protocol's chains,
	// leaving out a protocol with no chain left. A scan that names one reports it as a chain
	// error. Each entry must be in SupportedChainIDs.
	DisabledChains []ChainID
	// ScanWorkers bounds the (protocol, chain) deployments one scan reads at once; the wallet's
	// own deployments run beside them. Each scan has workers of its own, while the indexer lane,
	// the RPC endpoints and the wallet provider are shared by every scan, so more workers make a
	// lone scan faster without adding capacity for concurrent ones. Zero selects
	// defaultScanWorkers.
	ScanWorkers int
}

func (c EngineConfig) headLagBlocks() uint64 {
	if c.HeadLagBlocks == 0 {
		return defaultHeadLagBlocks
	}
	return c.HeadLagBlocks
}

func (c EngineConfig) sentioIndexer(protocolID string) SentioIndexerConfig {
	return c.SentioIndexers[protocolID]
}

type ScanOptions struct {
	ProtocolIDs map[string]struct{}
	ChainIDs    map[ChainID]struct{}
	BlockNumber map[ChainID]uint64
	SkipPrices  bool
	// CollectDeadline, when set, is when the scan stops reading positions. Chain setup, wallet
	// discovery, presence probes and adapters still running are canceled, deployments not yet
	// started are skipped, and each is reported as an "incomplete" error; the scan then values
	// what it read. Zero reads everything, bounded only by ctx.
	CollectDeadline time.Time
	// PricingDeadline, when set, bounds pricing. It is separate from CollectDeadline so that a
	// scan stopped at its collection deadline can still price what it read. Pricing that fails
	// because its context ended is reported as an "incomplete" error about no chain, apart from
	// the per-token pricing errors. Zero bounds pricing by ctx alone.
	PricingDeadline time.Time
}

// incompleteScope labels what a scan stopped before reading, as opposed to what failed: a chain
// it never set up, a deployment it never started, one it stopped midway, or — about no chain at
// all — prices it did not finish fetching. An unpriced holding is therefore unquotable only when
// no such pricing entry is present.
const incompleteScope = "incomplete"

// Messages of the errors a scan reports in incompleteScope. They say what is missing, not why the
// scan stopped: its collection deadline or the caller's context.
const (
	chainNotSetUpMessage    = "chain not set up before the scan stopped"
	notReadMessage          = "not read before the scan stopped"
	stoppedMessage          = "stopped before it finished reading"
	pricesNotFetchedMessage = "prices not all fetched before the scan stopped"
)

// withOptionalDeadline bounds ctx by deadline unless deadline is zero.
func withOptionalDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}

func (o ScanOptions) includesProtocol(protocolID string) bool {
	if len(o.ProtocolIDs) == 0 {
		return true
	}
	_, exists := o.ProtocolIDs[protocolID]
	return exists
}

func (o ScanOptions) includesChain(chainID ChainID) bool {
	if len(o.ChainIDs) == 0 {
		return true
	}
	_, exists := o.ChainIDs[chainID]
	return exists
}

func NewEngine(
	rpcURLs map[ChainID]string,
	priceProvider PriceProvider,
) *Engine {
	return NewEngineWithConfig(rpcURLs, priceProvider, EngineConfig{})
}

func NewEngineWithConfig(
	rpcURLs map[ChainID]string,
	priceProvider PriceProvider,
	config EngineConfig,
) *Engine {
	adapters := make([]Adapter, 0, 24)
	adapters = append(adapters, aaveAdapters()...)
	adapters = append(adapters, compoundV2Adapters()...)
	adapters = append(adapters, newVenusAdapter())
	adapters = append(adapters, newCompoundV3Adapter())
	adapters = append(adapters, erc4626Adapters()...)
	adapters = append(adapters, lstAdapters()...)
	adapters = append(adapters, newLidoAdapter())
	adapters = append(adapters, newMethAdapter(config.sentioIndexer("meth-protocol")))
	adapters = append(adapters, newEtherfiAdapter(config.sentioIndexer("etherfi")))
	adapters = append(adapters, newFraxEtherAdapter(config.sentioIndexer("frax-ether")))
	adapters = append(adapters, newRenzoAdapter(config.sentioIndexer("renzo")))
	adapters = append(adapters, newAsterAdapter(config.sentioIndexer("aster")))
	adapters = append(adapters, newFxProtocolAdapter())
	adapters = append(adapters, newRocketPoolAdapter())
	adapters = append(adapters, newStaderAdapter())
	adapters = append(adapters, newOlympusAdapter())
	adapters = append(adapters, newFraxlendAdapter())
	adapters = append(adapters, newAaveV4Adapter())
	adapters = append(adapters, newMakerDAOAdapter())
	adapters = append(adapters, newSkyAdapter())
	adapters = append(adapters, newMapleAdapter())
	adapters = append(adapters, newLiquityV1Adapter())
	adapters = append(adapters, newCurveCrvUSDAdapter())
	adapters = append(adapters, newCurveLendingAdapter())
	adapters = append(adapters, newVesperAdapter())
	adapters = append(adapters, newYearnV3Adapter())
	adapters = append(adapters, newBeefyAdapter())
	adapters = append(adapters, newStakeWiseAdapter())
	adapters = append(adapters, newListaAdapter(config.sentioIndexer("lista")))
	adapters = append(adapters, newEulerV2Adapter(config.sentioIndexer("euler-v2")))
	adapters = append(adapters, newMorphoAdapter(config.sentioIndexer("morpho-blue")))
	adapters = append(adapters, newPendleAdapter(config.sentioIndexer("pendle")))
	adapters = append(adapters, newFluidAdapter())
	adapters = append(adapters, newUniswapAdapters(
		config.sentioIndexer("uniswap-v3"),
		config.sentioIndexer("uniswap-v4"),
	)...)
	adapters = append(adapters, newWalletAdapter())
	registrations := mustRegisterAdapters(adapters, protocolAvailabilityByID)
	disabledChains := make(map[ChainID]struct{}, len(config.DisabledChains))
	for _, chainID := range config.DisabledChains {
		if !supportsChain(SupportedChainIDs, chainID) {
			panic(fmt.Sprintf("disabled chain %d is not a supported chain", chainID))
		}
		disabledChains[chainID] = struct{}{}
	}
	return &Engine{
		rpcURLs:               rpcURLs,
		adapters:              adapters,
		registrations:         registrations,
		priceProvider:         priceProvider,
		walletBalanceProvider: config.WalletBalanceProvider,
		headLagBlocks:         config.headLagBlocks(),
		observer:              config.Observer,
		indexerLane:           newIndexerLane(config.IndexerConcurrency),
		disabledChains:        disabledChains,
		scanWorkers:           config.ScanWorkers,
		sentioIndexers:        maps.Clone(config.SentioIndexers),
	}
}

// workers is how many deployments a scan reads at once.
func (e *Engine) workers() int {
	if e.scanWorkers <= 0 {
		return defaultScanWorkers
	}
	return e.scanWorkers
}

// prefetchPresence starts, for every adapter that can answer it in one request, the question of
// which chains hold anything for each scanned account. The probes run in the background while the
// workers get through the adapters that need no indexer, so the index-backed adapters — which
// come late in the adapter order — find their verdicts waiting. Each protocol's probes run on a
// context of their own; it returns their cancel functions by protocol ID.
func (e *Engine) prefetchPresence(
	ctx context.Context,
	options ScanOptions,
	chains map[ChainID]*chainScan,
) map[string]context.CancelFunc {
	if len(chains) < presenceMinChains {
		return nil
	}
	accounts := make([]common.Address, 0, 1)
	seen := make(map[common.Address]struct{})
	for _, chainID := range SupportedChainIDs {
		chain := chains[chainID]
		if chain == nil {
			continue
		}
		for _, account := range chain.accounts {
			if _, exists := seen[account.Address]; exists {
				continue
			}
			seen[account.Address] = struct{}{}
			accounts = append(accounts, account.Address)
		}
	}
	cancels := make(map[string]context.CancelFunc)
	for _, adapter := range e.adapters {
		info := adapter.Info()
		if !options.includesProtocol(info.ID) {
			continue
		}
		prefetcher, ok := adapter.(presencePrefetcher)
		if !ok {
			continue
		}
		probeCtx, cancel := context.WithCancel(ctx)
		cancels[info.ID] = cancel
		// The probe's requests report under the protocol they serve; a chain of zero says "all".
		prefetchCtx := withDeployment(probeCtx, info.ID, 0)
		for _, account := range accounts {
			prefetcher.prefetchPresence(prefetchCtx, account)
		}
	}
	return cancels
}

// presenceLifetimes ends each protocol's prefetched presence probes once no deployment of the
// protocol in the scan is left to use them: each has finished, been skipped as inactive at its pin,
// or will never be dispatched. A probe nobody will wait for would otherwise go on holding its
// prefetch slot and its lane slot, and run its queries to the end.
type presenceLifetimes struct {
	mu        sync.Mutex
	remaining map[string]int
	cancel    map[string]context.CancelFunc
}

// newPresenceLifetimes tracks the protocols in cancel, each with deployments[protocolID]
// deployments left, and ends at once the probes of a protocol the scan dispatches nothing for.
func newPresenceLifetimes(
	cancel map[string]context.CancelFunc,
	deployments map[string]int,
) *presenceLifetimes {
	lifetimes := &presenceLifetimes{remaining: make(map[string]int, len(cancel)), cancel: cancel}
	for protocolID, cancelProbes := range cancel {
		if deployments[protocolID] == 0 {
			cancelProbes()
			continue
		}
		lifetimes.remaining[protocolID] = deployments[protocolID]
	}
	return lifetimes
}

// done records that one deployment of protocolID will not use the protocol's probes again.
func (l *presenceLifetimes) done(protocolID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	remaining, tracked := l.remaining[protocolID]
	if !tracked {
		return
	}
	if remaining > 1 {
		l.remaining[protocolID] = remaining - 1
		return
	}
	delete(l.remaining, protocolID)
	l.cancel[protocolID]()
}

// Chains returns the supported chains this engine serves, in canonical order.
func (e *Engine) Chains() []ChainID {
	chains := make([]ChainID, 0, len(SupportedChainIDs))
	for _, chainID := range SupportedChainIDs {
		if !e.chainDisabled(chainID) {
			chains = append(chains, chainID)
		}
	}
	return chains
}

func (e *Engine) chainDisabled(chainID ChainID) bool {
	_, disabled := e.disabledChains[chainID]
	return disabled
}

func (e *Engine) Protocols() []ProtocolInfo {
	result := make([]ProtocolInfo, 0, len(e.adapters))
	for _, adapter := range e.adapters {
		info := adapter.Info()
		if len(e.disabledChains) > 0 {
			chains := make([]ChainID, 0, len(info.Chains))
			for _, chainID := range info.Chains {
				if !e.chainDisabled(chainID) {
					chains = append(chains, chainID)
				}
			}
			if len(chains) == 0 {
				continue
			}
			info.Chains = chains
		}
		result = append(result, info)
	}
	return result
}

type chainScan struct {
	client                 *RPCClient
	block                  BlockRef
	accounts               []attributedAccount
	walletProviderAccounts map[common.Address]walletProviderAccount
	walletProviderErrors   []error
	// walletCoverage says which tokens the wallet could not have discovered — on a chain the
	// provider does not cover, or at a pin its sample was not taken at — where no read failed.
	walletCoverage []error
}

func attributedGroups(
	groups []Group,
	account attributedAccount,
	root common.Address,
) []Group {
	if account.Address == root {
		return groups
	}
	result := make([]Group, len(groups))
	for index, group := range groups {
		group.ID = fmt.Sprintf(
			"attributed:%s:%s:%s",
			account.Attribution,
			strings.ToLower(account.Address.Hex()),
			group.ID,
		)
		if group.Metadata == nil {
			group.Metadata = make(map[string]any)
		}
		group.Metadata["attributedAccount"] = account.Address
		group.Metadata["accountAttribution"] = account.Attribution
		group.Metadata["accountAttributionSource"] = account.Source
		result[index] = group
	}
	return result
}

func (e *Engine) Scan(ctx context.Context, address common.Address) *Response {
	return e.ScanWithOptions(ctx, address, ScanOptions{})
}

func (e *Engine) ScanWithOptions(
	ctx context.Context,
	address common.Address,
	options ScanOptions,
) *Response {
	scanStartedAt := time.Now()
	observation := ScanObservation{Options: options}
	// The scan owns its context: what it starts in the background, such as presence probes, ends
	// when it returns rather than whenever the caller cancels.
	ctx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()
	ctx = withScan(ctx, e.observer, e.indexerLane)
	observer := observerFrom(ctx)
	// Reading positions stops at the collection deadline; pricing derives its own context from
	// ctx, so it can still value what was read.
	collectCtx, cancelCollect := withOptionalDeadline(ctx, options.CollectDeadline)
	defer cancelCollect()
	protocols := make([]ProtocolInfo, 0, len(e.adapters))
	for _, protocol := range e.Protocols() {
		if options.includesProtocol(protocol.ID) {
			protocols = append(protocols, protocol)
		}
	}
	response := &Response{
		SchemaVersion:      1,
		Address:            address,
		SupportedProtocols: protocols,
		Snapshots:          make([]Snapshot, 0),
		ProtocolSummaries:  make([]ProtocolSummary, 0),
		Errors:             make([]ScanError, 0),
		ChainBlocks:        make(map[ChainID]uint64),
		Prices:             make(map[string]float64),
	}
	chains := make(map[ChainID]*chainScan)
	var mutex sync.Mutex
	var wait sync.WaitGroup
	// chainFailed reports a chain that could not be set up. A failure once collection has stopped
	// is the scan's doing rather than the chain's, so it reports what the scan is missing.
	chainFailed := func(chainID ChainID, err error) {
		scanError := ScanError{Scope: "chain", ChainID: chainID, Message: PublicError(err)}
		if collectCtx.Err() != nil {
			scanError = ScanError{Scope: incompleteScope, ChainID: chainID, Message: chainNotSetUpMessage}
		}
		mutex.Lock()
		response.Errors = append(response.Errors, scanError)
		mutex.Unlock()
	}
	for _, chainID := range SupportedChainIDs {
		if !options.includesChain(chainID) {
			continue
		}
		if e.chainDisabled(chainID) {
			// Only a scan that names the chain asked for it; an unfiltered scan just skips it.
			if len(options.ChainIDs) > 0 {
				mutex.Lock()
				response.Errors = append(response.Errors, ScanError{
					Scope: "chain", ChainID: chainID, Message: "chain is disabled",
				})
				mutex.Unlock()
			}
			continue
		}
		wait.Add(1)
		go func(chainID ChainID) {
			defer wait.Done()
			url := e.rpcURLs[chainID]
			if url == "" {
				mutex.Lock()
				response.Errors = append(response.Errors, ScanError{
					Scope: "chain", ChainID: chainID, Message: "RPC URL is not configured",
				})
				mutex.Unlock()
				return
			}
			client, err := DialRPC(collectCtx, chainID, url)
			if err != nil {
				chainFailed(chainID, err)
				return
			}
			var block BlockRef
			blockNumber, fixedBlock := options.BlockNumber[chainID]
			if fixedBlock {
				block, err = client.BlockByNumber(collectCtx, blockNumber)
				block.Fixed = true
			} else {
				block, err = client.LatestSettledBlock(collectCtx, e.headLagBlocks)
			}
			if err != nil {
				client.Close()
				chainFailed(chainID, err)
				return
			}
			accounts, err := resolveAccountScope(collectCtx, client, block, address)
			if err != nil {
				client.Close()
				chainFailed(chainID, err)
				return
			}
			mutex.Lock()
			chains[chainID] = &chainScan{client: client, block: block, accounts: accounts}
			response.ChainBlocks[chainID] = block.Number
			mutex.Unlock()
		}(chainID)
	}
	wait.Wait()
	observation.ChainSetup = time.Since(scanStartedAt)
	observation.Chains = len(chains)
	pinned := make(map[ChainID]BlockRef, len(chains))
	// A presence probe asks only about the chains its account is read on.
	accountChains := make(map[string][]ChainID)
	for _, chainID := range SupportedChainIDs {
		chain := chains[chainID]
		if chain == nil {
			continue
		}
		pinned[chainID] = chain.block
		for _, account := range chain.accounts {
			key := strings.ToLower(account.Address.Hex())
			accountChains[key] = append(accountChains[key], chainID)
		}
	}
	collectCtx = withPinnedBlocks(collectCtx, pinned, accountChains)
	probeCancels := e.prefetchPresence(collectCtx, options, chains)
	defer func() {
		for _, chain := range chains {
			chain.client.Close()
		}
	}()

	type deployment struct {
		registeredAdapter
		chainID ChainID
	}
	// The wallet's deployments are kept apart: they need wallet discovery's answer, and run beside
	// the workers rather than on them.
	deployments := make([]deployment, 0)
	walletDeployments := make([]deployment, 0, len(chains))
	deploymentCounts := make(map[string]int)
	for _, adapter := range e.adapters {
		info := adapter.Info()
		if !options.includesProtocol(info.ID) {
			continue
		}
		registration, exists := e.registrations[info.ID]
		if !exists {
			panic(fmt.Sprintf("adapter %q is missing its validated availability", info.ID))
		}
		for _, chainID := range info.Chains {
			// A chain that failed to pin has already reported its error; there is nothing to
			// dispatch for it.
			if !options.includesChain(chainID) || chains[chainID] == nil {
				continue
			}
			job := deployment{registeredAdapter: registration, chainID: chainID}
			deploymentCounts[info.ID]++
			if info.ID == walletProtocolID {
				walletDeployments = append(walletDeployments, job)
				continue
			}
			deployments = append(deployments, job)
		}
	}
	probes := newPresenceLifetimes(probeCancels, deploymentCounts)
	// read reads one deployment into the response.
	read := func(job deployment) {
		chain := chains[job.chainID]
		if chain == nil {
			probes.done(job.Info().ID)
			return
		}
		info := job.Info()
		if !job.ActiveAt(job.chainID, chain.block.Number) {
			probes.done(info.ID)
			return
		}
		startedAt := time.Now()
		// Every RPC and indexer request the adapter makes reports under its name, and every index
		// read records its coverage notes for this deployment, whatever the adapter returns.
		jobCtx, coverage := withCoverage(withDeployment(collectCtx, info.ID, job.chainID))
		groups := make([]Group, 0)
		var positionErr error
		for _, account := range chain.accounts {
			var accountGroups []Group
			var err error
			// Only a wallet deployment looks at what discovery found: the other deployments run
			// while discovery is still filling it in.
			var providerAccount walletProviderAccount
			useProvider := false
			if info.ID == walletProtocolID {
				providerAccount, useProvider = chain.walletProviderAccounts[account.Address]
			}
			if useProvider {
				accountGroups, err = providerWalletGroups(
					jobCtx,
					chain.client,
					chain.block,
					job.chainID,
					account.Address,
					providerAccount,
				)
			} else {
				accountGroups, err = job.Positions(
					jobCtx,
					chain.client,
					chain.block,
					account.Address,
				)
			}
			groups = append(groups, attributedGroups(accountGroups, account, address)...)
			if err != nil {
				positionErr = errors.Join(positionErr, err)
				// Wallet holdings are independent per account, so one malformed or failed
				// account must not discard holdings attributed to the remaining accounts.
				if info.ID == walletProtocolID {
					continue
				}
				break
			}
		}
		probes.done(info.ID)
		mutex.Lock()
		if positionErr != nil {
			scanError := ScanError{
				Scope:        "protocol",
				ChainID:      job.chainID,
				ProtocolID:   info.ID,
				ProtocolName: info.Name,
				Message:      PublicError(positionErr),
			}
			// An adapter that fails once collection has stopped was stopped: what the
			// response lacks is the rest of this deployment, not an error of its own.
			// Whatever groups it read before are kept, as for any other failure.
			if collectCtx.Err() != nil {
				scanError.Scope = incompleteScope
				scanError.Message = stoppedMessage
			}
			response.Errors = append(response.Errors, scanError)
		}
		// A coverage note qualifies groups read as asked, so it stops nothing. Every account of the
		// deployment can carry the same note; report each once.
		coverageNotes := coverage.take()
		noted := make(map[string]struct{}, len(coverageNotes))
		for _, note := range coverageNotes {
			message := PublicError(note)
			if _, seen := noted[message]; seen {
				continue
			}
			noted[message] = struct{}{}
			response.Errors = append(response.Errors, ScanError{
				Scope:        "coverage",
				ChainID:      job.chainID,
				ProtocolID:   info.ID,
				ProtocolName: info.Name,
				Message:      message,
			})
		}
		if info.ID == walletProtocolID {
			// Provider partial failures are independent observations. Keep each one as a
			// separate scan error instead of errors.Join, whose newline-delimited tail is
			// intentionally removed by PublicError at the service boundary.
			for _, providerErr := range chain.walletProviderErrors {
				response.Errors = append(response.Errors, ScanError{
					Scope:        "protocol",
					ChainID:      job.chainID,
					ProtocolID:   info.ID,
					ProtocolName: info.Name,
					Message:      PublicError(providerErr),
				})
			}
			// A coverage note qualifies holdings that were read as asked: the wallet may
			// lack a token, but every balance it reports is the one at the pin.
			for _, note := range chain.walletCoverage {
				response.Errors = append(response.Errors, ScanError{
					Scope:        "coverage",
					ChainID:      job.chainID,
					ProtocolID:   info.ID,
					ProtocolName: info.Name,
					Message:      PublicError(note),
				})
			}
		}
		if len(groups) > 0 {
			response.Snapshots = append(response.Snapshots, Snapshot{
				ProtocolID:     info.ID,
				ProtocolName:   info.Name,
				ChainID:        job.chainID,
				Account:        address,
				Block:          chain.block,
				Groups:         groups,
				NetValuePolicy: "floor-zero",
			})
		}
		mutex.Unlock()
		elapsed := time.Since(startedAt)
		observer.ObserveProtocol(ProtocolObservation{
			ProtocolID: info.ID,
			ChainID:    job.chainID,
			Duration:   elapsed,
			Groups:     len(groups),
			Err:        redactEndpoints(positionErr),
		})
		log.Printf(
			"portfolio protocol=%s chain=%d duration=%s error=%q",
			info.ID,
			job.chainID,
			elapsed.Round(time.Millisecond),
			PublicError(positionErr),
		)
	}
	protocolsStartedAt := time.Now()
	// Wallet discovery runs beside the workers rather than ahead of them, since only the wallet's
	// own deployments read its answer. Once it is in they all run at once, each on its own chain,
	// apart from the workers, so they never queue behind a slow protocol: a scan stopped at its
	// deadline loses index-backed protocols before it loses the wallet. What reconciles the
	// holdings with the protocols' positions waits for both.
	var wallet sync.WaitGroup
	var walletUnread []deployment
	if len(walletDeployments) > 0 {
		wallet.Add(1)
		go func() {
			defer wallet.Done()
			discoveryStartedAt := time.Now()
			configureWalletBalances(collectCtx, e.walletBalanceProvider, address, chains)
			observation.WalletDiscovery = time.Since(discoveryStartedAt)
			if collectCtx.Err() != nil {
				walletUnread = walletDeployments
				return
			}
			var reads sync.WaitGroup
			for _, job := range walletDeployments {
				reads.Add(1)
				go func() {
					defer reads.Done()
					read(job)
				}()
			}
			reads.Wait()
		}()
	}
	jobs := make(chan deployment)
	var workers sync.WaitGroup
	for worker := 0; worker < e.workers(); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				read(job)
			}
		}()
	}
	dispatched := 0
sendJobs:
	for _, job := range deployments {
		if collectCtx.Err() != nil {
			break
		}
		select {
		case jobs <- job:
			dispatched++
		case <-collectCtx.Done():
			break sendJobs
		}
	}
	close(jobs)
	// A deployment never dispatched will not wait for its protocol's probes either.
	for _, job := range deployments[dispatched:] {
		probes.done(job.Info().ID)
	}
	workers.Wait()
	wallet.Wait()
	observation.Protocols = time.Since(protocolsStartedAt)
	observation.Deployments = dispatched + len(walletDeployments) - len(walletUnread)
	// The deployments the scan never reached are what a stopped scan is missing; name each one
	// that would have run at its chain's pin.
	for _, job := range slices.Concat(deployments[dispatched:], walletUnread) {
		if !job.ActiveAt(job.chainID, chains[job.chainID].block.Number) {
			continue
		}
		info := job.Info()
		response.Errors = append(response.Errors, ScanError{
			Scope:        incompleteScope,
			ChainID:      job.chainID,
			ProtocolID:   info.ID,
			ProtocolName: info.Name,
			Message:      notReadMessage,
		})
	}
	// Holdings are decided once every adapter has finished, so a token an adapter already
	// counted is never reported twice — and never depends on which adapter finished first.
	response.Snapshots = suppressDuplicateHoldings(response.Snapshots)
	sort.Slice(response.Snapshots, func(left, right int) bool {
		if response.Snapshots[left].ProtocolID != response.Snapshots[right].ProtocolID {
			return response.Snapshots[left].ProtocolID < response.Snapshots[right].ProtocolID
		}
		return response.Snapshots[left].ChainID < response.Snapshots[right].ChainID
	})

	pricingStartedAt := time.Now()
	if !options.SkipPrices {
		pricingCtx, cancelPricing := withOptionalDeadline(ctx, options.PricingDeadline)
		prices, priceErrors := fetchPrices(pricingCtx, e.priceProvider, response.Snapshots)
		// Failures once the pricing context has ended mean pricing was cut short, not that tokens
		// cannot be quoted; say so, or the holdings it never priced pass for unquotable ones.
		if len(priceErrors) > 0 && pricingCtx.Err() != nil {
			response.Errors = append(response.Errors, ScanError{Scope: incompleteScope, Message: pricesNotFetchedMessage})
		}
		cancelPricing()
		response.Prices = prices
		response.Errors = append(response.Errors, priceErrors...)
	}
	summaries, valuationErr := applyValuations(
		response.Snapshots,
		response.Prices,
		response.SupportedProtocols,
	)
	if valuationErr != nil {
		response.Errors = append(response.Errors, ScanError{
			Scope: "pricing", Message: PublicError(valuationErr),
		})
	} else {
		response.ProtocolSummaries = summaries
	}
	sort.Slice(response.Errors, func(left, right int) bool {
		leftError := response.Errors[left]
		rightError := response.Errors[right]
		if leftError.Scope != rightError.Scope {
			return leftError.Scope < rightError.Scope
		}
		if leftError.ChainID != rightError.ChainID {
			return leftError.ChainID < rightError.ChainID
		}
		if leftError.ProtocolID != rightError.ProtocolID {
			return leftError.ProtocolID < rightError.ProtocolID
		}
		return leftError.Message < rightError.Message
	})
	response.CompletedAt = time.Now().UTC()
	if !options.SkipPrices {
		observation.Pricing = time.Since(pricingStartedAt)
	}
	observation.Duration = time.Since(scanStartedAt)
	observation.Snapshots = len(response.Snapshots)
	observation.Errors = response.Errors
	for _, scanError := range response.Errors {
		if scanError.Scope == incompleteScope {
			observation.Incomplete++
		}
	}
	observation.Canceled = ctx.Err() != nil
	observer.ObserveScan(observation)
	return response
}
