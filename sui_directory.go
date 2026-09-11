package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// SuiGraphQLDirectory uses the network's general object index only for object
// discovery. It needs no protocol processor or replay of protocol events.
type SuiGraphQLDirectory struct {
	endpoint string
	client   *http.Client
}

func NewSuiGraphQLDirectory(endpoint string) *SuiGraphQLDirectory {
	return &SuiGraphQLDirectory{endpoint: endpoint, client: &http.Client{Timeout: 20 * time.Second}}
}

func (d *SuiGraphQLDirectory) ObjectsByType(ctx context.Context, objectType string) ([]string, error) {
	endpoint, err := url.Parse(d.endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
		return nil, fmt.Errorf("Sui object directory is not configured with an HTTP endpoint")
	}
	typ, err := NormalizeMoveType(objectType)
	if err != nil {
		return nil, err
	}
	var after *string
	seen, cursors := map[string]bool{}, map[string]bool{}
	result := []string{}
	for page := 0; page < suiObjectLimit; page++ {
		body, _ := json.Marshal(map[string]any{"query": `query($type:String!,$after:String){chainIdentifier objects(first:50,after:$after,filter:{type:$type}){nodes{address} pageInfo{hasNextPage endCursor}}}`, "variables": map[string]any{"type": typ, "after": after}})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("invalid Sui directory request")
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := d.client.Do(request)
		if err != nil {
			return nil, redactedError{message: "Sui object directory request failed", cause: err}
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Sui object directory request failed (HTTP %d)", response.StatusCode)
		}
		var payload struct {
			Data struct {
				ChainIdentifier string `json:"chainIdentifier"`
				Objects         *struct {
					Nodes []struct {
						Address string `json:"address"`
					} `json:"nodes"`
					PageInfo *struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"objects"`
			} `json:"data"`
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(data, &payload) != nil || len(payload.Errors) > 0 || payload.Data.Objects == nil || payload.Data.Objects.PageInfo == nil {
			return nil, fmt.Errorf("invalid Sui object directory response")
		}
		if err := checkSuiChainIdentifier(payload.Data.ChainIdentifier, SuiMainnetChainIdentifier); err != nil {
			return nil, err
		}
		objects := payload.Data.Objects
		for _, node := range objects.Nodes {
			id, err := ParseSuiAddress(node.Address)
			if err != nil || seen[id.Hex()] {
				return nil, fmt.Errorf("invalid or duplicate Sui directory object")
			}
			seen[id.Hex()] = true
			result = append(result, id.Hex())
		}
		if len(result) > suiObjectLimit {
			return nil, fmt.Errorf("Sui object directory exceeds limit")
		}
		if !objects.PageInfo.HasNextPage {
			sort.Strings(result)
			return result, nil
		}
		after = objects.PageInfo.EndCursor
		if after == nil || *after == "" || cursors[*after] || len(objects.Nodes) == 0 {
			return nil, fmt.Errorf("invalid Sui directory pagination")
		}
		cursors[*after] = true
	}
	return nil, fmt.Errorf("Sui object directory pagination exceeds limit")
}
