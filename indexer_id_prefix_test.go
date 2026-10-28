package portfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A time-travel query narrows the rows the GraphQL server reads only through id, so an
// account-keyed request must bound id by the account's row prefix or it reads the whole table.
func TestAccountKeyedPagesBoundIDByRowPrefix(t *testing.T) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode GraphQL request: %v", err)
		}
		writer.Header().Set("content-type", "application/json")
		// Only the request matters; an error ends the page without further reads.
		_ = json.NewEncoder(writer).Encode(map[string]any{"errors": []map[string]any{{"message": "stop"}}})
	}))
	t.Cleanup(server.Close)
	api := &sentioAPIClient{apiKey: "test", httpClient: server.Client(), statuses: make(map[string]sentioStatusCache)}
	config := SentioIndexerConfig{GraphQLURL: server.URL + "/graphql", StatusURL: server.URL + "/status", ProcessorVersion: "1"}
	ctx := context.Background()
	owner := strings.ToLower(presenceTestAccount.Hex())

	for _, test := range []struct {
		name     string
		send     func() error
		prefix   string
		entities []string
	}{
		{
			name: "morpho",
			send: func() error {
				_, err := (&morphoIndexer{api: api, config: config}).graphqlPage(ctx, Base, presenceTestAccount, "", "", 1000, indexStart{})
				return err
			},
			prefix:   "8453:" + owner + ":",
			entities: []string{"morphoMarketPositionRefs", "morphoVaultPositionRefs"},
		},
		{
			name: "lista",
			send: func() error {
				_, err := (&listaIndexer{api: api, config: config}).graphqlPage(ctx, BSC, presenceTestAccount, "", "", 1000, indexStart{})
				return err
			},
			prefix:   "56:" + owner + ":",
			entities: []string{"listaMarketPositionRefs", "listaVaultPositionRefs"},
		},
		{
			name: "pendle",
			send: func() error {
				_, err := (&pendleIndexer{api: api, config: config}).graphqlPage(ctx, Arbitrum, presenceTestAccount, "", 1000, indexStart{})
				return err
			},
			prefix:   "42161:" + owner + ":",
			entities: []string{"pendlePositionRefs"},
		},
		{
			name: "euler",
			send: func() error {
				_, err := (&eulerIndexer{api: api, config: config}).graphqlPage(ctx, Ethereum, presenceTestAccount, "", "", 1000, indexStart{})
				return err
			},
			prefix:   "1:" + owner[:40] + ":",
			entities: []string{"eulerPositionRefs", "eulerRewardTokens"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.send(); err == nil || !strings.Contains(err.Error(), "stop") {
				t.Fatalf("page error = %v, want the server's", err)
			}
			if request.Variables["idPrefix"] != test.prefix {
				t.Fatalf("idPrefix = %v, want %q", request.Variables["idPrefix"], test.prefix)
			}
			for _, entity := range test.entities {
				if where := entityWhere(request.Query, entity); !strings.Contains(where, "id_starts_with: $idPrefix") {
					t.Fatalf("%s is not bounded by the row prefix: %q", entity, where)
				}
			}
		})
	}
}

// A probe keeps the where clause of the pages it stands in for, prefix included.
func TestPresenceProbesBoundIDByRowPrefix(t *testing.T) {
	owner := strings.ToLower(presenceTestAccount.Hex())
	for _, probe := range []presenceProbe{
		(&morphoIndexer{}).presenceProbe(presenceTestAccount),
		(&pendleIndexer{}).presenceProbe(presenceTestAccount),
	} {
		for _, field := range probe.fields {
			for _, chainID := range []ChainID{Ethereum, Base} {
				want := fmt.Sprintf(`id_starts_with: "%d:%s:"`, chainID, owner)
				if where := field.where(chainID); !strings.Contains(where, want) {
					t.Fatalf("%s %s on chain %d: where %q lacks %s", probe.name, field.entity, chainID, where, want)
				}
			}
		}
	}
}

// entityWhere returns the where argument of entity's field in query.
func entityWhere(query, entity string) string {
	_, field, found := strings.Cut(query, "  "+entity+"(")
	if !found {
		return ""
	}
	_, where, _ := strings.Cut(field, "where: {")
	where, _, _ = strings.Cut(where, "}")
	return where
}
