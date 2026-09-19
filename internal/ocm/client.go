package ocm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	maxOCMBody    = 1 << 20
	maxRemoteFile = 32 << 20
)

// Client talks to a remote OCM endpoint.
type Client struct {
	HTTP *http.Client
}

var _ files.RemoteFile = (*Client)(nil)

// NewClient returns a Client with a 15s timeout and same-host redirects.
func NewClient() *Client {
	return &Client{HTTP: newHTTPClient()}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: sameHostRedirect,
	}
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return fmt.Errorf("%w: too many redirects", ErrInvalid)
	}
	if len(via) > 0 && !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
		return fmt.Errorf("%w: redirect host", ErrInvalid)
	}
	return nil
}

func (c *Client) httpc() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return newHTTPClient()
}

// OutgoingNotice is the JSON posted to a remote POST /ocm/shares.
type OutgoingNotice struct {
	ShareWith    string
	Name         string
	ProviderID   string
	Owner        string
	Sender       string
	ResourceType string
	Token        string
}

// Discover returns the remote OCM API endPoint.
func (c *Client) Discover(ctx context.Context, origin string) (string, error) {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return "", fmt.Errorf("%w: origin", ErrInvalid)
	}
	urls := []string{origin + "/.well-known/ocm", origin + "/ocm-provider"}
	var last error
	for _, u := range urls {
		ep, err := c.discoverURL(ctx, u)
		if err == nil {
			return ep, nil
		}
		last = err
	}
	if last == nil {
		last = ErrInvalid
	}
	return "", last
}

func (c *Client) discoverURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOCMBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: discovery status %d", ErrInvalid, resp.StatusCode)
	}
	var doc discoveryDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("%w: discovery json", ErrInvalid)
	}
	if !doc.Enabled || strings.TrimSpace(doc.EndPoint) == "" {
		return "", fmt.Errorf("%w: discovery endpoint", ErrInvalid)
	}
	return strings.TrimRight(strings.TrimSpace(doc.EndPoint), "/"), nil
}

// NotifyOutgoing posts a share-creation notification to the remote endPoint.
func (c *Client) NotifyOutgoing(ctx context.Context, endPoint string, n OutgoingNotice) error {
	endPoint = strings.TrimRight(strings.TrimSpace(endPoint), "/")
	if endPoint == "" || n.ShareWith == "" || n.Name == "" || n.ProviderID == "" || n.Token == "" {
		return fmt.Errorf("%w: notify", ErrInvalid)
	}
	payload, err := json.Marshal(outgoingShareJSON{
		ShareWith:    n.ShareWith,
		Name:         n.Name,
		ProviderID:   n.ProviderID,
		Owner:        n.Owner,
		Sender:       n.Sender,
		ShareType:    "user",
		ResourceType: n.ResourceType,
		Protocol: outgoingProtocolJSON{
			Name:    "webdav",
			Options: outgoingProtocolOptionsJSON{SharedSecret: n.Token},
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endPoint+"/shares", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOCMBody))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: notify status %d", ErrInvalid, resp.StatusCode)
	}
	return nil
}

// SplitCloudID splits user@remote into uid and remote.
func SplitCloudID(id string) (uid, remote string) {
	s := strings.TrimSpace(id)
	i := strings.LastIndex(s, "@")
	if i <= 0 || i == len(s)-1 {
		return "", ""
	}
	return s[:i], s[i+1:]
}

// NormalizeOrigin turns a remote host or URL into an origin without a trailing slash.
func NormalizeOrigin(remote string) string {
	remote = strings.TrimSpace(strings.TrimRight(remote, "/"))
	if remote == "" {
		return ""
	}
	if strings.Contains(remote, "://") {
		return remote
	}
	return "https://" + remote
}

// Get fetches a federated file from the sender's public WebDAV.
func (c *Client) Get(ctx context.Context, origin, token, rel string) (io.ReadCloser, *webdav.Entry, error) {
	origin = NormalizeOrigin(origin)
	if origin == "" || token == "" {
		return nil, nil, fmt.Errorf("%w: webdav", ErrInvalid)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, nil, fmt.Errorf("%w: origin scheme", ErrInvalid)
	}
	target := strings.TrimRight(origin, "/") + "/public.php/webdav/"
	if rel != "" && rel != "/" {
		target += strings.TrimPrefix(rel, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(token, "")
	resp, err := c.httpc().Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOCMBody))
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, nil, webdav.ErrForbidden
		case http.StatusNotFound:
			return nil, nil, webdav.ErrNotFound
		default:
			return nil, nil, fmt.Errorf("ocm: webdav status %d", resp.StatusCode)
		}
	}
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	ct := resp.Header.Get("Content-Type")
	var mod time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, perr := http.ParseTime(lm); perr == nil {
			mod = t
		}
	}
	size := resp.ContentLength
	body := resp.Body
	if size < 0 {
		buf, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteFile+1))
		_ = resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}
		if int64(len(buf)) > maxRemoteFile {
			return nil, nil, fmt.Errorf("%w: file too large", ErrInvalid)
		}
		body = io.NopCloser(bytes.NewReader(buf))
		size = int64(len(buf))
	}
	return body, &webdav.Entry{
		Size:        size,
		ETag:        etag,
		ContentType: ct,
		ModTime:     mod,
	}, nil
}

type outgoingShareJSON struct {
	ShareWith    string               `json:"shareWith"`
	Name         string               `json:"name"`
	ProviderID   string               `json:"providerId"`
	Owner        string               `json:"owner"`
	Sender       string               `json:"sender"`
	ShareType    string               `json:"shareType"`
	ResourceType string               `json:"resourceType"`
	Protocol     outgoingProtocolJSON `json:"protocol"`
}

type outgoingProtocolJSON struct {
	Name    string                      `json:"name"`
	Options outgoingProtocolOptionsJSON `json:"options"`
}

type outgoingProtocolOptionsJSON struct {
	SharedSecret string `json:"sharedSecret"`
}
