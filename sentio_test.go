package portfolio

import (
	"context"
	"encoding/json"
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
// certificate, so these tests exercise the transport the indexers actually use.
func newTestSentioAPIClient(server *httptest.Server, timeout time.Duration) *sentioAPIClient {
	transport := newSentioTransport()
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return &sentioAPIClient{
		apiKey:     "test-key",
		httpClient: &http.Client{Timeout: timeout, Transport: transport},
		statuses:   make(map[string]sentioStatusCache),
	}
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
			server.EnableHTTP2 = true
			server.StartTLS()
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
	server.EnableHTTP2 = true
	server.StartTLS()
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
