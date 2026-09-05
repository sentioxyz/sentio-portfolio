package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

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

func TestSentioAPIRetryDropsPoisonedHTTP2Connection(t *testing.T) {
	for _, test := range []struct {
		name              string
		stallAfterHeaders bool
	}{
		{name: "awaiting headers"},
		{name: "reading body", stallAfterHeaders: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mutex sync.Mutex
			poisonedConnection := ""
			requestCount := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.ProtoMajor != 2 {
					t.Errorf("request protocol = %s, want HTTP/2", request.Proto)
				}
				mutex.Lock()
				requestCount++
				if poisonedConnection == "" {
					poisonedConnection = request.RemoteAddr
				}
				poisoned := request.RemoteAddr == poisonedConnection
				mutex.Unlock()
				if poisoned {
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

			httpClient := server.Client()
			httpClient.Timeout = 25 * time.Millisecond
			client := &sentioAPIClient{
				apiKey: "test-key", httpClient: httpClient,
				statuses: make(map[string]sentioStatusCache),
			}
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
				t.Fatal("subsequent request did not retain the healthy transport")
			}
			mutex.Lock()
			defer mutex.Unlock()
			if requestCount != 3 {
				t.Fatalf("request count = %d, want 3", requestCount)
			}
		})
	}
}
