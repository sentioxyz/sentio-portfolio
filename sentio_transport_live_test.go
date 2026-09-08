package portfolio

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestSentioTransportNegotiatesHTTP1Live asks a real indexer endpoint which protocol the transport
// ends up speaking. No credentials are involved: an unauthenticated request is answered with a
// 401 or 403, and what matters is that the answer arrives as HTTP/1.1 over an ALPN-negotiated
// http/1.1 connection. A transport that still offered h2 through ALPN would be answered in HTTP/2
// and read the frames as a malformed response; this is the check against the real edge that the
// transport offers only the protocol it speaks.
func TestSentioTransportNegotiatesHTTP1Live(t *testing.T) {
	url := os.Getenv("PORTFOLIO_INDEXER_LIVE_URL")
	if url == "" {
		t.Skip("set PORTFOLIO_INDEXER_LIVE_URL to an indexer status or GraphQL URL to probe the edge")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: newSentioTransport()}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request over the indexer transport failed: %v", err)
	}
	response.Body.Close()
	if response.ProtoMajor != 1 {
		t.Fatalf("response protocol = %s, want HTTP/1.1", response.Proto)
	}
	if response.TLS == nil || response.TLS.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("negotiated ALPN protocol = %q, want http/1.1", response.TLS.NegotiatedProtocol)
	}
	if response.StatusCode >= http.StatusInternalServerError {
		t.Fatalf("edge answered HTTP %d", response.StatusCode)
	}
}
