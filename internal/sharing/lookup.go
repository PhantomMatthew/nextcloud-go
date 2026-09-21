package sharing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxLookupBody = 1 << 20

// LookupClient queries a Nextcloud lookup server for federated users.
type LookupClient struct {
	BaseURL string       // empty disables lookup
	HTTP    *http.Client // nil -> default 10s timeout client
}

// LookupResult is one federated user hit from the lookup server.
type LookupResult struct {
	FederationID string
	Name         string
}

// Search returns lookup server hits for query. A nil client or empty BaseURL
// yields no hits and no error.
func (c *LookupClient) Search(ctx context.Context, query string) ([]LookupResult, error) {
	if c == nil {
		return nil, nil
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		return nil, nil
	}
	u := base + "/users?search=" + url.QueryEscape(strings.TrimSpace(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil) //nolint:gosec // G704: base is admin-configured; only the escaped search query is user input
	if err != nil {
		return nil, fmt.Errorf("lookup: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc().Do(req) //nolint:gosec // G704: base is admin-configured; only the escaped search query is user input
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("lookup: status %d", resp.StatusCode)
	}
	var rows []struct {
		FederationID string `json:"federationId"`
		Name         string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLookupBody)).Decode(&rows); err != nil {
		return nil, fmt.Errorf("lookup: decode: %w", err)
	}
	out := make([]LookupResult, 0, len(rows))
	for _, row := range rows {
		if row.FederationID == "" {
			continue
		}
		out = append(out, LookupResult{FederationID: row.FederationID, Name: row.Name})
	}
	return out, nil
}

func (c *LookupClient) httpc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}
