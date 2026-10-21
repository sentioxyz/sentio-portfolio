package portfolio

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// deadlineTestAdapter returns one group holding a token of its own on BSC. A stuck adapter first
// waits for its context to end, like one whose requests never come back.
type deadlineTestAdapter struct {
	adapterBase
	stuck bool
}

func newDeadlineTestAdapter(id string, stuck bool) *deadlineTestAdapter {
	return &deadlineTestAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: id, Name: id, Chains: []ChainID{BSC}}},
		stuck:       stuck,
	}
}

func (a *deadlineTestAdapter) Positions(
	ctx context.Context,
	_ *RPCClient,
	block BlockRef,
	_ common.Address,
) ([]Group, error) {
	if a.stuck {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return []Group{{
		ID:    a.info.ID,
		Label: a.info.Name,
		Components: []Component{{
			Kind: "asset",
			Token: Token{
				ChainID: block.ChainID, Address: common.BytesToAddress([]byte(a.info.ID)), Symbol: "T", Decimals: 18,
			},
			AmountRaw: "1000000000000000000",
		}},
	}}, nil
}

// deadlineTestPrices quotes every token at one dollar or, when stuck, waits for its context to end
// and fails. It records whether each call's context was still live when the call began.
type deadlineTestPrices struct {
	stuck bool
	mu    sync.Mutex
	calls []error
}

func (p *deadlineTestPrices) USDPrices(ctx context.Context, tokens []Token) PriceResult {
	return p.quote(ctx, tokens)
}

func (p *deadlineTestPrices) USDPricesAt(ctx context.Context, tokens []Token, _ time.Time) PriceResult {
	return p.quote(ctx, tokens)
}

func (p *deadlineTestPrices) quote(ctx context.Context, tokens []Token) PriceResult {
	p.mu.Lock()
	p.calls = append(p.calls, ctx.Err())
	p.mu.Unlock()
	if p.stuck {
		<-ctx.Done()
		return PriceResult{Failures: []PriceFailure{{Message: ctx.Err().Error()}}}
	}
	prices := make(map[AssetID]float64, len(tokens))
	for _, token := range tokens {
		prices[AssetForToken(token)] = 1
	}
	return PriceResult{Prices: prices}
}

// newDeadlineTestEngine serves adapters on BSC alone, pinned through rpc.
func newDeadlineTestEngine(
	t *testing.T,
	rpc http.Handler,
	prices PriceProvider,
	observer Observer,
	adapters ...Adapter,
) *Engine {
	t.Helper()
	server := httptest.NewServer(rpc)
	t.Cleanup(server.Close)
	availability := make(map[string]chainAvailability, len(adapters))
	for _, adapter := range adapters {
		availability[adapter.Info().ID] = chainAvailability{BSC: {availableFromGenesis()}}
	}
	registrations, err := registerAdapters(adapters, availability)
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		rpcURLs:       map[ChainID]string{BSC: server.URL},
		adapters:      adapters,
		registrations: registrations,
		priceProvider: prices,
		headLagBlocks: defaultHeadLagBlocks,
		observer:      observer,
		indexerLane:   newIndexerLane(defaultIndexerConcurrency),
	}
}

func deadlineTestOptions(collect, pricing time.Time) ScanOptions {
	return ScanOptions{
		ChainIDs:        map[ChainID]struct{}{BSC: {}},
		BlockNumber:     map[ChainID]uint64{BSC: 1_000},
		CollectDeadline: collect,
		PricingDeadline: pricing,
	}
}

func scanErrorsIn(response *Response, scope string) []ScanError {
	var matching []ScanError
	for _, scanError := range response.Errors {
		if scanError.Scope == scope {
			matching = append(matching, scanError)
		}
	}
	return matching
}

func TestScanStopsCollectingAtItsDeadline(t *testing.T) {
	prices := &deadlineTestPrices{}
	recorder := &recordingObserver{}
	// The wallet registers last, as it does in the engine. Three stuck adapters then hold all three
	// workers until the deadline, so the adapter after them is never dispatched.
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, prices, recorder,
		newDeadlineTestAdapter("quick", false),
		newDeadlineTestAdapter("stuck-1", true),
		newDeadlineTestAdapter("stuck-2", true),
		newDeadlineTestAdapter("stuck-3", true),
		newDeadlineTestAdapter("unreached", false),
		newDeadlineTestAdapter(walletProtocolID, false),
	)
	startedAt := time.Now()
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x1"),
		deadlineTestOptions(startedAt.Add(200*time.Millisecond), startedAt.Add(time.Minute)),
	)
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("scan took %s with a 200ms collection deadline", elapsed)
	}

	read := make(map[string]bool)
	for _, snapshot := range response.Snapshots {
		read[snapshot.ProtocolID] = true
	}
	if len(read) != 2 || !read["quick"] || !read[walletProtocolID] {
		t.Fatalf("snapshots = %+v, want the quick adapter and the wallet, which goes first", read)
	}
	// Pricing runs on its own context after collection stopped, so what was read is still valued.
	if len(prices.calls) != 1 || prices.calls[0] != nil {
		t.Fatalf("price calls = %v, want one made with a live context", prices.calls)
	}
	if len(response.Prices) != 2 || len(response.ProtocolSummaries) != 2 {
		t.Fatalf("prices = %v, summaries = %+v, want both snapshots valued", response.Prices, response.ProtocolSummaries)
	}

	want := map[string]string{
		"stuck-1":   stoppedMessage,
		"stuck-2":   stoppedMessage,
		"stuck-3":   stoppedMessage,
		"unreached": notReadMessage,
	}
	incomplete := scanErrorsIn(response, incompleteScope)
	if len(incomplete) != len(want) {
		t.Fatalf("incomplete errors = %+v, want one for each of %v", incomplete, want)
	}
	for _, scanError := range incomplete {
		if want[scanError.ProtocolID] != scanError.Message || scanError.ChainID != BSC {
			t.Fatalf("incomplete error = %+v, want %q", scanError, want[scanError.ProtocolID])
		}
	}
	for _, scanError := range scanErrorsIn(response, "protocol") {
		// The only protocol error is the wallet's missing discovery provider.
		if scanError.ProtocolID != walletProtocolID {
			t.Fatalf("a stopped adapter was reported as failed: %+v", scanError)
		}
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.scans) != 1 {
		t.Fatalf("scan observations = %d, want 1", len(recorder.scans))
	}
	observation := recorder.scans[0]
	if observation.Incomplete != 4 || observation.Canceled || observation.Deployments != 5 {
		t.Fatalf("observation incomplete = %d, canceled = %t, deployments = %d, want 4, false, 5",
			observation.Incomplete, observation.Canceled, observation.Deployments)
	}
}

func TestScanPricingStopsAtItsOwnDeadline(t *testing.T) {
	prices := &deadlineTestPrices{stuck: true}
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, prices, nil,
		newDeadlineTestAdapter("quick", false),
	)
	startedAt := time.Now()
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x1"),
		deadlineTestOptions(time.Time{}, startedAt.Add(150*time.Millisecond)),
	)
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("scan took %s with a 150ms pricing deadline", elapsed)
	}
	if len(response.Snapshots) != 1 || len(response.Prices) != 0 {
		t.Fatalf("snapshots = %d, prices = %v, want the snapshot read and left unpriced", len(response.Snapshots), response.Prices)
	}
	// The token's own failure stays a pricing error; that pricing was cut short is said apart, about
	// no chain, so the unpriced holding does not pass for an unquotable one.
	incomplete := scanErrorsIn(response, incompleteScope)
	if len(scanErrorsIn(response, "pricing")) == 0 || len(incomplete) != 1 ||
		incomplete[0].Message != pricesNotFetchedMessage || incomplete[0].ChainID != 0 || incomplete[0].ProtocolID != "" {
		t.Fatalf("errors = %+v, want the pricing failure and one incomplete entry for the prices", response.Errors)
	}
}

// A token the provider will not quote is a pricing error, not missing prices.
func TestScanReportsRefusedPricesAsPricingErrorsOnly(t *testing.T) {
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, refusingPrices{}, nil,
		newDeadlineTestAdapter("quick", false),
	)
	startedAt := time.Now()
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x1"),
		deadlineTestOptions(startedAt.Add(time.Minute), startedAt.Add(time.Minute)),
	)
	if len(scanErrorsIn(response, "pricing")) != 1 || len(scanErrorsIn(response, incompleteScope)) != 0 {
		t.Fatalf("errors = %+v, want one pricing error and nothing incomplete", response.Errors)
	}
}

// refusingPrices answers at once that it cannot quote any token.
type refusingPrices struct{}

func (refusingPrices) USDPrices(_ context.Context, tokens []Token) PriceResult {
	failures := make([]PriceFailure, 0, len(tokens))
	for _, token := range tokens {
		asset := AssetForToken(token)
		failures = append(failures, PriceFailure{Asset: &asset, Message: "asset not found"})
	}
	return PriceResult{Failures: failures}
}

func (p refusingPrices) USDPricesAt(ctx context.Context, tokens []Token, _ time.Time) PriceResult {
	return p.USDPrices(ctx, tokens)
}

func TestScanPricingEndsWithTheCaller(t *testing.T) {
	prices := &deadlineTestPrices{stuck: true}
	recorder := &recordingObserver{}
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, prices, recorder,
		newDeadlineTestAdapter("quick", false),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	response := engine.ScanWithOptions(
		ctx,
		common.HexToAddress("0x1"),
		deadlineTestOptions(time.Time{}, startedAt.Add(time.Minute)),
	)
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("scan took %s after its caller gave up at 150ms", elapsed)
	}
	if len(scanErrorsIn(response, "pricing")) == 0 || len(scanErrorsIn(response, incompleteScope)) != 1 {
		t.Fatalf("errors = %+v, want the abandoned pricing reported as such", response.Errors)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.scans) != 1 || !recorder.scans[0].Canceled || recorder.scans[0].Incomplete != 1 {
		t.Fatalf("scan observations = %+v, want one canceled scan missing its prices", recorder.scans)
	}
}

// Every request hangs until its context ends: the scan still returns at its collection deadline,
// reporting the chain it could not set up as missing rather than failed.
func TestScanReturnsAtItsDeadlineWhenRequestsHang(t *testing.T) {
	stop := make(chan struct{})
	hang := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-stop:
		}
	})
	engine := newDeadlineTestEngine(t, hang, &deadlineTestPrices{stuck: true}, nil,
		newDeadlineTestAdapter("stuck", true),
		newDeadlineTestAdapter(walletProtocolID, true),
	)
	// Cleanups run last in, first out: free the hung handlers before the server waits for them.
	t.Cleanup(func() { close(stop) })
	startedAt := time.Now()
	collect := startedAt.Add(150 * time.Millisecond)
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x1"),
		deadlineTestOptions(collect, collect.Add(150*time.Millisecond)),
	)
	if late := time.Since(collect); late > 500*time.Millisecond {
		t.Fatalf("scan returned %s after its collection deadline", late)
	}
	incomplete := scanErrorsIn(response, incompleteScope)
	if len(incomplete) != 1 || incomplete[0].ChainID != BSC || incomplete[0].Message != chainNotSetUpMessage {
		t.Fatalf("incomplete errors = %+v, want the chain that was never set up", incomplete)
	}
	if chainErrors := scanErrorsIn(response, "chain"); len(chainErrors) != 0 {
		t.Fatalf("chain errors = %+v, want none: the deadline, not the chain, stopped the setup", chainErrors)
	}
}

// statusTestServer answers status reads only once released, counting the reads that arrive. The
// answer is a 404, which the clients do not retry.
type statusTestServer struct {
	url      string
	client   *http.Client
	requests atomic.Int32
	release  chan struct{}
}

func newStatusTestServer(t *testing.T) *statusTestServer {
	t.Helper()
	status := &statusTestServer{release: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		status.requests.Add(1)
		select {
		case <-status.release:
		case <-request.Context().Done():
		}
		http.NotFound(writer, request)
	}))
	t.Cleanup(server.Close)
	status.url = server.URL
	status.client = server.Client()
	return status
}

// A caller waiting for a status read that another caller is refreshing gives up when its own
// context ends, without sending a request, however long the refresh takes.
func TestStatusReadsGiveUpWithTheirContext(t *testing.T) {
	for _, reader := range []struct {
		name string
		new  func(*statusTestServer) func(context.Context) error
	}{
		{
			name: "indexer client",
			new: func(server *statusTestServer) func(context.Context) error {
				client := &sentioAPIClient{
					apiKey: "test", httpClient: server.client, statuses: make(map[string]sentioStatusCache),
				}
				config := SentioIndexerConfig{GraphQLURL: server.url, StatusURL: server.url, ProcessorVersion: "1"}
				return func(ctx context.Context) error {
					_, err := client.chainStatuses(ctx, config, nil, false)
					return err
				}
			},
		},
		{
			name: "mETH",
			new: func(server *statusTestServer) func(context.Context) error {
				adapter := &MethAdapter{
					indexer:    SentioIndexerConfig{GraphQLURL: server.url, StatusURL: server.url, ProcessorVersion: "1"},
					apiKey:     "test",
					httpClient: server.client,
				}
				return func(ctx context.Context) error {
					_, err := adapter.processedBlock(ctx)
					return err
				}
			},
		},
	} {
		t.Run(reader.name, func(t *testing.T) {
			server := newStatusTestServer(t)
			read := reader.new(server)
			holder := make(chan error, 1)
			go func() { holder <- read(context.Background()) }()
			for deadline := time.Now().Add(time.Second); server.requests.Load() == 0; {
				if time.Now().After(deadline) {
					t.Fatal("the holder's status read never arrived")
				}
				time.Sleep(time.Millisecond)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			startedAt := time.Now()
			err := read(ctx)
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(startedAt) > time.Second {
				t.Fatalf("waiting read = %v after %s, want the deadline at about 50ms", err, time.Since(startedAt))
			}
			if server.requests.Load() != 1 {
				t.Fatalf("status requests = %d, want only the holder's", server.requests.Load())
			}
			close(server.release)
			if err := <-holder; err == nil {
				t.Fatal("the holder's read succeeded against a failing status endpoint")
			}
		})
	}
}
