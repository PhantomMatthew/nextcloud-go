package wopi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNoDiscoveryAction reports that the Collabora discovery document offers
// no edit/view action for the requested extension.
var ErrNoDiscoveryAction = errors.New("wopi: no discovery action for extension")

// Discovery fetches and caches the Collabora WOPI discovery document
// ({collabora_url}/hosting/discovery): the per-extension action URLs the
// viewer page posts the access token to. The fetch target is admin-owned
// config, never request input. Collabora recommends host-side caching, so
// the parsed action map refreshes at most once per CacheTTL.
type Discovery struct {
	BaseURL string // office.collabora_url
	HTTP    *http.Client
	Clock   func() time.Time
	// CacheTTL bounds how long a parsed document is reused; <=0 selects
	// defaultDiscoveryCacheTTL.
	CacheTTL time.Duration

	mu        sync.Mutex
	actions   map[string]discoveryAction
	fetchedAt time.Time
}

const (
	defaultDiscoveryCacheTTL = time.Hour
	defaultDiscoveryTimeout  = 10 * time.Second
	maxDiscoveryBody         = 4 << 20
)

// discoveryAction holds one extension's editor URLs.
type discoveryAction struct {
	edit string
	view string
}

func (d *Discovery) clock() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

func (d *Discovery) cacheTTL() time.Duration {
	if d.CacheTTL > 0 {
		return d.CacheTTL
	}
	return defaultDiscoveryCacheTTL
}

func (d *Discovery) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: defaultDiscoveryTimeout}
}

// ActionURL returns the discovery urlsrc for ext (lowercase, no dot):
// the edit action when edit is allowed and offered, else the view action,
// else the edit action as the final fallback (a read-only token still
// blocks writes server-side).
func (d *Discovery) ActionURL(ctx context.Context, ext string, edit bool) (string, error) {
	if d == nil || strings.TrimSpace(d.BaseURL) == "" {
		return "", errors.New("wopi: discovery without collabora url")
	}
	actions, err := d.load(ctx)
	if err != nil {
		return "", err
	}
	a, ok := actions[ext]
	if !ok {
		return "", ErrNoDiscoveryAction
	}
	u := a.edit
	if !edit {
		u = a.view
	}
	if u == "" {
		u = a.edit
	}
	if u == "" {
		u = a.view
	}
	if u == "" {
		return "", ErrNoDiscoveryAction
	}
	return u, nil
}

// load returns the cached action map, refetching when stale. The lock is
// held across the fetch so a cold cache single-flights naturally.
func (d *Discovery) load(ctx context.Context) (map[string]discoveryAction, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.actions != nil && d.clock().Sub(d.fetchedAt) < d.cacheTTL() {
		return d.actions, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(d.BaseURL, "/")+"/hosting/discovery", nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("wopi: discovery fetch: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || len(body) > maxDiscoveryBody {
		return nil, fmt.Errorf("wopi: discovery status %d", resp.StatusCode)
	}
	actions, err := parseDiscovery(body)
	if err != nil {
		return nil, err
	}
	d.actions = actions
	d.fetchedAt = d.clock()
	return actions, nil
}

type discoveryXML struct {
	Zones []struct {
		Apps []struct {
			Actions []struct {
				Ext    string `xml:"ext,attr"`
				Name   string `xml:"name,attr"`
				URLSrc string `xml:"urlsrc,attr"`
			} `xml:"action"`
		} `xml:"app"`
	} `xml:"net-zone"`
}

// parseDiscovery maps extension → {edit, view} urlsrc across every
// net-zone (first zone wins per extension/action pair, matching upstream's
// external-zone preference in practice). Actions without an ext (app-level
// defaults) or without a urlsrc are skipped.
func parseDiscovery(body []byte) (map[string]discoveryAction, error) {
	var doc discoveryXML
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("wopi: discovery parse: %w", err)
	}
	actions := map[string]discoveryAction{}
	for _, zone := range doc.Zones {
		for _, app := range zone.Apps {
			for _, a := range app.Actions {
				ext := strings.ToLower(strings.TrimSpace(a.Ext))
				if ext == "" || a.URLSrc == "" {
					continue
				}
				cur := actions[ext]
				switch a.Name {
				case "edit":
					if cur.edit == "" {
						cur.edit = a.URLSrc
					}
				case "view":
					if cur.view == "" {
						cur.view = a.URLSrc
					}
				}
				actions[ext] = cur
			}
		}
	}
	if len(actions) == 0 {
		return nil, errors.New("wopi: discovery document has no usable actions")
	}
	return actions, nil
}
