package portfolio

import (
	"context"
	"errors"
	"fmt"
	"log"
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
}

// defaultHeadLagBlocks is how far behind the advertised head a live scan pins itself. Four blocks
// was enough to remove the failure on every chain measured; see LatestSettledBlock for why the
// margin is needed at all.
const defaultHeadLagBlocks = 4

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
}

func (o ScanOptions) empty() bool {
	return len(o.ProtocolIDs) == 0 &&
		len(o.ChainIDs) == 0 &&
		len(o.BlockNumber) == 0 &&
		!o.SkipPrices
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
	return &Engine{
		rpcURLs:               rpcURLs,
		adapters:              adapters,
		registrations:         registrations,
		priceProvider:         priceProvider,
		walletBalanceProvider: config.WalletBalanceProvider,
		headLagBlocks:         config.headLagBlocks(),
	}
}

func (e *Engine) Protocols() []ProtocolInfo {
	result := make([]ProtocolInfo, 0, len(e.adapters))
	for _, adapter := range e.adapters {
		result = append(result, adapter.Info())
	}
	return result
}

type chainScan struct {
	client                 *RPCClient
	block                  BlockRef
	accounts               []attributedAccount
	walletProviderAccounts map[common.Address]walletProviderAccount
	walletProviderErrors   []error
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
	for _, chainID := range SupportedChainIDs {
		if !options.includesChain(chainID) {
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
			client, err := DialRPC(ctx, chainID, url)
			if err != nil {
				mutex.Lock()
				response.Errors = append(response.Errors, ScanError{
					Scope: "chain", ChainID: chainID, Message: PublicError(err),
				})
				mutex.Unlock()
				return
			}
			var block BlockRef
			blockNumber, fixedBlock := options.BlockNumber[chainID]
			if fixedBlock {
				block, err = client.BlockByNumber(ctx, blockNumber)
				block.Fixed = true
			} else {
				block, err = client.LatestSettledBlock(ctx, e.headLagBlocks)
			}
			if err != nil {
				client.Close()
				mutex.Lock()
				response.Errors = append(response.Errors, ScanError{
					Scope: "chain", ChainID: chainID, Message: PublicError(err),
				})
				mutex.Unlock()
				return
			}
			accounts, err := resolveAccountScope(ctx, client, block, address)
			if err != nil {
				client.Close()
				mutex.Lock()
				response.Errors = append(response.Errors, ScanError{
					Scope: "chain", ChainID: chainID, Message: PublicError(err),
				})
				mutex.Unlock()
				return
			}
			mutex.Lock()
			chains[chainID] = &chainScan{client: client, block: block, accounts: accounts}
			response.ChainBlocks[chainID] = block.Number
			mutex.Unlock()
		}(chainID)
	}
	wait.Wait()
	if options.includesProtocol(walletProtocolID) {
		configureWalletBalances(ctx, e.walletBalanceProvider, address, chains)
	}
	defer func() {
		for _, chain := range chains {
			chain.client.Close()
		}
	}()

	type deployment struct {
		registeredAdapter
		chainID ChainID
	}
	deployments := make([]deployment, 0)
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
			if !options.includesChain(chainID) {
				continue
			}
			deployments = append(deployments, deployment{
				registeredAdapter: registration,
				chainID:           chainID,
			})
		}
	}
	jobs := make(chan deployment)
	var workers sync.WaitGroup
	for worker := 0; worker < 3; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobs {
				chain := chains[job.chainID]
				if chain == nil {
					continue
				}
				info := job.Info()
				if !job.ActiveAt(job.chainID, chain.block.Number) {
					continue
				}
				startedAt := time.Now()
				groups := make([]Group, 0)
				var positionErr error
				for _, account := range chain.accounts {
					var accountGroups []Group
					var err error
					providerAccount, useProvider := chain.walletProviderAccounts[account.Address]
					if info.ID == walletProtocolID && useProvider {
						accountGroups, err = providerWalletGroups(
							ctx,
							chain.client,
							chain.block,
							job.chainID,
							account.Address,
							providerAccount,
						)
					} else {
						accountGroups, err = job.Positions(
							ctx,
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
				mutex.Lock()
				if positionErr != nil {
					response.Errors = append(response.Errors, ScanError{
						Scope:        "protocol",
						ChainID:      job.chainID,
						ProtocolID:   info.ID,
						ProtocolName: info.Name,
						Message:      PublicError(positionErr),
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
				log.Printf(
					"portfolio protocol=%s chain=%d duration=%s error=%q",
					info.ID,
					job.chainID,
					time.Since(startedAt).Round(time.Millisecond),
					PublicError(positionErr),
				)
			}
		}()
	}
sendJobs:
	for _, job := range deployments {
		select {
		case jobs <- job:
		case <-ctx.Done():
			break sendJobs
		}
	}
	close(jobs)
	workers.Wait()
	// Holdings are decided once every adapter has finished, so a token an adapter already
	// counted is never reported twice — and never depends on which adapter finished first.
	response.Snapshots = suppressDuplicateHoldings(response.Snapshots)
	sort.Slice(response.Snapshots, func(left, right int) bool {
		if response.Snapshots[left].ProtocolID != response.Snapshots[right].ProtocolID {
			return response.Snapshots[left].ProtocolID < response.Snapshots[right].ProtocolID
		}
		return response.Snapshots[left].ChainID < response.Snapshots[right].ChainID
	})

	if !options.SkipPrices {
		prices, priceErrors := fetchPrices(ctx, e.priceProvider, response.Snapshots)
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
	return response
}
