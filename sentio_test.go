package portfolio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
