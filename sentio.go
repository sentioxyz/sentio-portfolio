package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
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
	sentioRetryInitial   = 300 * time.Millisecond
	sentioStatusCacheTTL = 30 * time.Second

	// sentioIdleConnsPerHost is how many idle connections to the indexer one client keeps warm.
	// The scan workers and the presence prefetcher put at most a handful of requests in flight
	// per protocol; keeping that many connections avoids a TLS handshake per request once the
	// pool has filled.
	sentioIdleConnsPerHost = 8
)

// SentioIndexerConfig is supplied by the host at runtime. Endpoint values may
// contain private project paths and must never be included in public errors.
type SentioIndexerConfig struct {
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

type sentioAPIClient struct {
	apiKey string
	// httpClient is shared by every request and never replaced. Its transport speaks HTTP/1.1
	// only (see newSentioTransport), so requests from the scan workers run concurrently, each on
	// its own connection, with nothing in front of them but the indexer lane.
	httpClient *http.Client
	statusMu   sync.Mutex
	statuses   map[string]sentioStatusCache
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
func newSentioTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.MaxIdleConnsPerHost = sentioIdleConnsPerHost
	return transport
}

func (c *sentioAPIClient) doJSON(
	ctx context.Context,
	method string,
	endpoint string,
	body any,
	result any,
) error {
	if c.apiKey == "" {
		return fmt.Errorf("Sentio API key is not configured")
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		var reader io.Reader
		if body != nil {
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			reader = bytes.NewReader(payload)
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		request.Header.Set("accept", "application/json")
		request.Header.Set("accept-encoding", "identity")
		request.Header.Set("api-key", c.apiKey)
		if body != nil {
			request.Header.Set("content-type", "application/json")
		}
		startedAt := time.Now()
		response, err := c.httpClient.Do(request)
		if err == nil {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			response.Body.Close()
			if readErr != nil {
				err = readErr
			} else if response.StatusCode != http.StatusOK {
				err = fmt.Errorf(
					"HTTP %d: %s",
					response.StatusCode,
					strings.TrimSpace(string(payload[:min(len(payload), 300)])),
				)
				if response.StatusCode != http.StatusTooManyRequests &&
					response.StatusCode < http.StatusInternalServerError {
					observeIndexerRequest(ctx, method, attempt, startedAt, err)
					return redactEndpoints(err)
				}
			} else if decodeErr := json.Unmarshal(payload, result); decodeErr != nil {
				err = decodeErr
			} else {
				observeIndexerRequest(ctx, method, attempt, startedAt, nil)
				return nil
			}
		}
		observeIndexerRequest(ctx, method, attempt, startedAt, err)
		last = err
		if attempt < 2 {
			timer := time.NewTimer(sentioRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("request failed after 3 attempts: %w", redactEndpoints(last))
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

func (c *sentioAPIClient) chainStatuses(
	ctx context.Context,
	config SentioIndexerConfig,
	requiredChains []ChainID,
	forceRefresh bool,
) (map[ChainID]sentioChainStatus, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	cacheKey := config.StatusURL + "@" + config.ProcessorVersion
	if !forceRefresh {
		if cached, exists := c.statuses[cacheKey]; exists && time.Since(cached.At) < sentioStatusCacheTTL {
			return validateSentioChainStatuses(cached.Chains, requiredChains)
		}
	}

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
	c.statuses[cacheKey] = sentioStatusCache{At: time.Now(), Chains: chains}
	return validateSentioChainStatuses(chains, requiredChains)
}
