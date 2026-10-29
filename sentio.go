package portfolio

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sentioRetryInitial = 300 * time.Millisecond
	// sentioResponseLimit bounds the body one request may return.
	sentioResponseLimit  = 4 << 20
	sentioStatusCacheTTL = 30 * time.Second
	// sentioStatusRefreshTimeout bounds one processor status refresh, which belongs to no caller
	// and so would otherwise end only with its retries: long enough for an attempt that stalls and
	// a retry, and no longer than the entry it writes stays fresh.
	sentioStatusRefreshTimeout = 30 * time.Second

	// sentioIdleConnsPerHost is how many idle connections to the indexer one client keeps warm.
	// The scan workers and the presence prefetcher put at most a handful of requests in flight
	// per protocol; keeping that many connections avoids a TLS handshake per request once the
	// pool has filled.
	sentioIdleConnsPerHost = 8
)

// SentioIndexerConfig is supplied by the host at runtime. Endpoint values may
// contain private project paths and must never be included in public errors.
type SentioIndexerConfig struct {
	// SQLURL is the version-pinned SQL execute endpoint (…/sql/execute) a Sui
	// protocol index is read through; statements run on the async routes beside
	// it (sentio_sql.go). Empty derives it from GraphQLURL.
	SQLURL           string
	GraphQLURL       string
	StatusURL        string
	ProcessorVersion string
}

func (c SentioIndexerConfig) validate() error {
	if strings.TrimSpace(c.GraphQLURL) == "" {
		return fmt.Errorf("Sentio indexer GraphQL endpoint is not configured")
	}
	if strings.TrimSpace(c.StatusURL) == "" {
		return fmt.Errorf("Sentio indexer status endpoint is not configured")
	}
	if strings.TrimSpace(c.ProcessorVersion) == "" {
		return fmt.Errorf("Sentio indexer processor version is not configured")
	}
	return nil
}

type sentioChainStatus struct {
	State           string
	ProcessedBlock  uint64
	EstimatedLatest uint64
	err             error
}

type sentioStatusCache struct {
	At     time.Time
	Chains map[ChainID]sentioChainStatus
}

// errSentioResponseTooLarge is a body past sentioResponseLimit.
var errSentioResponseTooLarge = errors.New("Sentio response exceeds the size limit")

// sentioHTTPError is a response with a status other than 200.
type sentioHTTPError struct {
	status  int
	message string
}

func (e sentioHTTPError) Error() string { return e.message }

type sentioAPIClient struct {
	apiKey string
	// httpClient is shared by every request and never replaced. Its transport speaks HTTP/1.1
	// only (see newSentioTransport), so requests from the scan workers run concurrently, each on
	// its own connection, with nothing in front of them but the indexer lane — or, for a status
	// read, the one refresh it shares.
	httpClient *http.Client
	// sql paces and bounds SQL statements (sentio_sql.go).
	sql sentioSQLTiming
	// statusTimeout bounds one status refresh; zero takes sentioStatusRefreshTimeout.
	statusTimeout time.Duration
	// statusMu guards statuses and refreshes. It is held only to read or swap an entry, never
	// across a request, so waiting for a status is waiting on a refresh, which a caller can leave.
	statusMu  sync.Mutex
	statuses  map[string]sentioStatusCache
	refreshes map[string]*statusRefresh
}

// statusRefresh is one processor status read, shared by every caller that needs the status while
// it runs. chains and err are set before done closes.
type statusRefresh struct {
	done   chan struct{}
	chains map[ChainID]sentioChainStatus
	err    error
}

func newSentioAPIClient() *sentioAPIClient {
	apiKey := os.Getenv("PORTFOLIO_SENTIO_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("NEXT_PUBLIC_SENTIO_API_KEY")
	}
	return &sentioAPIClient{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 25 * time.Second, Transport: newSentioTransport()},
		statuses:   make(map[string]sentioStatusCache),
		refreshes:  make(map[string]*statusRefresh),
	}
}

// newSentioTransport clones the default transport restricted to HTTP/1.1. Over HTTP/2 every
// request of a protocol is multiplexed onto one connection: a connection whose peer has gone
// silent stalls every stream on it, and once the client times one out the connection stays in
// the pool for the retry to land on again. Recovering from that took a client rotation behind a
// mutex, which serialized every chain of the protocol. With one connection per in-flight request
// a stalled request stalls only itself, and net/http closes a connection whose request timed out
// instead of returning it to the pool, so a retry never lands on it, with no code here to make
// it so.
//
// The TLS config must be replaced, not inherited. Cloning the default transport first runs its
// HTTP/2 setup, which leaves "h2" in the cloned TLSClientConfig.NextProtos, and restricting
// Protocols does not strip it: the client would still offer h2 through ALPN, the edge would take
// it, and the HTTP/1.1 client would read the HTTP/2 frames as a malformed response. Offering only
// http/1.1 is what makes the edge speak it.
func newSentioTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	transport.MaxIdleConnsPerHost = sentioIdleConnsPerHost
	return transport
}

// doJSON sends one request outside the indexer lane: the processor status read, which runs none of
// the key's queries.
func (c *sentioAPIClient) doJSON(
	ctx context.Context,
	method string,
	endpoint string,
	body any,
	result any,
) error {
	return c.doJSONAttempts(ctx, method, endpoint, body, result, 3, nil)
}

// graphql sends one GraphQL query and decodes the data it answers into data. Each attempt holds a
// lane slot for its own round trip and gives it back before a retry waits, so no wait — a status
// read, a presence proof, a retry's backoff, a stand-in's block read — ever holds a slot.
func (c *sentioAPIClient) graphql(ctx context.Context, endpoint, query string, data any) error {
	var envelope graphQLEnvelope
	err := c.doJSONAttempts(ctx, http.MethodPost, endpoint, map[string]any{"query": query}, &envelope, 3, lockSentioLane)
	if err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, graphQLError := range envelope.Errors {
			messages = append(messages, graphQLError.Message)
		}
		return redactEndpoints(fmt.Errorf("GraphQL: %s", strings.Join(messages, "; ")))
	}
	if err := json.Unmarshal(envelope.Data, data); err != nil {
		return fmt.Errorf("GraphQL returned malformed data: %w", err)
	}
	return nil
}

// graphQLEnvelope is a GraphQL response. Each attempt decodes it afresh, so an attempt that failed
// to decode leaves nothing behind for the one that answers after it.
type graphQLEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (e *graphQLEnvelope) UnmarshalJSON(body []byte) error {
	type fields graphQLEnvelope
	var decoded fields
	if err := json.Unmarshal(body, &decoded); err != nil {
		return err
	}
	*e = graphQLEnvelope(decoded)
	return nil
}

// doJSONAttempts sends one request, retrying what may answer differently. admit, when set, is
// called before each attempt and its release after the attempt's round trip.
func (c *sentioAPIClient) doJSONAttempts(
	ctx context.Context,
	method, endpoint string,
	body, result any,
	attempts int,
	admit func(context.Context) (func(), error),
) error {
	if c.apiKey == "" {
		return fmt.Errorf("Sentio API key is not configured")
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		var release func()
		if admit != nil {
			var err error
			if release, err = admit(ctx); err != nil {
				return err
			}
		}
		startedAt := time.Now()
		retry, err := c.roundTrip(ctx, method, endpoint, payload, result)
		if release != nil {
			release()
		}
		observeIndexerRequest(ctx, method, attempt, startedAt, err)
		if err == nil {
			return nil
		}
		if !retry {
			return redactEndpoints(err)
		}
		last = err
		if attempt+1 < attempts {
			timer := time.NewTimer(sentioRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("request failed after %d attempts: %w", attempts, redactEndpoints(last))
}

// roundTrip sends one request with payload as its JSON body and decodes a 200 response into
// result. It neither retries nor observes; retry reports whether another attempt may answer
// differently: a transport, read or decode failure, a 429 or a 5xx. A body past
// sentioResponseLimit is not one of them: a cut body would only fail to decode, and the same
// request answers the same way again.
func (c *sentioAPIClient) roundTrip(ctx context.Context, method, endpoint string, payload []byte, result any) (retry bool, err error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("accept-encoding", "identity")
	request.Header.Set("api-key", c.apiKey)
	if payload != nil {
		request.Header.Set("content-type", "application/json")
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return true, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, sentioResponseLimit+1))
	response.Body.Close()
	switch {
	case err != nil:
		return true, err
	case response.StatusCode != http.StatusOK:
		return response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError,
			sentioHTTPError{status: response.StatusCode, message: fmt.Sprintf(
				"HTTP %d: %s",
				response.StatusCode,
				strings.TrimSpace(string(body[:min(len(body), 300)])),
			)}
	case len(body) > sentioResponseLimit:
		return false, errSentioResponseTooLarge
	}
	if err := json.Unmarshal(body, result); err != nil {
		return true, err
	}
	return false, nil
}

// observeIndexerRequest reports one indexer round trip. The two request shapes the client makes
// are told apart by HTTP method: a processor status read is a GET, a position query is a GraphQL
// POST. The wall-clock covers the request until the body is read.
func observeIndexerRequest(ctx context.Context, method string, attempt int, startedAt time.Time, err error) {
	kind := IndexerGraphQL
	scope := scopeFrom(ctx)
	chainID := scope.chainID
	if method == http.MethodGet {
		kind = IndexerStatus
		chainID = 0
	}
	scope.observer.ObserveIndexer(IndexerObservation{
		ProtocolID: scope.protocolID,
		ChainID:    chainID,
		Kind:       kind,
		Presence:   scope.presence,
		Attempt:    attempt + 1,
		Duration:   time.Since(startedAt),
		Err:        redactEndpoints(err),
	})
}

type sentioIndexerStatusResponse struct {
	Processors []struct {
		Version      any    `json:"version"`
		VersionState string `json:"versionState"`
		Status       struct {
			State string `json:"state"`
		} `json:"processorStatus"`
		States []struct {
			ChainID         string `json:"chainId"`
			ProcessedBlock  string `json:"processedBlockNumber"`
			EstimatedLatest string `json:"estimatedLatestBlockNumber"`
			Status          struct {
				State       string `json:"state"`
				ErrorRecord struct {
					Message string `json:"message"`
				} `json:"errorRecord"`
			} `json:"status"`
		} `json:"states"`
	} `json:"processors"`
}

// A scan needs only its own chain to be ready. Keep the configured-chain bound
// while allowing other chains in the same version to backfill or report errors.
func (c *sentioAPIClient) chainStatusesForScan(
	ctx context.Context,
	config SentioIndexerConfig,
	configuredChains []ChainID,
	chainID ChainID,
	forceRefresh bool,
) (map[ChainID]sentioChainStatus, error) {
	if !supportsChain(configuredChains, chainID) {
		return nil, fmt.Errorf("indexer is not configured for chain %d", chainID)
	}
	return c.chainStatuses(ctx, config, []ChainID{chainID}, forceRefresh)
}

func validateSentioChainStatuses(
	chains map[ChainID]sentioChainStatus,
	requiredChains []ChainID,
) (map[ChainID]sentioChainStatus, error) {
	for _, chainID := range requiredChains {
		status, exists := chains[chainID]
		if !exists {
			return nil, fmt.Errorf("processor status omitted chain %d", chainID)
		}
		if status.err != nil {
			return nil, status.err
		}
	}
	return chains, nil
}

// chainStatuses returns the processor's status for every chain, from an entry younger than
// sentioStatusCacheTTL unless forceRefresh asks for a newer one, and fails if one of
// requiredChains is missing or failed.
//
// An entry is refreshed by one read per StatusURL@version at a time, which every caller needing it
// while it runs shares — its failure too, which is not cached. Each caller waits for the read or
// for its own context, whichever ends first: the read runs on a context of its own, carrying ctx's
// values but not its cancellation and bounded by statusTimeout, so the caller that happened to
// start it can leave without failing the others. The read takes no indexer lane slot: it runs none
// of the key's queries, and every chain job of the processor waits on it, so a read queued behind
// the lane would hold them all back.
func (c *sentioAPIClient) chainStatuses(
	ctx context.Context,
	config SentioIndexerConfig,
	requiredChains []ChainID,
	forceRefresh bool,
) (map[ChainID]sentioChainStatus, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	key := config.StatusURL + "@" + config.ProcessorVersion
	// A forced refresh replaces an entry found too old for the caller's pin, so a read already in
	// flight when it was asked for, which may have been sent before that pin, cannot answer it: the
	// caller waits that read out and shares the next.
	var tooEarly *statusRefresh
	for {
		// A caller already gone starts no read.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.statusMu.Lock()
		if cached, exists := c.statuses[key]; exists && !forceRefresh && time.Since(cached.At) < sentioStatusCacheTTL {
			c.statusMu.Unlock()
			return validateSentioChainStatuses(cached.Chains, requiredChains)
		}
		refresh := c.refreshes[key]
		switch {
		case refresh == nil:
			refresh = c.startStatusRefresh(ctx, key, config)
		case forceRefresh && tooEarly == nil:
			tooEarly = refresh
		}
		c.statusMu.Unlock()
		select {
		case <-refresh.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if refresh == tooEarly {
			continue
		}
		if refresh.err != nil {
			return nil, refresh.err
		}
		return validateSentioChainStatuses(refresh.chains, requiredChains)
	}
}

// startStatusRefresh registers a read of key's status and starts it. The caller holds statusMu.
func (c *sentioAPIClient) startStatusRefresh(
	ctx context.Context,
	key string,
	config SentioIndexerConfig,
) *statusRefresh {
	if c.refreshes == nil {
		c.refreshes = make(map[string]*statusRefresh)
	}
	refresh := &statusRefresh{done: make(chan struct{})}
	c.refreshes[key] = refresh
	timeout := c.statusTimeout
	if timeout <= 0 {
		timeout = sentioStatusRefreshTimeout
	}
	// The read reports under the caller that started it, which is all it keeps of that caller.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	go func() {
		defer cancel()
		chains, err := c.readChainStatuses(readCtx, config)
		if err != nil && readCtx.Err() != nil {
			// The read's own bound, not any caller's deadline, ended it.
			err = fmt.Errorf("processor status: no answer within %s", timeout)
		}
		c.statusMu.Lock()
		if err == nil {
			if c.statuses == nil {
				c.statuses = make(map[string]sentioStatusCache)
			}
			c.statuses[key] = sentioStatusCache{At: time.Now(), Chains: chains}
		}
		delete(c.refreshes, key)
		c.statusMu.Unlock()
		refresh.chains, refresh.err = chains, err
		close(refresh.done)
	}()
	return refresh
}

// readChainStatuses sends one status request and parses every chain of the configured version.
func (c *sentioAPIClient) readChainStatuses(
	ctx context.Context,
	config SentioIndexerConfig,
) (map[ChainID]sentioChainStatus, error) {
	var payload sentioIndexerStatusResponse
	if err := c.doJSON(ctx, http.MethodGet, config.StatusURL, nil, &payload); err != nil {
		return nil, fmt.Errorf("processor status: %w", err)
	}
	matched := 0
	chains := make(map[ChainID]sentioChainStatus)
	for _, processor := range payload.Processors {
		if fmt.Sprint(processor.Version) != config.ProcessorVersion ||
			(processor.VersionState != "ACTIVE" && processor.VersionState != "PENDING") ||
			processor.Status.State != "PROCESSING" {
			continue
		}
		matched++
		for _, state := range processor.States {
			chainNumber, err := strconv.ParseUint(state.ChainID, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("processor returned invalid chain ID %q", state.ChainID)
			}
			chainID := ChainID(chainNumber)
			if previous, duplicate := chains[chainID]; duplicate {
				previous.err = fmt.Errorf("processor returned duplicate chain %d", chainID)
				chains[chainID] = previous
				continue
			}
			chainStatus := sentioChainStatus{State: state.Status.State}
			if state.Status.ErrorRecord.Message != "" {
				chainStatus.err = redactEndpoints(fmt.Errorf("processor chain %d error: %s", chainID, state.Status.ErrorRecord.Message))
				chains[chainID] = chainStatus
				continue
			}
			processed, err := strconv.ParseUint(state.ProcessedBlock, 10, 64)
			if err != nil {
				chainStatus.err = fmt.Errorf("processor chain %d returned invalid block %q", chainID, state.ProcessedBlock)
				chains[chainID] = chainStatus
				continue
			}
			estimated, err := strconv.ParseUint(state.EstimatedLatest, 10, 64)
			if err != nil {
				chainStatus.err = fmt.Errorf("processor chain %d returned invalid latest block %q", chainID, state.EstimatedLatest)
				chains[chainID] = chainStatus
				continue
			}
			chains[chainID] = sentioChainStatus{
				State: state.Status.State, ProcessedBlock: processed, EstimatedLatest: estimated,
			}
		}
	}
	if matched != 1 {
		return nil, fmt.Errorf(
			"processor status returned %d runnable version %s processors",
			matched,
			config.ProcessorVersion,
		)
	}
	return chains, nil
}
