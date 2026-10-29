package portfolio

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChainStatusesIsolatesChainsAndValidatesCache(t *testing.T) {
	for _, test := range []struct {
		name       string
		otherState string
		wantError  string
	}{
		{
			name: "lagging chain error",
			otherState: `,{"chainId":"143","processedBlockNumber":"10","estimatedLatestBlockNumber":"100",
				"status":{"state":"CATCHING_UP","errorRecord":{"message":"RPC unavailable"}}}`,
			wantError: "RPC unavailable",
		},
		{
			name: "malformed other block",
			otherState: `,{"chainId":"143","processedBlockNumber":"invalid","estimatedLatestBlockNumber":"100",
				"status":{"state":"CATCHING_UP"}}`,
			wantError: "invalid block",
		},
		{
			name: "duplicate other chain",
			otherState: `,{"chainId":"143","processedBlockNumber":"10","estimatedLatestBlockNumber":"100","status":{"state":"CATCHING_UP"}},
				{"chainId":"143","processedBlockNumber":"11","estimatedLatestBlockNumber":"100","status":{"state":"CATCHING_UP"}}`,
			wantError: "duplicate chain 143",
		},
		{name: "missing chain after cache fill", wantError: "omitted chain 143"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				_, _ = fmt.Fprintf(w, `{"processors":[{"version":42,"versionState":"PENDING",
					"processorStatus":{"state":"PROCESSING"},"states":[
					{"chainId":"137","processedBlockNumber":"100","estimatedLatestBlockNumber":"100",
					"status":{"state":"PROCESSING_LATEST"}}%s]}]}`, test.otherState)
			}))
			defer server.Close()
			client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
			config := SentioIndexerConfig{GraphQLURL: "https://example.invalid/graphql", StatusURL: server.URL, ProcessorVersion: "42"}
			for _, forceRefresh := range []bool{false, true} {
				statuses, err := client.chainStatusesForScan(context.Background(), config, []ChainID{Polygon, Monad}, Polygon, forceRefresh)
				if err != nil || statuses[Polygon].ProcessedBlock != 100 {
					t.Fatalf("healthy Polygon blocked by another chain: statuses=%+v error=%v", statuses, err)
				}
				if _, err := client.chainStatusesForScan(context.Background(), config, []ChainID{Polygon, Monad}, Monad, false); err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("cached Monad error = %v, want %q", err, test.wantError)
				}
				if _, err := client.chainStatuses(context.Background(), config, []ChainID{Polygon}, false); err != nil {
					t.Fatalf("failed Monad lookup poisoned cached Polygon: %v", err)
				}
			}
			if requests != 2 {
				t.Fatalf("status requests = %d, want one initial fetch and one forced refresh", requests)
			}
		})
	}
}

func TestChainStatusesForScanRejectsUnconfiguredChain(t *testing.T) {
	client := newSentioAPIClient()
	_, err := client.chainStatusesForScan(context.Background(), SentioIndexerConfig{}, []ChainID{Polygon}, Monad, false)
	if err == nil || !strings.Contains(err.Error(), "not configured for chain 143") {
		t.Fatalf("unconfigured chain should be rejected before making an API request: %v", err)
	}
}

func TestChainStatusesAcceptsPinnedPendingVersion(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		versionState string
		status       string
		wantOK       bool
	}{
		{name: "active", versionState: "ACTIVE", status: "PROCESSING", wantOK: true},
		{name: "pending", versionState: "PENDING", status: "PROCESSING", wantOK: true},
		{name: "pending-not-processing", versionState: "PENDING", status: "STOPPED"},
		{name: "draft", versionState: "DRAFT", status: "PROCESSING"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("api-key") != "test-key" {
					t.Fatal("status request omitted the API key")
				}
				writer.Header().Set("content-type", "application/json")
				_, _ = fmt.Fprintf(writer, `{
				  "processors":[{
				    "version":42,
				    "versionState":%q,
				    "processorStatus":{"state":%q},
				    "states":[{
				      "chainId":"137",
				      "processedBlockNumber":"100",
				      "estimatedLatestBlockNumber":"101",
				      "status":{"state":"PROCESSING_LATEST","errorRecord":{"message":""}}
				    }]
				  }]
				}`, testCase.versionState, testCase.status)
			}))
			defer server.Close()

			client := &sentioAPIClient{
				apiKey: "test-key", httpClient: server.Client(),
				statuses: make(map[string]sentioStatusCache),
			}
			statuses, err := client.chainStatuses(context.Background(), SentioIndexerConfig{
				GraphQLURL:       "https://example.invalid/graphql",
				StatusURL:        server.URL,
				ProcessorVersion: "42",
			}, []ChainID{Polygon}, false)
			if !testCase.wantOK {
				if err == nil {
					t.Fatalf("chainStatuses accepted %s/%s: %+v", testCase.versionState, testCase.status, statuses)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := statuses[Polygon]; got.ProcessedBlock != 100 || got.State != "PROCESSING_LATEST" {
				t.Fatalf("Polygon status = %+v", got)
			}
		})
	}
}

// newTestSentioAPIClient builds a client on the production transport, trusting the test server's
// certificate, so these tests exercise the transport the indexers actually use. Only the root
// pool is borrowed from the test server: replacing the whole TLS config would drop the ALPN
// advertisement the transport depends on, and hide exactly the failure the tests must catch.
func newTestSentioAPIClient(server *httptest.Server, timeout time.Duration) *sentioAPIClient {
	transport := newSentioTransport()
	transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return &sentioAPIClient{
		apiKey:     "test-key",
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
		statuses:   make(map[string]sentioStatusCache),
	}
}

// startEdgeLikeTLS starts the test server the way the indexer's edge is configured: HTTP/2
// preferred, HTTP/1.1 offered. httptest's own HTTP/2 mode lists only h2, and Go's TLS server then
// lets an http/1.1-only client through as if it had sent no ALPN at all, which would hide a client
// that stopped advertising its protocol.
func startEdgeLikeTLS(server *httptest.Server) {
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.EnableHTTP2 = true
	server.StartTLS()
}

// TestSentioAPIRetryAbandonsStalledConnection has the first connection the server accepts stall
// every request forever, before the headers or mid-body. The client must time the request out,
// leave that connection behind and succeed on a fresh one. It must also do so over HTTP/1.1
// although the server offers HTTP/2: on a multiplexed connection the stall would have held up the
// protocol's other requests, and the timed-out connection would have stayed in the pool.
func TestSentioAPIRetryAbandonsStalledConnection(t *testing.T) {
	for _, test := range []struct {
		name              string
		stallAfterHeaders bool
	}{
		{name: "awaiting headers"},
		{name: "reading body", stallAfterHeaders: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mutex sync.Mutex
			stalledConnection := ""
			requestCount := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.ProtoMajor != 1 {
					t.Errorf("request protocol = %s, want HTTP/1.1", request.Proto)
				}
				if request.TLS.NegotiatedProtocol != "http/1.1" {
					t.Errorf("negotiated ALPN protocol = %q, want http/1.1 advertised explicitly", request.TLS.NegotiatedProtocol)
				}
				mutex.Lock()
				requestCount++
				if stalledConnection == "" {
					stalledConnection = request.RemoteAddr
				}
				stalled := request.RemoteAddr == stalledConnection
				mutex.Unlock()
				if stalled {
					if test.stallAfterHeaders {
						writer.Header().Set("content-type", "application/json")
						writer.WriteHeader(http.StatusOK)
						writer.(http.Flusher).Flush()
					}
					<-request.Context().Done()
					return
				}
				writer.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(writer).Encode(map[string]bool{"ok": true})
			}))
			startEdgeLikeTLS(server)
			t.Cleanup(server.Close)

			client := newTestSentioAPIClient(server, 25*time.Millisecond)
			var response struct {
				OK bool `json:"ok"`
			}
			if err := client.doJSON(context.Background(), http.MethodGet, server.URL, nil, &response); err != nil {
				t.Fatal(err)
			}
			if !response.OK {
				t.Fatal("retry did not decode the healthy connection response")
			}
			response.OK = false
			if err := client.doJSON(context.Background(), http.MethodGet, server.URL, nil, &response); err != nil {
				t.Fatal(err)
			}
			if !response.OK {
				t.Fatal("subsequent request did not decode")
			}
			mutex.Lock()
			defer mutex.Unlock()
			if requestCount != 3 {
				t.Fatalf("request count = %d, want 3", requestCount)
			}
		})
	}
}

// TestSentioAPIRequestsOverlap proves one client serves concurrent requests concurrently. The
// server holds the first request until the second has arrived, which can only happen while both
// are in flight; a client that serialized its requests would time the first one out, retry, and
// show up at the server three times instead of two.
func TestSentioAPIRequestsOverlap(t *testing.T) {
	var arrivals atomic.Int32
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if arrivals.Add(1) == 2 {
			close(release)
		}
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]bool{"ok": true})
	}))
	startEdgeLikeTLS(server)
	t.Cleanup(server.Close)

	client := newTestSentioAPIClient(server, 2*time.Second)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			var response struct {
				OK bool `json:"ok"`
			}
			err := client.doJSON(context.Background(), http.MethodGet, server.URL, nil, &response)
			if err == nil && !response.OK {
				err = fmt.Errorf("response did not decode")
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
	if got := arrivals.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2", got)
	}
}

// heldStatusServer answers processor status reads for version 42 on Polygon, each only once let
// through, reporting Polygon processed up to 100 plus the read's number so a caller can tell which
// read answered it. A failing server answers 404, which the client does not retry. A read whose
// client gave up before it was let through counts as canceled.
type heldStatusServer struct {
	*httptest.Server
	reads    atomic.Int32
	canceled atomic.Int32
	// letThrough lets one held read answer for every value sent, and every read once closed.
	letThrough chan struct{}
	closeOnce  sync.Once
}

func newHeldStatusServer(t *testing.T, failing bool) *heldStatusServer {
	t.Helper()
	server := &heldStatusServer{letThrough: make(chan struct{})}
	server.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		read := server.reads.Add(1)
		select {
		case <-server.letThrough:
		case <-request.Context().Done():
			server.canceled.Add(1)
			return
		}
		if failing {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(writer).Encode(uniswapIndexerTestStatus("42", []ChainID{Polygon}, 100+uint64(read)))
	}))
	t.Cleanup(server.Close)
	// Cleanups run last in, first out: free the held handlers before the server waits for them.
	t.Cleanup(server.letAllThrough)
	return server
}

func (s *heldStatusServer) letAllThrough() {
	s.closeOnce.Do(func() { close(s.letThrough) })
}

func (s *heldStatusServer) config() SentioIndexerConfig {
	return SentioIndexerConfig{GraphQLURL: s.URL + "/graphql", StatusURL: s.URL + "/status", ProcessorVersion: "42"}
}

// statusAnswer is what one chainStatuses call returned for Polygon.
type statusAnswer struct {
	processed uint64
	err       error
}

func readPolygonStatus(ctx context.Context, client *sentioAPIClient, config SentioIndexerConfig, force bool) statusAnswer {
	statuses, err := client.chainStatuses(ctx, config, []ChainID{Polygon}, force)
	return statusAnswer{processed: statuses[Polygon].ProcessedBlock, err: err}
}

// waitUntil polls condition, since what it observes happens on other goroutines.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !condition(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// assertStillWaiting fails if any caller has answered yet.
func assertStillWaiting(t *testing.T, answers <-chan statusAnswer, what string) {
	t.Helper()
	select {
	case answer := <-answers:
		t.Fatalf("%s answered %+v before the read it waits for did", what, answer)
	case <-time.After(30 * time.Millisecond):
	}
}

// Callers that find one status stale share one read, and the caller that started it can leave
// without ending it: the read runs on, answers everyone still waiting, and fills the cache.
func TestStatusRefreshOutlivesTheCallerThatStartedIt(t *testing.T) {
	server := newHeldStatusServer(t, false)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client()}
	config := server.config()

	starterCtx, cancelStarter := context.WithCancel(context.Background())
	starter := make(chan statusAnswer, 1)
	go func() { starter <- readPolygonStatus(starterCtx, client, config, false) }()
	waitUntil(t, "the starter's read arrives", func() bool { return server.reads.Load() == 1 })
	waiters := make(chan statusAnswer, 3)
	for range cap(waiters) {
		go func() { waiters <- readPolygonStatus(context.Background(), client, config, false) }()
	}
	assertStillWaiting(t, waiters, "a waiter")

	cancelStarter()
	if answer := <-starter; !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("starter = %+v, want its own cancellation", answer)
	}
	assertStillWaiting(t, waiters, "a waiter")
	server.letThrough <- struct{}{}
	for range cap(waiters) {
		if answer := <-waiters; answer.err != nil || answer.processed != 101 {
			t.Fatalf("waiter = %+v, want the one read's block 101", answer)
		}
	}
	if server.reads.Load() != 1 || server.canceled.Load() != 0 {
		t.Fatalf("reads = %d, canceled = %d, want one read that ran to its end", server.reads.Load(), server.canceled.Load())
	}
	if answer := readPolygonStatus(context.Background(), client, config, false); answer.err != nil ||
		answer.processed != 101 || server.reads.Load() != 1 {
		t.Fatalf("next caller = %+v after %d reads, want the cached block 101", answer, server.reads.Load())
	}
}

// A failed read answers everyone who waited on it, so a failing endpoint costs one read rather
// than one per waiter, and is not cached: the next caller reads again.
func TestStatusRefreshSharesItsFailureWithoutCachingIt(t *testing.T) {
	server := newHeldStatusServer(t, true)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client()}
	config := server.config()

	answers := make(chan statusAnswer, 3)
	for range cap(answers) {
		go func() { answers <- readPolygonStatus(context.Background(), client, config, false) }()
	}
	waitUntil(t, "the read arrives", func() bool { return server.reads.Load() == 1 })
	assertStillWaiting(t, answers, "a caller")
	server.letAllThrough()
	for range cap(answers) {
		if answer := <-answers; answer.err == nil || !strings.Contains(answer.err.Error(), "HTTP 404") {
			t.Fatalf("caller = %+v, want the shared read's 404", answer)
		}
	}
	if server.reads.Load() != 1 {
		t.Fatalf("reads = %d, want the one the callers shared", server.reads.Load())
	}
	if answer := readPolygonStatus(context.Background(), client, config, false); answer.err == nil || server.reads.Load() != 2 {
		t.Fatalf("next caller = %+v after %d reads, want a read of its own", answer, server.reads.Load())
	}
}

// The read belongs to no caller, so its own timeout ends it when the endpoint never answers, and
// it says so rather than passing for any caller's deadline.
func TestStatusRefreshEndsAtItsOwnTimeout(t *testing.T) {
	server := newHeldStatusServer(t, false)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client(), statusTimeout: 50 * time.Millisecond}
	startedAt := time.Now()
	answers := make(chan statusAnswer, 1)
	go func() { answers <- readPolygonStatus(context.Background(), client, server.config(), false) }()
	var answer statusAnswer
	select {
	case answer = <-answers:
	case <-time.After(2 * time.Second):
		t.Fatal("the read outlived its own timeout")
	}
	if answer.err == nil || !strings.Contains(answer.err.Error(), "no answer within 50ms") ||
		errors.Is(answer.err, context.DeadlineExceeded) || time.Since(startedAt) > time.Second {
		t.Fatalf("read = %+v after %s, want its own timeout at about 50ms", answer, time.Since(startedAt))
	}
	waitUntil(t, "the abandoned request ends", func() bool { return server.canceled.Load() == 1 })
}

// A forced refresh replaces an entry found too old, so a read already in flight when it was asked
// for cannot answer it: forced callers wait that read out and share the next one.
func TestForcedStatusRefreshIsAnsweredByALaterRead(t *testing.T) {
	server := newHeldStatusServer(t, false)
	client := &sentioAPIClient{apiKey: "test-key", httpClient: server.Client()}
	config := server.config()

	first := make(chan statusAnswer, 1)
	go func() { first <- readPolygonStatus(context.Background(), client, config, false) }()
	waitUntil(t, "the first read arrives", func() bool { return server.reads.Load() == 1 })
	forced := make(chan statusAnswer, 2)
	for range cap(forced) {
		go func() { forced <- readPolygonStatus(context.Background(), client, config, true) }()
	}
	assertStillWaiting(t, forced, "a forced caller")

	server.letThrough <- struct{}{}
	if answer := <-first; answer.err != nil || answer.processed != 101 {
		t.Fatalf("first caller = %+v, want block 101", answer)
	}
	waitUntil(t, "the forced callers' read arrives", func() bool { return server.reads.Load() == 2 })
	assertStillWaiting(t, forced, "a forced caller")
	server.letThrough <- struct{}{}
	for range cap(forced) {
		if answer := <-forced; answer.err != nil || answer.processed != 102 {
			t.Fatalf("forced caller = %+v, want the later read's block 102", answer)
		}
	}
	if server.reads.Load() != 2 {
		t.Fatalf("reads = %d, want the first and one the forced callers shared", server.reads.Load())
	}
}

// Each StatusURL@version refreshes on its own: a read that hangs for one indexer holds up no
// caller of another, as one client serving both Uniswap generations needs.
func TestStatusRefreshesAreKeyedByEndpointAndVersion(t *testing.T) {
	hung := newHeldStatusServer(t, false)
	answering := newHeldStatusServer(t, false)
	answering.letAllThrough()
	client := &sentioAPIClient{apiKey: "test-key", httpClient: hung.Client()}

	go readPolygonStatus(context.Background(), client, hung.config(), false)
	waitUntil(t, "the hung read arrives", func() bool { return hung.reads.Load() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if answer := readPolygonStatus(ctx, client, answering.config(), false); answer.err != nil || answer.processed != 101 {
		t.Fatalf("other indexer = %+v, want its own read's block 101 while the first hangs", answer)
	}
}

// mETH reads its status through the shared client, under the rules every indexer follows: a
// pinned version still PENDING is runnable, while an error record or a malformed latest block on
// Ethereum fails the read, and Ethereum must be at PROCESSING_LATEST.
func TestMethReadsItsStatusLikeEveryIndexer(t *testing.T) {
	for _, test := range []struct {
		name         string
		versionState string
		state        string
		estimated    string
		errorRecord  string
		wantError    string
	}{
		{name: "active", versionState: "ACTIVE", state: "PROCESSING_LATEST", estimated: "120"},
		{name: "pending", versionState: "PENDING", state: "PROCESSING_LATEST", estimated: "120"},
		{name: "draft", versionState: "DRAFT", state: "PROCESSING_LATEST", estimated: "120", wantError: "0 runnable version"},
		{name: "catching up", versionState: "ACTIVE", state: "CATCHING_UP", estimated: "120", wantError: "backfill is incomplete for chain 1"},
		{name: "error record", versionState: "ACTIVE", state: "PROCESSING_LATEST", estimated: "120", errorRecord: "stuck", wantError: "processor chain 1 error: stuck"},
		{name: "malformed latest", versionState: "ACTIVE", state: "PROCESSING_LATEST", estimated: "soon", wantError: "invalid latest block"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(writer, `{"processors":[{"version":1,"versionState":%q,
					"processorStatus":{"state":"PROCESSING"},"states":[{"chainId":"1",
					"processedBlockNumber":"110","estimatedLatestBlockNumber":%q,
					"status":{"state":%q,"errorRecord":{"message":%q}}}]}]}`,
					test.versionState, test.estimated, test.state, test.errorRecord)
			}))
			defer server.Close()
			index := &sentioIndex{
				api:    &sentioAPIClient{apiKey: "test-key", httpClient: server.Client()},
				config: SentioIndexerConfig{GraphQLURL: server.URL, StatusURL: server.URL, ProcessorVersion: "1"},
				chains: []ChainID{Ethereum},
			}
			_, processed, err := index.queryBlock(context.Background(), BlockRef{ChainID: Ethereum, Number: 120})
			if test.wantError == "" {
				if err != nil || processed != 110 {
					t.Fatalf("processed = %d, %v, want block 110", processed, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("processed = %d, %v, want an error naming %q", processed, err, test.wantError)
			}
		})
	}
}
