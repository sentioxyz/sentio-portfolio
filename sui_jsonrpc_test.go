package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// suiRPCTestServer is a scripted Sui JSON-RPC endpoint. It answers by method, can refuse batches
// the way public providers do, and advances its head on every head read so a test can see the
// window a head read is reported with.
type suiRPCTestServer struct {
	t *testing.T

	mu            sync.Mutex
	head          uint64
	headStep      uint64
	rejectBatches bool
	failNext      []int // HTTP statuses to answer before serving normally
	balances      []map[string]any
	metadata      map[string]any // coin type -> metadata object, nil entry -> JSON null
	calls         []string       // method per element, "batch:N" per batch envelope
	requests      int            // HTTP requests received, whether served or failed
}

type suiRPCRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func newSuiRPCTestServer(t *testing.T) *suiRPCTestServer {
	return &suiRPCTestServer{
		t:        t,
		head:     320_000_000,
		headStep: 3,
		metadata: map[string]any{},
	}
}

func (s *suiRPCTestServer) methodCalls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.calls {
		if call == method {
			count++
		}
	}
	return count
}

func (s *suiRPCTestServer) batchCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.calls {
		if strings.HasPrefix(call, "batch:") {
			count++
		}
	}
	return count
}

func (s *suiRPCTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		s.t.Errorf("read request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests++
	if len(s.failNext) > 0 {
		status := s.failNext[0]
		s.failNext = s.failNext[1:]
		s.mu.Unlock()
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte("try later"))
		return
	}
	s.mu.Unlock()
	writer.Header().Set("content-type", "application/json")
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var requests []suiRPCRequest
		if err := json.Unmarshal(body, &requests); err != nil {
			s.t.Errorf("decode batch: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.calls = append(s.calls, fmt.Sprintf("batch:%d", len(requests)))
		reject := s.rejectBatches
		s.mu.Unlock()
		if reject {
			_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32005,"message":"Batched requests are not supported by this server"}}`))
			return
		}
		responses := make([]string, 0, len(requests))
		for _, single := range requests {
			responses = append(responses, s.answer(single))
		}
		_, _ = writer.Write([]byte("[" + strings.Join(responses, ",") + "]"))
		return
	}
	var single suiRPCRequest
	if err := json.Unmarshal(body, &single); err != nil {
		s.t.Errorf("decode request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	_, _ = writer.Write([]byte(s.answer(single)))
}

func (s *suiRPCTestServer) answer(request suiRPCRequest) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, request.Method)
	result := func(value any) string {
		encoded, _ := json.Marshal(value)
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, request.ID, encoded)
	}
	rpcError := func(code int, message string) string {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, request.ID, code, message)
	}
	switch request.Method {
	case "sui_getChainIdentifier":
		return result(SuiMainnetChainIdentifier)
	case "sui_getLatestCheckpointSequenceNumber":
		current := s.head
		s.head += s.headStep
		return result(fmt.Sprint(current))
	case "sui_getCheckpoint":
		var sequence string
		_ = json.Unmarshal(request.Params[0], &sequence)
		if sequence == "999999999999" {
			return rpcError(-32602, "Verified checkpoint not found for sequence number: 999999999999")
		}
		return result(map[string]any{
			"epoch":          "1244",
			"sequenceNumber": sequence,
			"digest":         suiTestDigest,
			"timestampMs":    "1788826290073",
			"transactions":   []string{},
		})
	case "suix_getAllBalances":
		return result(s.balances)
	case "suix_getCoinMetadata":
		var coinType string
		_ = json.Unmarshal(request.Params[0], &coinType)
		if strings.Contains(coinType, "::broken::") {
			return rpcError(-32602, "Invalid struct type")
		}
		entry, exists := s.metadata[coinType]
		if !exists {
			return result(nil)
		}
		return result(entry)
	default:
		return rpcError(-32601, "Method not found")
	}
}

func newSuiRPCTestClient(t *testing.T, server *suiRPCTestServer) (*SuiJSONRPCClient, *httptest.Server) {
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := DialSuiJSONRPC(context.Background(), 0, httpServer.URL, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client, httpServer
}

func TestDialSuiJSONRPCVerifiesTheChainIdentifier(t *testing.T) {
	server := newSuiRPCTestServer(t)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	if _, err := DialSuiJSONRPC(context.Background(), 0, httpServer.URL, "4c78adac"); err == nil ||
		!strings.Contains(err.Error(), SuiMainnetChainIdentifier) {
		t.Fatalf("DialSuiJSONRPC against another network = %v, want an identifier mismatch", err)
	}
	if _, err := DialSuiJSONRPC(context.Background(), 0, "", SuiMainnetChainIdentifier); err == nil {
		t.Fatal("DialSuiJSONRPC accepted an empty endpoint")
	}
	client, err := DialSuiJSONRPC(context.Background(), 0, httpServer.URL, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestSuiJSONRPCHoldingsReportTheHeadWindowAndRefusePins(t *testing.T) {
	server := newSuiRPCTestServer(t)
	server.balances = []map[string]any{
		{"coinType": "0x2::sui::SUI", "coinObjectCount": 25, "totalBalance": "96383083128", "lockedBalance": map[string]any{}},
		{"coinType": "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<0x2::sui::SUI, 0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>", "totalBalance": "1000"},
		{"coinType": "0xdead::spent::SPENT", "totalBalance": "0"},
	}
	client, _ := newSuiRPCTestClient(t, server)
	owner, _ := ParseSuiAddress("0x3cd40490b1e5a583f8717284291c6db97f604bd3d812b6e214591823d985b900")

	holdings, err := client.Holdings(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if holdings.Exact {
		t.Fatal("a JSON-RPC head read claimed to be exact")
	}
	// The head is read once before the balances and once after, and the fake advances it on each
	// read, so the window must be ordered and the reported checkpoint the later observation.
	if holdings.HeadBeforeRead != 320_000_000 || holdings.Checkpoint.Sequence != 320_000_003 {
		t.Fatalf("window = %d..%d, want 320000000..320000003", holdings.HeadBeforeRead, holdings.Checkpoint.Sequence)
	}
	if holdings.Checkpoint.Timestamp.Unix() != 1788826290 || holdings.Checkpoint.Digest == ([32]byte{}) {
		t.Fatalf("checkpoint = %+v", holdings.Checkpoint)
	}
	if len(holdings.Balances) != 2 {
		t.Fatalf("balances = %+v, want SUI and the LP with the zero row dropped", holdings.Balances)
	}
	suiLong := "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"
	if holdings.Balances[0].CoinType != suiLong || holdings.Balances[0].Amount.String() != "96383083128" {
		t.Fatalf("first balance = %+v, want the short SUI type normalized", holdings.Balances[0])
	}
	wantLP := "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<" + suiLong +
		",0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>"
	if holdings.Balances[1].CoinType != wantLP {
		t.Fatalf("LP type = %s, want the spaced generic normalized to %s", holdings.Balances[1].CoinType, wantLP)
	}

	balanceCalls := server.methodCalls("suix_getAllBalances")
	_, err = client.Holdings(context.Background(), owner, &SuiCheckpoint{Sequence: 320_000_000})
	if !errors.Is(err, errSuiPinnedReadUnsupported) {
		t.Fatalf("pinned read error = %v, want errSuiPinnedReadUnsupported", err)
	}
	if server.methodCalls("suix_getAllBalances") != balanceCalls {
		t.Fatal("a refused pinned read still read the head")
	}
}

func TestSuiJSONRPCHoldingsKeepTheWindowOrderedWhenTheHeadReadsBackwards(t *testing.T) {
	server := newSuiRPCTestServer(t)
	server.head = 320_000_010
	server.headStep = ^uint64(0) - 3 // each head read answers four lower, as a lagging backend would
	server.balances = []map[string]any{{"coinType": "0x2::sui::SUI", "totalBalance": "1"}}
	client, _ := newSuiRPCTestClient(t, server)
	owner, _ := ParseSuiAddress("0x1")

	holdings, err := client.Holdings(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if holdings.HeadBeforeRead > holdings.Checkpoint.Sequence {
		t.Fatalf("window %d..%d ends before it starts", holdings.HeadBeforeRead, holdings.Checkpoint.Sequence)
	}
}

func TestSuiJSONRPCCheckpointsResolveAndReportUnknownSequences(t *testing.T) {
	server := newSuiRPCTestServer(t)
	client, _ := newSuiRPCTestClient(t, server)

	latest, err := client.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := decodeBase58(suiTestDigest)
	if latest.Sequence != 320_000_000 || string(latest.Digest[:]) != string(wantDigest) {
		t.Fatalf("latest = %+v", latest)
	}
	pinned, err := client.CheckpointBySequence(context.Background(), 320_000_000)
	if err != nil || pinned.Sequence != 320_000_000 || pinned.Timestamp.Unix() != 1788826290 {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}
	_, err = client.CheckpointBySequence(context.Background(), 999_999_999_999)
	if !errors.Is(err, errSuiCheckpointUnavailable) {
		t.Fatalf("unknown checkpoint error = %v, want errSuiCheckpointUnavailable", err)
	}
	if got := server.methodCalls("sui_getCheckpoint"); got != 3 {
		t.Fatalf("sui_getCheckpoint calls = %d, want 3: a JSON-RPC error must not be retried", got)
	}
}

func TestSuiJSONRPCRetriesTransportFailuresAndRedactsTheEndpoint(t *testing.T) {
	server := newSuiRPCTestServer(t)
	server.balances = []map[string]any{}
	observer := &recordingObserver{}
	ctx := withObserver(context.Background(), observer)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := DialSuiJSONRPC(ctx, Ethereum, httpServer.URL, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}

	server.mu.Lock()
	server.failNext = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
	server.mu.Unlock()
	if _, err := client.LatestCheckpoint(ctx); err != nil {
		t.Fatalf("latest after two retryable statuses: %v", err)
	}
	attempts := 0
	for _, observation := range observer.rpcs {
		if observation.Method == "sui_getLatestCheckpointSequenceNumber" {
			attempts++
			if observation.ChainID != Ethereum {
				t.Fatalf("observation chain = %v", observation.ChainID)
			}
		}
	}
	if attempts != 3 {
		t.Fatalf("head read attempts observed = %d, want 3", attempts)
	}

	server.mu.Lock()
	server.failNext = []int{http.StatusUnauthorized}
	requestsBefore := server.requests
	server.mu.Unlock()
	_, err = client.LatestCheckpoint(ctx)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthorized error = %v", err)
	}
	server.mu.Lock()
	requestsAfter := server.requests
	server.mu.Unlock()
	if requestsAfter-requestsBefore != 1 {
		t.Fatalf("a 401 was followed by %d more requests, want none", requestsAfter-requestsBefore-1)
	}

	host := strings.TrimPrefix(httpServer.URL, "http://")
	httpServer.Close()
	_, err = client.LatestCheckpoint(ctx)
	if err == nil || strings.Contains(err.Error(), host) {
		t.Fatalf("transport error = %v, want a redacted failure", err)
	}
	for _, observation := range observer.rpcs {
		if observation.Err != nil && strings.Contains(observation.Err.Error(), host) {
			t.Fatalf("observation quotes the endpoint: %v", observation.Err)
		}
	}
}

func TestSuiJSONRPCCoinMetadataBatchesAndNamesUnusableCoins(t *testing.T) {
	server := newSuiRPCTestServer(t)
	suiLong := "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"
	blank := "0x000000000000000000000000000000000000000000000000000000000000000a::blank::BLANK"
	nodecimals := "0x000000000000000000000000000000000000000000000000000000000000000b::nod::NOD"
	missing := "0x000000000000000000000000000000000000000000000000000000000000000c::missing::MISSING"
	broken := "0x000000000000000000000000000000000000000000000000000000000000000d::broken::BROKEN"
	server.metadata = map[string]any{
		suiLong:    map[string]any{"decimals": 9, "name": "Sui", "symbol": "SUI", "description": "", "iconUrl": "", "id": "0xf256"},
		blank:      map[string]any{"decimals": 6, "name": "blank", "symbol": "   "},
		nodecimals: map[string]any{"name": "nod", "symbol": "NOD"},
	}
	client, _ := newSuiRPCTestClient(t, server)

	metadata, unusable, err := client.CoinMetadata(context.Background(), []string{suiLong, blank, nodecimals, missing, broken})
	if err != nil {
		t.Fatal(err)
	}
	if server.batchCalls() != 1 {
		t.Fatalf("batch envelopes = %d, want the five coins in one batch", server.batchCalls())
	}
	if len(metadata) != 1 || metadata[suiLong].Symbol != "SUI" || metadata[suiLong].Decimals != 9 {
		t.Fatalf("metadata = %+v", metadata)
	}
	for coinType, want := range map[string]string{
		blank: "empty", nodecimals: "no decimals", missing: "no on-chain metadata", broken: "Invalid struct type",
	} {
		reason, exists := unusable[coinType]
		if !exists || !strings.Contains(reason.Error(), want) {
			t.Errorf("unusable[%s] = %v, want %q", coinType, reason, want)
		}
	}
}

func TestSuiJSONRPCCoinMetadataFallsBackToSingleCallsWhenBatchesAreRejected(t *testing.T) {
	server := newSuiRPCTestServer(t)
	server.rejectBatches = true
	first := "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"
	second := "0x000000000000000000000000000000000000000000000000000000000000000a::a::A"
	server.metadata = map[string]any{
		first:  map[string]any{"decimals": 9, "name": "Sui", "symbol": "SUI"},
		second: map[string]any{"decimals": 6, "name": "a", "symbol": "A"},
	}
	client, _ := newSuiRPCTestClient(t, server)

	metadata, unusable, err := client.CoinMetadata(context.Background(), []string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 2 || len(unusable) != 0 {
		t.Fatalf("metadata = %+v, unusable = %v", metadata, unusable)
	}
	if server.batchCalls() != 1 || server.methodCalls("suix_getCoinMetadata") != 2 {
		t.Fatalf("batches = %d, single metadata calls = %d, want one rejected batch then two singles",
			server.batchCalls(), server.methodCalls("suix_getCoinMetadata"))
	}

	// The rejection is remembered: the next request goes straight to single calls.
	if _, _, err := client.CoinMetadata(context.Background(), []string{first, second}); err != nil {
		t.Fatal(err)
	}
	if server.batchCalls() != 1 || server.methodCalls("suix_getCoinMetadata") != 4 {
		t.Fatalf("after fallback: batches = %d, singles = %d", server.batchCalls(), server.methodCalls("suix_getCoinMetadata"))
	}
}
