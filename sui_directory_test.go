package portfolio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSuiDirectoryPaginatesAndRejectsGaps(t *testing.T) {
	for _, mode := range []string{"complete", "wrong chain", "duplicate", "missing page", "repeated cursor", "graphql error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct {
					Variables map[string]any `json:"variables"`
				}
				json.NewDecoder(r.Body).Decode(&request)
				if calls == 2 && request.Variables["after"] != "page2" {
					t.Error("cursor not forwarded")
				}
				id := "0x11"
				if calls > 1 && mode != "duplicate" {
					id = "0x22"
				}
				chain := SuiMainnetChainIdentifier
				if mode == "wrong chain" {
					chain = "00000000"
				}
				page := map[string]any{"hasNextPage": calls == 1, "endCursor": "page2"}
				if mode == "repeated cursor" {
					page["hasNextPage"] = true
				}
				object := map[string]any{"nodes": []map[string]string{{"address": id}}, "pageInfo": page}
				if mode == "missing page" {
					delete(object, "pageInfo")
				}
				result := map[string]any{"data": map[string]any{"chainIdentifier": chain, "objects": object}}
				if mode == "graphql error" {
					result["errors"] = []map[string]string{{"message": "partial"}}
				}
				json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			ids, err := NewSuiGraphQLDirectory(server.URL).ObjectsByType(context.Background(), naviStorageType)
			if mode == "complete" {
				if err != nil || len(ids) != 2 || calls != 2 {
					t.Fatalf("%v %v calls=%d", ids, err, calls)
				}
			} else if err == nil {
				t.Fatal("incomplete directory accepted")
			}
		})
	}
}
