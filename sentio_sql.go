package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sentio SQL runs asynchronously on the LARGE engine. A statement is submitted once to the async
// execute endpoint and its execution is polled until it finishes, so a statement may run longer
// than one HTTP request may wait, and a submission is never repeated: asking again would execute
// the statement again.
const (
	sentioSQLEngine = "LARGE"
	// sentioSQLStatementTimeout bounds one statement from submission to result, within whatever
	// deadline the caller's context already carries. A statement still unfinished then is
	// cancelled and reported as too costly, so a range read asks for fewer samples.
	sentioSQLStatementTimeout = 2 * time.Minute
	// Polls start at sentioSQLPollInitial and double to sentioSQLPollMax. The cap bounds how long
	// a finished result waits to be seen: statements usually take a second or two, and a poll only
	// reads the execution's row.
	sentioSQLPollInitial = 100 * time.Millisecond
	sentioSQLPollMax     = 500 * time.Millisecond
	// sentioSQLCancelTimeout bounds the best-effort cancellation of a statement given up on.
	sentioSQLCancelTimeout = 5 * time.Second
)

// sentioSQLTiming paces and bounds the client's SQL statements. A zero field takes its default.
type sentioSQLTiming struct {
	pollInitial, pollMax, statement time.Duration
}

// errSentioSQLTooCostly is a statement that ended for its size rather than its content: it did not
// finish within sentioSQLStatementTimeout, was killed, or failed on a ClickHouse execution limit.
// A range read answers it by asking for fewer samples.
var errSentioSQLTooCostly = errors.New("Sentio SQL statement exceeded an execution limit")

// errSentioSQLFailed is a statement that finished with an error. The server's message is not
// kept: it may name the deployment behind the index.
var errSentioSQLFailed = errors.New("Sentio SQL statement failed")

// sentioSQLLimitErrors are the ClickHouse error names of a statement too large to execute.
var sentioSQLLimitErrors = []string{"TIMEOUT_EXCEEDED", "MEMORY_LIMIT_EXCEEDED", "TOO_MANY_ROWS_OR_BYTES"}

// sentioSQLEndpoints are the async routes of one project's SQL, derived from its execute endpoint
// (…/sql/execute): submission at …/sql/execute/async, and an execution's result and
// cancellation at …/sql/query_result/{id} and …/sql/cancel_query/{id}.
type sentioSQLEndpoints struct {
	submit string
	// base is …/sql, carrying the execute endpoint's query.
	base url.URL
}

func newSentioSQLEndpoints(execute string) (sentioSQLEndpoints, error) {
	endpoint, err := url.Parse(execute)
	if err != nil || !strings.HasSuffix(endpoint.Path, "/sql/execute") {
		return sentioSQLEndpoints{}, fmt.Errorf("invalid Sentio SQL endpoint")
	}
	submit, base := *endpoint, *endpoint
	submit.Path, submit.RawPath = endpoint.Path+"/async", ""
	base.Path, base.RawPath = strings.TrimSuffix(endpoint.Path, "/execute"), ""
	return sentioSQLEndpoints{submit: submit.String(), base: base}, nil
}

// execution is the route of an execution's result or cancellation, pinned to version.
func (e sentioSQLEndpoints) execution(route, executionID string, version uint64) string {
	endpoint := e.base
	endpoint.Path += "/" + route + "/" + executionID
	query := endpoint.Query()
	query.Set("version", strconv.FormatUint(version, 10))
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

// sentioSQLExecution is the part of a query_result response the client reads.
type sentioSQLExecution struct {
	ExecutionInfo struct {
		// Status is PENDING, RUNNING, FINISHED or KILLED, rendered as a name or a number, and
		// absent while pending: PENDING is the enum's zero value.
		Status json.RawMessage `json:"status"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	} `json:"executionInfo"`
}

func (e sentioSQLExecution) status() string {
	var name string
	if json.Unmarshal(e.ExecutionInfo.Status, &name) == nil {
		return strings.ToUpper(name)
	}
	var number int
	if json.Unmarshal(e.ExecutionInfo.Status, &number) == nil {
		switch number {
		case 1:
			return "RUNNING"
		case 2:
			return "FINISHED"
		case 3:
			return "KILLED"
		}
	}
	return "PENDING"
}

// executeSQL submits statement against version once and waits for it to finish, returning the
// result's tabular data as the service encodes it. A poll that fails transiently is asked again;
// the submission never is. A statement given up on, because it outlived its timeout or ctx
// ended, is cancelled before returning.
func (c *sentioAPIClient) executeSQL(
	ctx context.Context,
	endpoints sentioSQLEndpoints,
	version uint64,
	statement string,
	rowLimit int,
) (result json.RawMessage, err error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("Sentio API key is not configured")
	}
	startedAt := time.Now()
	defer func() { observeIndexerSQL(ctx, startedAt, err) }()
	timing := c.sqlTiming()
	statementCtx, cancel := context.WithTimeout(ctx, timing.statement)
	defer cancel()

	payload, err := json.Marshal(map[string]any{
		"version":  version,
		"sqlQuery": map[string]any{"sql": statement, "size": rowLimit},
		"engine":   sentioSQLEngine,
	})
	if err != nil {
		return nil, err
	}
	var submitted struct {
		ExecutionID string `json:"executionId"`
	}
	if _, err := c.roundTrip(statementCtx, http.MethodPost, endpoints.submit, payload, &submitted); err != nil {
		return nil, redactEndpoints(err)
	}
	if submitted.ExecutionID == "" {
		return nil, fmt.Errorf("Sentio SQL statement was not accepted")
	}

	resultURL := endpoints.execution("query_result", submitted.ExecutionID, version)
	for wait := timing.pollInitial; ; wait = min(2*wait, timing.pollMax) {
		timer := time.NewTimer(wait)
		select {
		case <-statementCtx.Done():
			timer.Stop()
			return nil, c.abandonSQL(ctx, endpoints, submitted.ExecutionID, version)
		case <-timer.C:
		}
		var execution sentioSQLExecution
		retry, err := c.roundTrip(statementCtx, http.MethodGet, resultURL, nil, &execution)
		if err != nil {
			if statementCtx.Err() != nil {
				return nil, c.abandonSQL(ctx, endpoints, submitted.ExecutionID, version)
			}
			if retry {
				continue
			}
			return nil, redactEndpoints(err)
		}
		switch execution.status() {
		case "FINISHED":
			info := execution.ExecutionInfo
			switch {
			case info.Error != "":
				for _, limit := range sentioSQLLimitErrors {
					if strings.Contains(info.Error, limit) {
						return nil, errSentioSQLTooCostly
					}
				}
				return nil, errSentioSQLFailed
			case len(info.Result) == 0 || string(info.Result) == "null":
				return nil, fmt.Errorf("Sentio SQL statement finished without a result")
			}
			return info.Result, nil
		case "KILLED":
			return nil, errSentioSQLTooCostly
		}
	}
}

// abandonSQL cancels an execution the client stopped waiting for, so it does not run on for
// nobody, and reports why it stopped: ctx's own error when the caller's deadline or cancellation
// ended it, errSentioSQLTooCostly when the statement outlived its own timeout.
func (c *sentioAPIClient) abandonSQL(ctx context.Context, endpoints sentioSQLEndpoints, executionID string, version uint64) error {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sentioSQLCancelTimeout)
	defer cancel()
	var ignored json.RawMessage
	_, _ = c.roundTrip(cancelCtx, http.MethodPut, endpoints.execution("cancel_query", executionID, version), nil, &ignored)
	if err := ctx.Err(); err != nil {
		return err
	}
	return errSentioSQLTooCostly
}

func (c *sentioAPIClient) sqlTiming() sentioSQLTiming {
	timing := c.sql
	if timing.pollInitial <= 0 {
		timing.pollInitial = sentioSQLPollInitial
	}
	if timing.pollMax <= 0 {
		timing.pollMax = sentioSQLPollMax
	}
	if timing.statement <= 0 {
		timing.statement = sentioSQLStatementTimeout
	}
	return timing
}

// observeIndexerSQL reports one SQL statement, from submission to result, whatever number of
// polls it took.
func observeIndexerSQL(ctx context.Context, startedAt time.Time, err error) {
	scope := scopeFrom(ctx)
	scope.observer.ObserveIndexer(IndexerObservation{
		ProtocolID: scope.protocolID,
		ChainID:    scope.chainID,
		Kind:       IndexerSQL,
		Attempt:    1,
		Duration:   time.Since(startedAt),
		Err:        redactEndpoints(err),
	})
}
