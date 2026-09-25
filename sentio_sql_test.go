package portfolio

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// asyncSQLHandler serves the async SQL routes from a handler written for the synchronous execute
// endpoint. A submission to …/sql/execute/async runs handle as a request to …/sql/execute: a
// non-200 answer fails the submission itself, and a 200 answer becomes the execution that
// …/sql/query_result/{id} returns, finished with its "result", or its "error" as the execution's
// error, unless "executionStatus" holds it in another state (RUNNING, KILLED). handle runs once
// per submission, so it counts executions.
func asyncSQLHandler(t *testing.T, handle http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	executions := map[string][]byte{}
	return func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/sql/execute/async"):
			inner := request.Clone(request.Context())
			inner.URL.Path = strings.TrimSuffix(request.URL.Path, "/async")
			recorder := httptest.NewRecorder()
			handle(recorder, inner)
			if recorder.Code != http.StatusOK {
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			mu.Lock()
			id := strconv.Itoa(len(executions) + 1)
			executions[id] = recorder.Body.Bytes()
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"executionId": id, "queueLength": 0})
		case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/sql/query_result/"):
			mu.Lock()
			body, ok := executions[path.Base(request.URL.Path)]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var outcome struct {
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
				Status string          `json:"executionStatus"`
			}
			_ = json.Unmarshal(body, &outcome)
			info := map[string]any{"status": "FINISHED"}
			if outcome.Status != "" {
				info["status"] = outcome.Status
			}
			if len(outcome.Result) > 0 {
				info["result"] = outcome.Result
			}
			if len(outcome.Error) > 0 && string(outcome.Error) != "null" {
				info["error"] = string(outcome.Error)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"executionInfo": info})
		case request.Method == http.MethodPut && strings.Contains(request.URL.Path, "/sql/cancel_query/"):
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected SQL request %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// fastSQL polls a test client's statements without the production pacing.
func fastSQL(api *sentioAPIClient) {
	api.sql = sentioSQLTiming{pollInitial: time.Millisecond, pollMax: 5 * time.Millisecond}
}

func TestSentioSQLEndpointsDeriveTheAsyncRoutes(t *testing.T) {
	endpoints, err := newSentioSQLEndpoints("https://indexer.test/api/v1/analytics/owner/project/sql/execute?version=7")
	if err != nil {
		t.Fatal(err)
	}
	if endpoints.submit != "https://indexer.test/api/v1/analytics/owner/project/sql/execute/async?version=7" {
		t.Fatalf("submit = %s", endpoints.submit)
	}
	if got := endpoints.execution("query_result", "e-1", 7); got != "https://indexer.test/api/v1/analytics/owner/project/sql/query_result/e-1?version=7" {
		t.Fatalf("result = %s", got)
	}
	if got := endpoints.execution("cancel_query", "e-1", 7); got != "https://indexer.test/api/v1/analytics/owner/project/sql/cancel_query/e-1?version=7" {
		t.Fatalf("cancel = %s", got)
	}
	for _, invalid := range []string{"https://indexer.test/api/v1/analytics/owner/project/sql", "https://indexer.test/", "::"} {
		if _, err := newSentioSQLEndpoints(invalid); err == nil {
			t.Fatalf("%q accepted", invalid)
		}
	}
}

// sqlExecutionServer is an async SQL endpoint whose executions report statuses in order, the last
// one repeating.
type sqlExecutionServer struct {
	mu          sync.Mutex
	submissions []map[string]any
	polls       []string
	cancels     []string
	statuses    []map[string]any
	submit      func(w http.ResponseWriter) bool
	poll        func(poll int, w http.ResponseWriter) bool
}

func (s *sqlExecutionServer) start(t *testing.T) (*sentioAPIClient, sentioSQLEndpoints) {
	t.Helper()
	t.Setenv("PORTFOLIO_SENTIO_API_KEY", "test-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/sql/execute/async":
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			s.submissions = append(s.submissions, body)
			if s.submit != nil && s.submit(w) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"executionId": "e-1"})
		case request.Method == http.MethodGet && request.URL.Path == "/sql/query_result/e-1":
			s.polls = append(s.polls, request.URL.Query().Get("version"))
			if s.poll != nil && s.poll(len(s.polls), w) {
				return
			}
			status := s.statuses[min(len(s.polls), len(s.statuses))-1]
			_ = json.NewEncoder(w).Encode(map[string]any{"executionInfo": status})
		case request.Method == http.MethodPut && request.URL.Path == "/sql/cancel_query/e-1":
			s.cancels = append(s.cancels, request.URL.Query().Get("version"))
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	endpoints, err := newSentioSQLEndpoints(server.URL + "/sql/execute")
	if err != nil {
		t.Fatal(err)
	}
	api := newSentioAPIClient()
	fastSQL(api)
	return api, endpoints
}

func finishedSQL(rows ...map[string]any) map[string]any {
	return map[string]any{"status": "FINISHED", "result": map[string]any{"rows": rows}}
}

// A statement is submitted once, to the LARGE engine, and its result is polled at the pinned
// version until it finishes: pending (the zero value, left out), running (as a number), finished.
func TestSentioSQLSubmitsOnceOnLargeAndPollsToTheResult(t *testing.T) {
	server := &sqlExecutionServer{statuses: []map[string]any{{}, {"status": 1}, finishedSQL(map[string]any{"rowType": "snapshot"})}}
	api, endpoints := server.start(t)
	observer := &recordingObserver{}
	ctx := withObserver(context.Background(), observer)

	raw, err := api.executeSQL(ctx, endpoints, 7, "SELECT 1", 100)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if json.Unmarshal(raw, &result) != nil || len(result.Rows) != 1 || result.Rows[0]["rowType"] != "snapshot" {
		t.Fatalf("result = %s", raw)
	}
	if len(server.submissions) != 1 {
		t.Fatalf("%d submissions", len(server.submissions))
	}
	body := server.submissions[0]
	query, _ := body["sqlQuery"].(map[string]any)
	if body["engine"] != "LARGE" || body["version"] != float64(7) || query["sql"] != "SELECT 1" || query["size"] != float64(100) {
		t.Fatalf("submission = %v", body)
	}
	if _, sync := body["sync_v1"]; sync {
		t.Fatal("submission asks for the synchronous path")
	}
	if strings.Join(server.polls, ",") != "7,7,7" || len(server.cancels) != 0 {
		t.Fatalf("polls %v, cancels %v", server.polls, server.cancels)
	}
	if len(observer.indexers) != 1 || observer.indexers[0].Kind != IndexerSQL || observer.indexers[0].Err != nil {
		t.Fatalf("observations = %+v", observer.indexers)
	}
}

// A finished error keeps none of the server's message. One that names a ClickHouse execution limit,
// like a killed execution, is too costly, so a range read asks for less; any other is a failure.
func TestSentioSQLFailuresKeepNoServerDetail(t *testing.T) {
	for name, test := range map[string]struct {
		status  map[string]any
		costly  bool
		failure error
	}{
		"limit":  {status: map[string]any{"status": "FINISHED", "error": "Code: 159. DB::Exception: Timeout exceeded at secret-host (TIMEOUT_EXCEEDED)"}, costly: true},
		"memory": {status: map[string]any{"status": 2, "error": "secret-host: (MEMORY_LIMIT_EXCEEDED)"}, costly: true},
		"killed": {status: map[string]any{"status": "KILLED", "error": "execution cancelled before running"}, costly: true},
		"syntax": {status: map[string]any{"status": "FINISHED", "error": "Syntax error at secret-host"}, failure: errSentioSQLFailed},
		"empty":  {status: map[string]any{"status": "FINISHED"}},
	} {
		t.Run(name, func(t *testing.T) {
			server := &sqlExecutionServer{statuses: []map[string]any{test.status}}
			api, endpoints := server.start(t)
			_, err := api.executeSQL(context.Background(), endpoints, 1, "SELECT 1", 100)
			if err == nil || strings.Contains(err.Error(), "secret-host") {
				t.Fatalf("err = %v", err)
			}
			if errors.Is(err, errSentioSQLTooCostly) != test.costly || (test.failure != nil && !errors.Is(err, test.failure)) {
				t.Fatalf("err = %v, costly %v", err, errors.Is(err, errSentioSQLTooCostly))
			}
			if len(server.submissions) != 1 {
				t.Fatalf("%d submissions", len(server.submissions))
			}
		})
	}
}

// A statement still unfinished at its timeout is cancelled and reported as too costly. One whose
// caller gives up is cancelled too, and reports the caller's reason.
func TestSentioSQLCancelsWhatItStopsWaitingFor(t *testing.T) {
	t.Run("statement timeout", func(t *testing.T) {
		server := &sqlExecutionServer{statuses: []map[string]any{{"status": "RUNNING"}}}
		api, endpoints := server.start(t)
		api.sql.statement = 30 * time.Millisecond
		_, err := api.executeSQL(context.Background(), endpoints, 3, "SELECT 1", 100)
		if !errors.Is(err, errSentioSQLTooCostly) || strings.Join(server.cancels, ",") != "3" || len(server.submissions) != 1 {
			t.Fatalf("err %v, cancels %v, %d submissions", err, server.cancels, len(server.submissions))
		}
	})
	t.Run("caller deadline", func(t *testing.T) {
		server := &sqlExecutionServer{statuses: []map[string]any{{"status": "RUNNING"}}}
		api, endpoints := server.start(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := api.executeSQL(ctx, endpoints, 3, "SELECT 1", 100)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errSentioSQLTooCostly) || strings.Join(server.cancels, ",") != "3" {
			t.Fatalf("err %v, cancels %v", err, server.cancels)
		}
	})
}

// A poll that fails transiently is asked again; the submission never is, since asking again would
// execute the statement again.
func TestSentioSQLRetriesPollsButNeverResubmits(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		server := &sqlExecutionServer{statuses: []map[string]any{finishedSQL()}, poll: func(poll int, w http.ResponseWriter) bool {
			if poll == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return true
			}
			return false
		}}
		api, endpoints := server.start(t)
		if _, err := api.executeSQL(context.Background(), endpoints, 1, "SELECT 1", 100); err != nil || len(server.submissions) != 1 || len(server.polls) != 2 {
			t.Fatalf("err %v, %d submissions, %d polls", err, len(server.submissions), len(server.polls))
		}
	})
	t.Run("submission", func(t *testing.T) {
		server := &sqlExecutionServer{statuses: []map[string]any{finishedSQL()}, submit: func(w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}}
		api, endpoints := server.start(t)
		_, err := api.executeSQL(context.Background(), endpoints, 1, "SELECT 1", 100)
		var status sentioHTTPError
		if !errors.As(err, &status) || status.status != http.StatusServiceUnavailable || len(server.submissions) != 1 || len(server.polls) != 0 {
			t.Fatalf("err %v, %d submissions, %d polls", err, len(server.submissions), len(server.polls))
		}
		// The submission may have been accepted, so its failure is no reason to ask for less.
		if errors.Is(err, errSentioSQLTooCostly) || suiSQLRangeTooCostly(err) {
			t.Fatalf("failed submission %v counted as too costly", err)
		}
	})
	t.Run("rejected poll", func(t *testing.T) {
		server := &sqlExecutionServer{statuses: []map[string]any{finishedSQL()}, poll: func(_ int, w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusForbidden)
			return true
		}}
		api, endpoints := server.start(t)
		if _, err := api.executeSQL(context.Background(), endpoints, 1, "SELECT 1", 100); err == nil || len(server.polls) != 1 {
			t.Fatalf("err %v, %d polls", err, len(server.polls))
		}
	})
}
