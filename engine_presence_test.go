package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// scanTestChain serves one chain of a scan: its chain ID, every block read with the pin, and
// eth_call with the answers registered for a contract and calldata, singly or in a batch.
type scanTestChain struct {
	t       *testing.T
	chainID ChainID
	block   uint64
	answers map[contractStubCall]hexutil.Bytes
}

func newScanTestChain(t *testing.T, chainID ChainID, block uint64) *scanTestChain {
	return &scanTestChain{t: t, chainID: chainID, block: block, answers: make(map[contractStubCall]hexutil.Bytes)}
}

// answer makes contract return outputs when it is called with method(args...).
func (c *scanTestChain) answer(contract common.Address, contractABI abi.ABI, method string, args []any, outputs ...any) {
	c.t.Helper()
	data, err := contractABI.Pack(method, args...)
	if err != nil {
		c.t.Fatalf("encode %s call: %v", method, err)
	}
	result, err := contractABI.Methods[method].Outputs.Pack(outputs...)
	if err != nil {
		c.t.Fatalf("encode %s result: %v", method, err)
	}
	c.answers[contractStubCall{contract: contract, data: hexutil.Encode(data)}] = result
}

func (c *scanTestChain) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&raw); err != nil || len(raw) == 0 {
		c.t.Errorf("decode RPC request: %v", err)
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if raw[0] != '[' {
		var call rpcTestRequest
		if err := json.Unmarshal(raw, &call); err != nil {
			c.t.Errorf("decode RPC call: %v", err)
		}
		_ = json.NewEncoder(writer).Encode(c.respond(call))
		return
	}
	var calls []rpcTestRequest
	if err := json.Unmarshal(raw, &calls); err != nil {
		c.t.Errorf("decode RPC batch: %v", err)
	}
	responses := make([]map[string]any, len(calls))
	for index, call := range calls {
		responses[index] = c.respond(call)
	}
	_ = json.NewEncoder(writer).Encode(responses)
}

func (c *scanTestChain) respond(call rpcTestRequest) map[string]any {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	switch call.Method {
	case "eth_chainId":
		response["result"] = hexutil.EncodeUint64(uint64(c.chainID))
		return response
	case "eth_getBlockByNumber":
		response["result"] = map[string]any{
			"number":    hexutil.EncodeUint64(c.block),
			"hash":      common.BytesToHash([]byte{byte(c.chainID), byte(c.block), 1}).Hex(),
			"timestamp": hexutil.EncodeUint64(presenceTestTimestamp),
		}
		return response
	case "eth_call":
		var input struct {
			To   common.Address `json:"to"`
			Data hexutil.Bytes  `json:"data"`
		}
		if len(call.Params) > 0 && json.Unmarshal(call.Params[0], &input) == nil {
			if result, exists := c.answers[contractStubCall{contract: input.To, data: hexutil.Encode(input.Data)}]; exists {
				response["result"] = result
				return response
			}
		}
		c.t.Errorf("unexpected eth_call on chain %d: %s", c.chainID, call.Params)
	default:
		c.t.Errorf("unexpected RPC method %q", call.Method)
	}
	response["error"] = map[string]any{"code": 3, "message": "execution reverted"}
	return response
}

// probeTestIndex serves the status and the presence probes of test indexers. A status read reports
// every chain processed up to the pin; a probe, told apart by the entity it asks about, answers
// that it proves nothing. A held probe answers only once its client gives up, which the index
// records by closing that entity's canceled channel.
type probeTestIndex struct {
	*httptest.Server
	block    uint64
	hold     bool
	entities []string

	mu       sync.Mutex
	probes   map[string][]string
	arrived  chan string
	canceled map[string]chan struct{}
}

func newProbeTestIndex(t *testing.T, block uint64, hold bool, entities ...string) *probeTestIndex {
	t.Helper()
	index := &probeTestIndex{
		block: block, hold: hold, entities: entities,
		probes:   make(map[string][]string),
		arrived:  make(chan string, 16),
		canceled: make(map[string]chan struct{}),
	}
	for _, entity := range entities {
		index.canceled[entity] = make(chan struct{})
	}
	index.Server = httptest.NewServer(index)
	t.Cleanup(index.Close)
	return index
}

func (i *probeTestIndex) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("content-type", "application/json")
	if request.Method == http.MethodGet {
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("1", SupportedChainIDs, i.block))
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)
	entity := ""
	for _, candidate := range i.entities {
		if strings.Contains(body.Query, candidate+"(") {
			entity = candidate
		}
	}
	i.mu.Lock()
	i.probes[entity] = append(i.probes[entity], body.Query)
	i.mu.Unlock()
	i.arrived <- entity
	if i.hold {
		<-request.Context().Done()
		close(i.canceled[entity])
		return
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{}})
}

// probesOf returns the probe queries the index received about entity.
func (i *probeTestIndex) probesOf(entity string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.probes[entity]...)
}

func probeEnded(index *probeTestIndex, entity string) bool {
	select {
	case <-index.canceled[entity]:
		return true
	default:
		return false
	}
}

// probingTestAdapter prefetches a presence probe asking about entity on every chain of the probe,
// and on each chain does what a probing index read does with the verdict: reads the status and
// checks the chain's proof, waiting for the probe. positions, when set, runs instead.
type probingTestAdapter struct {
	adapterBase
	api       *sentioAPIClient
	probe     presenceProbe
	positions func(ctx context.Context, block BlockRef, account common.Address)

	mu    sync.Mutex
	asked []string
}

func newProbingTestAdapter(id, entity string, index *probeTestIndex, chains ...ChainID) *probingTestAdapter {
	return &probingTestAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: id, Name: id, Chains: chains}},
		api:         &sentioAPIClient{apiKey: "test-key", httpClient: index.Client()},
		probe: presenceProbe{
			name: id,
			config: SentioIndexerConfig{
				GraphQLURL: index.URL + "/graphql/" + id, StatusURL: index.URL + "/status/" + id, ProcessorVersion: "1",
			},
			requiredChains: chains,
			fields: []presenceField{{entity: entity, where: func(chainID ChainID) string {
				return fmt.Sprintf("chainId: %d", chainID)
			}}},
		},
	}
}

func (a *probingTestAdapter) prefetchPresence(ctx context.Context, account common.Address) {
	a.api.prefetchPresence(ctx, a.probe, strings.ToLower(account.Hex()))
}

func (a *probingTestAdapter) Positions(
	ctx context.Context,
	_ *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if a.positions != nil {
		a.positions(ctx, block, account)
		return nil, nil
	}
	key := strings.ToLower(account.Hex())
	a.mu.Lock()
	a.asked = append(a.asked, fmt.Sprintf("%d:%s", block.ChainID, key))
	a.mu.Unlock()
	statuses, err := a.api.chainStatuses(ctx, a.probe.config, nil, false)
	if err != nil {
		return nil, err
	}
	a.api.chainProvenEmpty(ctx, a.probe, block, key, statuses)
	return nil, nil
}

// waitingTestAdapter runs wait on every chain job, standing in for an adapter still reading.
type waitingTestAdapter struct {
	adapterBase
	wait func(ctx context.Context)
}

func (a *waitingTestAdapter) Positions(ctx context.Context, _ *RPCClient, _ BlockRef, _ common.Address) ([]Group, error) {
	a.wait(ctx)
	return nil, nil
}

// newPresenceTestEngine scans each chain in rpcs at its pin with adapters, under availability.
func newPresenceTestEngine(
	t *testing.T,
	rpcs []*scanTestChain,
	wallet WalletBalanceProvider,
	availability map[string]chainAvailability,
	adapters ...Adapter,
) *Engine {
	t.Helper()
	urls := make(map[ChainID]string, len(rpcs))
	for _, rpc := range rpcs {
		server := httptest.NewServer(rpc)
		t.Cleanup(server.Close)
		urls[rpc.chainID] = server.URL
	}
	registrations, err := registerAdapters(adapters, availability)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		rpcURLs:               urls,
		adapters:              adapters,
		registrations:         registrations,
		walletBalanceProvider: wallet,
		headLagBlocks:         defaultHeadLagBlocks,
		indexerLane:           newIndexerLane(defaultIndexerConcurrency),
	}
}

// pinnedScan scans exactly the chains of rpcs, each fixed at its block, without pricing.
func pinnedScan(rpcs []*scanTestChain) ScanOptions {
	options := ScanOptions{
		ChainIDs:    make(map[ChainID]struct{}, len(rpcs)),
		BlockNumber: make(map[ChainID]uint64, len(rpcs)),
		SkipPrices:  true,
	}
	for _, rpc := range rpcs {
		options.ChainIDs[rpc.chainID] = struct{}{}
		options.BlockNumber[rpc.chainID] = rpc.block
	}
	return options
}

// A protocol's prefetched probe ends as soon as no deployment of the protocol in the scan is left
// to wait for it — every one finished, or skipped as inactive at its pin — and the index sees the
// request end while the scan still runs. A protocol with a deployment still running keeps its own.
func TestScanEndsAProtocolsProbeOnceNoDeploymentNeedsIt(t *testing.T) {
	for _, ending := range []string{"finished", "inactive"} {
		t.Run(ending, func(t *testing.T) {
			const pin = uint64(1_000)
			chains := []ChainID{BSC, Base, Arbitrum}
			rpcs := []*scanTestChain{newScanTestChain(t, BSC, pin), newScanTestChain(t, Base, pin), newScanTestChain(t, Arbitrum, pin)}
			index := newProbeTestIndex(t, pin, true, "endedRefs", "neededRefs")
			bothInFlight := make(chan struct{})
			go func() {
				for range 2 {
					<-index.arrived
				}
				close(bothInFlight)
			}()

			// The first deployment holds the scan's one worker until both probes are in flight.
			hold := &waitingTestAdapter{
				adapterBase: adapterBase{info: ProtocolInfo{ID: "hold", Name: "hold", Chains: []ChainID{BSC}}},
				wait: func(ctx context.Context) {
					select {
					case <-bothInFlight:
					case <-ctx.Done():
					}
				},
			}
			ended := newProbingTestAdapter("ended", "endedRefs", index, chains...)
			ended.positions = func(context.Context, BlockRef, common.Address) {}
			checked := make(chan struct{})
			verdict := make(chan string, 1)
			gate := &waitingTestAdapter{
				adapterBase: adapterBase{info: ProtocolInfo{ID: "gate", Name: "gate", Chains: []ChainID{BSC}}},
				wait: func(context.Context) {
					defer close(checked)
					select {
					case <-index.canceled["endedRefs"]:
						if probeEnded(index, "neededRefs") {
							verdict <- "the needed protocol's probe ended with the other's"
							return
						}
						verdict <- ""
					case <-time.After(2 * time.Second):
						verdict <- "the ended protocol's probe was still running"
					}
				},
			}
			needed := newProbingTestAdapter("needed", "neededRefs", index, chains...)
			needed.positions = func(context.Context, BlockRef, common.Address) { <-checked }

			availability := map[string]chainAvailability{
				"hold":   {BSC: {availableFromGenesis()}},
				"ended":  {BSC: {availableFromGenesis()}, Base: {availableFromGenesis()}, Arbitrum: {availableFromGenesis()}},
				"gate":   {BSC: {availableFromGenesis()}},
				"needed": {BSC: {availableFromGenesis()}, Base: {availableFromGenesis()}, Arbitrum: {availableFromGenesis()}},
			}
			if ending == "inactive" {
				later := availableFrom(pin + 1)
				availability["ended"] = chainAvailability{BSC: {later}, Base: {later}, Arbitrum: {later}}
			}
			// The deployments are dispatched in adapter order, so the ended protocol's go first
			// once the hold lets the worker go.
			engine := newPresenceTestEngine(t, rpcs, nil, availability, hold, ended, gate, needed)
			engine.scanWorkers = 1
			startedAt := time.Now()
			engine.ScanWithOptions(context.Background(), common.HexToAddress("0x00000000000000000000000000000000000000a1"), pinnedScan(rpcs))
			if problem := <-verdict; problem != "" {
				t.Fatal(problem)
			}
			if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
				t.Fatalf("scan took %s", elapsed)
			}
			if len(index.probesOf("endedRefs")) != 1 || len(index.probesOf("neededRefs")) != 1 {
				t.Fatalf("probes = %d and %d, want one each", len(index.probesOf("endedRefs")), len(index.probesOf("neededRefs")))
			}
			waitUntil(t, "the needed protocol's probe ends with the scan", func() bool { return probeEnded(index, "neededRefs") })
		})
	}
}

// Accounts attributed through Instadapp are read on Ethereum alone, so the scan probes none of
// them: the owner's probe asks about every chain the owner is read on, and the attributed
// account's chain job, finding too few chains to probe, sends nothing for it.
func TestScanProbesNoAccountReadOnTooFewChains(t *testing.T) {
	const pin = uint64(20_000_000)
	owner := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	attributed := common.HexToAddress("0x00000000000000000000000000000000000000d5")
	ethereum := newScanTestChain(t, Ethereum, pin)
	ethereum.answer(instadappList, instadappListABI, "userLink", []any{owner}, uint64(1), uint64(1), uint64(1))
	ethereum.answer(instadappList, instadappListABI, "accountAddr", []any{uint64(1)}, attributed)
	ethereum.answer(instadappList, instadappListABI, "userList", []any{owner, uint64(1)}, uint64(0), uint64(0))
	rpcs := []*scanTestChain{ethereum, newScanTestChain(t, BSC, pin), newScanTestChain(t, Base, pin)}
	index := newProbeTestIndex(t, pin, false, "probedRefs")
	probing := newProbingTestAdapter("probing", "probedRefs", index, Ethereum, BSC, Base)
	engine := newPresenceTestEngine(t, rpcs, nil, map[string]chainAvailability{
		"probing": {Ethereum: {availableFromGenesis()}, BSC: {availableFromGenesis()}, Base: {availableFromGenesis()}},
	}, probing)

	options := pinnedScan(rpcs)
	options.ProtocolIDs = map[string]struct{}{"probing": {}}
	response := engine.ScanWithOptions(context.Background(), owner, options)
	if len(response.Errors) != 0 {
		t.Fatalf("errors = %+v", response.Errors)
	}
	probing.mu.Lock()
	asked := strings.Join(probing.asked, " ")
	probing.mu.Unlock()
	if !strings.Contains(asked, fmt.Sprintf("1:%s", strings.ToLower(attributed.Hex()))) {
		t.Fatalf("chain jobs asked about %s, want the attributed account on Ethereum among them", asked)
	}
	probes := index.probesOf("probedRefs")
	if len(probes) != 1 {
		t.Fatalf("probes = %d, want the owner's alone", len(probes))
	}
	for _, alias := range []string{"cp1:", "cp56:", "cp8453:"} {
		if !strings.Contains(probes[0], alias) {
			t.Fatalf("the owner's probe lacks %s:\n%s", alias, probes[0])
		}
	}
}

func TestPresenceLifetimesEndEachProtocolsProbesOnItsLastDeployment(t *testing.T) {
	ended := make(map[string]int)
	cancel := func(protocolID string) context.CancelFunc { return func() { ended[protocolID]++ } }
	lifetimes := newPresenceLifetimes(
		map[string]context.CancelFunc{"two": cancel("two"), "none": cancel("none")},
		map[string]int{"two": 2, "untracked": 1},
	)
	if ended["none"] != 1 || ended["two"] != 0 {
		t.Fatalf("ended = %v, want only the protocol without deployments", ended)
	}
	lifetimes.done("untracked")
	lifetimes.done("two")
	if ended["two"] != 0 {
		t.Fatalf("probes ended with a deployment left")
	}
	lifetimes.done("two")
	lifetimes.done("two")
	if ended["two"] != 1 {
		t.Fatalf("probes ended %d times, want once, on the last deployment", ended["two"])
	}
}
