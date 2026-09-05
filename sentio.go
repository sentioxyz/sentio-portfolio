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
)

// Sentio applies a queue limit per API key. All index-backed portfolio adapters therefore share
// one lane across status checks, pagination, and retries instead of limiting concurrency inside
// each protocol independently.
var sentioQueryMu sync.Mutex

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
	apiKey     string
	httpClient *http.Client
	httpMu     sync.Mutex
	statusMu   sync.Mutex
	statuses   map[string]sentioStatusCache
}

func newSentioAPIClient() *sentioAPIClient {
	apiKey := os.Getenv("PORTFOLIO_SENTIO_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("NEXT_PUBLIC_SENTIO_API_KEY")
	}
	httpClient := &http.Client{Timeout: 25 * time.Second}
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		httpClient.Transport = transport.Clone()
	}
	return &sentioAPIClient{
		apiKey: apiKey, httpClient: httpClient,
		statuses: make(map[string]sentioStatusCache),
	}
}

func freshSentioHTTPClient(client *http.Client) *http.Client {
	client.CloseIdleConnections()
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		return client
	}
	fresh := *client
	fresh.Transport = httpTransport.Clone()
	return &fresh
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
	c.httpMu.Lock()
	defer c.httpMu.Unlock()
	var last error
	httpClient := c.httpClient
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
		response, err := httpClient.Do(request)
		transportFailed := err != nil
		if err == nil {
			payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
			response.Body.Close()
			if readErr != nil {
				err = readErr
				transportFailed = true
			} else if response.StatusCode != http.StatusOK {
				err = fmt.Errorf(
					"HTTP %d: %s",
					response.StatusCode,
					strings.TrimSpace(string(payload[:min(len(payload), 300)])),
				)
				if response.StatusCode != http.StatusTooManyRequests &&
					response.StatusCode < http.StatusInternalServerError {
					return redactEndpoints(err)
				}
			} else if decodeErr := json.Unmarshal(payload, result); decodeErr != nil {
				err = decodeErr
			} else {
				return nil
			}
		}
		if transportFailed {
			// A canceled HTTP/2 stream can leave its underlying connection alive but
			// unable to serve subsequent streams. Retrying on that same connection
			// only repeats the timeout, so force the next attempt onto a fresh one.
			httpClient = freshSentioHTTPClient(httpClient)
			c.httpClient = httpClient
		}
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
