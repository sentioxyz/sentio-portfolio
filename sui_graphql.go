package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SuiGraphQLClient reads a Sui network through its GraphQL RPC, the one transport that answers
// "what did this address hold at checkpoint N": `address(atCheckpoint:)` reads holdings at a
// named checkpoint inside the service's consistent range, which is the settled-block contract the
// rest of the kernel keeps. The range is short, about an hour of checkpoints, so a pinned scan
// older than that fails explicitly rather than reading the head.
//
// Mysten runs a public GraphQL service; a deployment can also run its own indexer and GraphQL
// service with a longer consistent range. Neither the JSON-RPC nor the gRPC surface of a fullnode
// offers pinned reads, which is why SuiJSONRPCClient exists alongside this client rather than
// replacing it.
type SuiGraphQLClient struct {
	chainID    ChainID
	endpoint   string
	httpClient *http.Client
}

var _ SuiReader = (*SuiGraphQLClient)(nil)

const (
	suiGraphQLRequestTimeout   = 15 * time.Second
	suiGraphQLAttempts         = 3
	suiGraphQLRetryInitial     = 300 * time.Millisecond
	suiGraphQLMaxResponseBytes = 8 << 20
	// suiBalancePageSize is the largest page the GraphQL service serves for Address.balances.
	suiBalancePageSize = 50
	// suiMaxBalancePages bounds one address's holdings enumeration. Past it the read fails
	// rather than returning a partial inventory, as the Uniswap NFT enumeration does at its own
	// bound: a truncated list of holdings is a wrong portfolio nobody can see is wrong.
	suiMaxBalancePages = 200
	// suiMetadataBatchSize is how many coin types one metadata request aliases together.
	suiMetadataBatchSize = 25
)

// DialSuiGraphQL connects to a Sui GraphQL endpoint and verifies it serves the network named by
// chainIdentifier (SuiMainnetChainIdentifier for mainnet). chainID is the kernel's identity for
// that network and only labels observations; the endpoint is never quoted in errors.
func DialSuiGraphQL(
	ctx context.Context,
	chainID ChainID,
	endpoint string,
	chainIdentifier string,
) (*SuiGraphQLClient, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("Sui GraphQL endpoint is not configured")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 8
	client := &SuiGraphQLClient{
		chainID:    chainID,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: suiGraphQLRequestTimeout, Transport: transport},
	}
	var payload struct {
		ChainIdentifier string `json:"chainIdentifier"`
	}
	if _, err := client.query(ctx, "SuiChainIdentifier",
		`query SuiChainIdentifier { chainIdentifier }`, nil, &payload); err != nil {
		return nil, fmt.Errorf("read Sui chain identifier: %w", err)
	}
	if err := checkSuiChainIdentifier(payload.ChainIdentifier, chainIdentifier); err != nil {
		return nil, err
	}
	return client, nil
}

func (c *SuiGraphQLClient) Close() {
	c.httpClient.CloseIdleConnections()
}

type suiGraphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

type suiGraphQLError struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

func (e suiGraphQLError) Error() string {
	if len(e.Path) == 0 {
		return e.Message
	}
	parts := make([]string, 0, len(e.Path))
	for _, element := range e.Path {
		parts = append(parts, fmt.Sprint(element))
	}
	return strings.Join(parts, ".") + ": " + e.Message
}

type suiGraphQLResponse struct {
	Data   json.RawMessage   `json:"data"`
	Errors []suiGraphQLError `json:"errors"`
}

func retryableSuiStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// query runs one GraphQL operation. Transport failures and retryable HTTP statuses are retried;
// a response whose data is null is an error; a response carrying data alongside path-scoped
// errors decodes the data and hands the errors back for the caller to classify, because the
// service answers a partly failed selection that way.
func (c *SuiGraphQLClient) query(
	ctx context.Context,
	operation string,
	graphql string,
	variables map[string]any,
	result any,
) ([]suiGraphQLError, error) {
	body, err := json.Marshal(suiGraphQLRequest{
		Query: graphql, OperationName: operation, Variables: variables,
	})
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < suiGraphQLAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("content-type", "application/json")
		request.Header.Set("accept", "application/json")
		startedAt := time.Now()
		graphqlErrors, err := c.doQuery(request, result)
		err = describeSuiTransportError(err)
		observeSuiRequest(ctx, c.chainID, "graphql "+operation, 0, attempt, startedAt, err)
		if err == nil {
			return graphqlErrors, nil
		}
		last = err
		var httpErr suiHTTPError
		if errors.As(err, &httpErr) && !retryableSuiStatus(httpErr.status) {
			return nil, redactEndpoints(err)
		}
		var responseErr suiResponseError
		if errors.As(err, &responseErr) {
			return nil, redactEndpoints(err)
		}
		if attempt+1 < suiGraphQLAttempts {
			c.httpClient.CloseIdleConnections()
			if err := waitSuiRetry(ctx, suiGraphQLRetryInitial<<attempt); err != nil {
				return nil, err
			}
		}
	}
	return nil, redactEndpoints(fmt.Errorf("Sui GraphQL failed after %d attempts: %w", suiGraphQLAttempts, last))
}

// waitSuiRetry sleeps for a retry backoff, or returns early when ctx ends.
func waitSuiRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// describeSuiTransportError names what went wrong on the wire without quoting where. A url.Error
// spells out the endpoint and a dial error the address it resolved to, and redactEndpoints only
// knows URLs, so the message keeps the operation and the innermost cause and the original error
// stays reachable through errors.Is and errors.As.
func describeSuiTransportError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	if urlErr.Timeout() {
		return redactedError{message: "Sui " + urlErr.Op + " timed out", cause: err}
	}
	cause := urlErr.Err
	var opErr *net.OpError
	if errors.As(cause, &opErr) && opErr.Err != nil {
		cause = opErr.Err
	}
	return redactedError{message: "Sui " + urlErr.Op + " failed: " + cause.Error(), cause: err}
}

// suiHTTPError is a non-200 response. Only 429 and 5xx are retried.
type suiHTTPError struct {
	status int
	body   string
}

func (e suiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

// suiResponseError is a well-formed GraphQL response that answered the operation with errors and
// no data. Retrying would only repeat the same rejection.
type suiResponseError struct {
	errors []suiGraphQLError
}

func (e suiResponseError) Error() string {
	messages := make([]string, 0, len(e.errors))
	for _, graphqlErr := range e.errors {
		messages = append(messages, graphqlErr.Error())
	}
	return "Sui GraphQL: " + strings.Join(messages, "; ")
}

func (c *SuiGraphQLClient) doQuery(request *http.Request, result any) ([]suiGraphQLError, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, suiGraphQLMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > suiGraphQLMaxResponseBytes {
		return nil, fmt.Errorf("Sui GraphQL response exceeds %d bytes", suiGraphQLMaxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, suiHTTPError{
			status: response.StatusCode,
			body:   strings.TrimSpace(string(payload[:min(len(payload), 300)])),
		}
	}
	var envelope suiGraphQLResponse
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode Sui GraphQL response: %w", err)
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		if len(envelope.Errors) == 0 {
			return nil, errors.New("Sui GraphQL response carried neither data nor errors")
		}
		return nil, suiResponseError{errors: envelope.Errors}
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return nil, fmt.Errorf("decode Sui GraphQL data: %w", err)
	}
	return envelope.Errors, nil
}

// observeSuiRequest reports one round trip of either Sui transport to the scan's observer under
// the adapter the context names. GraphQL operations report as "graphql <operation>", JSON-RPC
// calls as their method, so a host can tell the transports apart in its metrics.
func observeSuiRequest(
	ctx context.Context,
	chainID ChainID,
	method string,
	batchSize int,
	attempt int,
	startedAt time.Time,
	err error,
) {
	scope := scopeFrom(ctx)
	scope.observer.ObserveRPC(RPCObservation{
		ChainID:    chainID,
		ProtocolID: scope.protocolID,
		Method:     method,
		BatchSize:  batchSize,
		Attempt:    attempt + 1,
		Duration:   time.Since(startedAt),
		Err:        redactEndpoints(err),
	})
}

type suiGraphQLCheckpoint struct {
	SequenceNumber uint64 `json:"sequenceNumber"`
	Digest         string `json:"digest"`
	Timestamp      string `json:"timestamp"`
}

func (p suiGraphQLCheckpoint) checkpoint() (SuiCheckpoint, error) {
	timestamp, err := time.Parse(time.RFC3339Nano, p.Timestamp)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d timestamp: %w", p.SequenceNumber, err)
	}
	return newSuiCheckpoint(p.SequenceNumber, p.Digest, timestamp)
}

const suiCheckpointSelection = `{ sequenceNumber digest timestamp }`

// LatestCheckpoint is the newest checkpoint the service has indexed, which is the head a live
// scan pins itself to. The service only answers reads at checkpoints it has fully processed, so
// unlike an EVM pool there is no advertised-but-unserved head to lag behind.
func (c *SuiGraphQLClient) LatestCheckpoint(ctx context.Context) (SuiCheckpoint, error) {
	var payload struct {
		Checkpoint *suiGraphQLCheckpoint `json:"checkpoint"`
	}
	if _, err := c.query(ctx, "SuiLatestCheckpoint",
		`query SuiLatestCheckpoint { checkpoint `+suiCheckpointSelection+` }`, nil, &payload); err != nil {
		return SuiCheckpoint{}, err
	}
	if payload.Checkpoint == nil {
		return SuiCheckpoint{}, errors.New("Sui GraphQL returned no latest checkpoint")
	}
	return payload.Checkpoint.checkpoint()
}

// CheckpointBySequence resolves a fixed checkpoint. A sequence the service does not know is
// errSuiCheckpointUnavailable.
func (c *SuiGraphQLClient) CheckpointBySequence(ctx context.Context, sequence uint64) (SuiCheckpoint, error) {
	var payload struct {
		Checkpoint *suiGraphQLCheckpoint `json:"checkpoint"`
	}
	if _, err := c.query(ctx, "SuiCheckpoint",
		`query SuiCheckpoint($sequence: UInt53!) { checkpoint(sequenceNumber: $sequence) `+
			suiCheckpointSelection+` }`,
		map[string]any{"sequence": sequence}, &payload); err != nil {
		return SuiCheckpoint{}, err
	}
	if payload.Checkpoint == nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d: %w", sequence, errSuiCheckpointUnavailable)
	}
	checkpoint, err := payload.Checkpoint.checkpoint()
	if err != nil {
		return SuiCheckpoint{}, err
	}
	if checkpoint.Sequence != sequence {
		return SuiCheckpoint{}, fmt.Errorf(
			"Sui GraphQL returned checkpoint %d for sequence %d", checkpoint.Sequence, sequence,
		)
	}
	return checkpoint, nil
}

// BalancesRange reports the checkpoints, inclusive, at which the service can answer
// Address.balances consistently. Fixed-checkpoint scans outside it cannot be served.
func (c *SuiGraphQLClient) BalancesRange(ctx context.Context) (first uint64, last uint64, err error) {
	var payload struct {
		ServiceConfig struct {
			AvailableRange struct {
				First *struct {
					SequenceNumber uint64 `json:"sequenceNumber"`
				} `json:"first"`
				Last *struct {
					SequenceNumber uint64 `json:"sequenceNumber"`
				} `json:"last"`
			} `json:"availableRange"`
		} `json:"serviceConfig"`
	}
	if _, err := c.query(ctx, "SuiBalancesRange",
		`query SuiBalancesRange { serviceConfig { availableRange(type: "Address", field: "balances") `+
			`{ first { sequenceNumber } last { sequenceNumber } } } }`, nil, &payload); err != nil {
		return 0, 0, err
	}
	available := payload.ServiceConfig.AvailableRange
	if available.First == nil || available.Last == nil {
		return 0, 0, errors.New("Sui GraphQL reported no consistent range for balances")
	}
	if available.Last.SequenceNumber < available.First.SequenceNumber {
		return 0, 0, fmt.Errorf(
			"Sui GraphQL consistent range %d-%d ends before it starts",
			available.First.SequenceNumber, available.Last.SequenceNumber,
		)
	}
	return available.First.SequenceNumber, available.Last.SequenceNumber, nil
}

const suiBalancesQuery = `query SuiBalances($owner: SuiAddress!, $checkpoint: UInt53!, $first: Int!, $after: String) {
  address(address: $owner, atCheckpoint: $checkpoint) {
    balances(first: $first, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { coinType { repr } totalBalance }
    }
  }
}`

// Holdings reads owner's balances at pin, or at the latest checkpoint when pin is nil. Either way
// the result is Exact: the GraphQL service scopes every page to the checkpoint's sequence number.
func (c *SuiGraphQLClient) Holdings(
	ctx context.Context,
	owner SuiAddress,
	pin *SuiCheckpoint,
) (SuiHoldings, error) {
	var checkpoint SuiCheckpoint
	if pin != nil {
		checkpoint = *pin
	} else {
		latest, err := c.LatestCheckpoint(ctx)
		if err != nil {
			return SuiHoldings{}, err
		}
		checkpoint = latest
	}
	balances, err := c.Balances(ctx, checkpoint, owner)
	if err != nil {
		return SuiHoldings{}, err
	}
	return SuiHoldings{
		Balances:       balances,
		Checkpoint:     checkpoint,
		Exact:          true,
		HeadBeforeRead: checkpoint.Sequence,
	}, nil
}

// Balances enumerates every coin type owner held at the checkpoint, summed across coin objects
// and address balances, sorted by normalized coin type. The read is scoped to the checkpoint's
// sequence number on every page, so a holding never mixes two states of the chain; a checkpoint
// the service no longer holds state for is errSuiOutsideConsistentRange.
func (c *SuiGraphQLClient) Balances(
	ctx context.Context,
	checkpoint SuiCheckpoint,
	owner SuiAddress,
) ([]SuiCoinBalance, error) {
	rows := make([]suiBalanceRow, 0)
	var after *string
	for page := 0; ; page++ {
		if page >= suiMaxBalancePages {
			return nil, fmt.Errorf(
				"address holds more than %d coin types", suiMaxBalancePages*suiBalancePageSize,
			)
		}
		variables := map[string]any{
			"owner": owner.Hex(), "checkpoint": checkpoint.Sequence, "first": suiBalancePageSize,
		}
		if after != nil {
			variables["after"] = *after
		}
		var payload struct {
			Address *struct {
				Balances *struct {
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						CoinType struct {
							Repr string `json:"repr"`
						} `json:"coinType"`
						TotalBalance string `json:"totalBalance"`
					} `json:"nodes"`
				} `json:"balances"`
			} `json:"address"`
		}
		graphqlErrors, err := c.query(ctx, "SuiBalances", suiBalancesQuery, variables, &payload)
		if err != nil {
			return nil, err
		}
		if len(graphqlErrors) > 0 {
			return nil, classifySuiBalanceErrors(checkpoint.Sequence, graphqlErrors)
		}
		if payload.Address == nil || payload.Address.Balances == nil {
			// The service returns an address with no balances as an empty page; a missing
			// selection without an error is a contract it does not make.
			return nil, errors.New("Sui GraphQL returned no balances selection")
		}
		for _, node := range payload.Address.Balances.Nodes {
			rows = append(rows, suiBalanceRow{coinType: node.CoinType.Repr, amount: node.TotalBalance})
		}
		pageInfo := payload.Address.Balances.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		if pageInfo.EndCursor == nil || *pageInfo.EndCursor == "" {
			return nil, errors.New("Sui GraphQL reported another balances page without a cursor")
		}
		if after != nil && *after == *pageInfo.EndCursor {
			return nil, errors.New("Sui GraphQL repeated a balances cursor")
		}
		after = pageInfo.EndCursor
	}
	return suiBalancesFrom(rows)
}

func classifySuiBalanceErrors(sequence uint64, graphqlErrors []suiGraphQLError) error {
	joined := make([]error, 0, len(graphqlErrors))
	for _, graphqlErr := range graphqlErrors {
		if strings.Contains(strings.ToLower(graphqlErr.Message), "outside consistent range") {
			return fmt.Errorf("checkpoint %d: %w", sequence, errSuiOutsideConsistentRange)
		}
		joined = append(joined, graphqlErr)
	}
	return fmt.Errorf("Sui GraphQL balances at checkpoint %d: %w", sequence, errors.Join(joined...))
}

// CoinMetadata reads the on-chain metadata of the given coin types, several per request through
// aliases. Coin types must already be normalized.
func (c *SuiGraphQLClient) CoinMetadata(
	ctx context.Context,
	coinTypes []string,
) (map[string]SuiCoinMetadata, map[string]error, error) {
	metadata := make(map[string]SuiCoinMetadata, len(coinTypes))
	unusable := make(map[string]error)
	for start := 0; start < len(coinTypes); start += suiMetadataBatchSize {
		end := min(start+suiMetadataBatchSize, len(coinTypes))
		batch := coinTypes[start:end]
		var declarations, selections strings.Builder
		variables := make(map[string]any, len(batch))
		for index, coinType := range batch {
			name := "t" + strconv.Itoa(index)
			if index > 0 {
				declarations.WriteString(", ")
			}
			declarations.WriteString("$" + name + ": String!")
			selections.WriteString(" m" + strconv.Itoa(index) + ": coinMetadata(coinType: $" + name +
				") { decimals symbol name }")
			variables[name] = coinType
		}
		graphql := "query SuiCoinMetadata(" + declarations.String() + ") {" + selections.String() + " }"
		var payload map[string]*struct {
			Decimals *int    `json:"decimals"`
			Symbol   *string `json:"symbol"`
			Name     string  `json:"name"`
		}
		graphqlErrors, err := c.query(ctx, "SuiCoinMetadata", graphql, variables, &payload)
		if err != nil {
			return nil, nil, err
		}
		if len(graphqlErrors) > 0 {
			joined := make([]error, 0, len(graphqlErrors))
			for _, graphqlErr := range graphqlErrors {
				joined = append(joined, graphqlErr)
			}
			return nil, nil, fmt.Errorf("Sui GraphQL coin metadata: %w", errors.Join(joined...))
		}
		for index, coinType := range batch {
			entry, present := payload["m"+strconv.Itoa(index)]
			if !present || entry == nil {
				unusable[coinType] = errors.New("coin has no on-chain metadata")
				continue
			}
			validated, err := suiCoinMetadataFrom(coinType, entry.Decimals, entry.Symbol, entry.Name)
			if err != nil {
				unusable[coinType] = err
				continue
			}
			metadata[coinType] = validated
		}
	}
	return metadata, unusable, nil
}
