package plugins

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// httpManifest builds a probe manifest granted the given outbound hosts and
// private-target dialing: the allowlist tests below intentionally run
// against loopback httptest servers.
func httpManifest(grants ...string) *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{HTTP: HTTPCapabilities{Outbound: grants, OutboundAllowPrivate: true}}
	return m
}

// httpReqBytes builds the MessagePack http_request payload.
func httpReqBytes(t *testing.T, method, rawURL string, headers map[string]string, body []byte, timeoutMS int32) []byte {
	t.Helper()
	raw, err := msgpack.Marshal(map[string]any{
		"method":     method,
		"url":        rawURL,
		"headers":    headers,
		"body_bytes": body,
		"timeout_ms": timeoutMS,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// httpHostPort strips the http:// prefix from an httptest server URL.
func httpHostPort(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHTTPTarget(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"http://example.com/x", "example.com"},
		{"http://example.com:80/x", "example.com"},
		{"https://example.com", "example.com"},
		{"https://example.com:443/x", "example.com"},
		{"http://example.com:443/x", "example.com:443"},
		{"http://example.com:8080/x", "example.com:8080"},
		{"https://EXAMPLE.com:443/", "example.com"},
		{"http://user@example.com/x", "example.com"},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := httpTarget(u); got != tc.want {
			t.Errorf("httpTarget(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestHTTPOutboundNoGrant(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, probeManifest(),
		wasmgen.HTTPProbeModule(httpReqBytes(t, "GET", "http://example.com/", nil, nil, 0), ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundWrongHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, httpManifest("example.com"),
		wasmgen.HTTPProbeModule(httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0), ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "spoofed.invalid" {
			_, _ = w.Write([]byte("host-spoofed"))
			return
		}
		w.Header().Set("X-Test", "probe-value")
		_, _ = w.Write([]byte("hello-http"))
	}))
	t.Cleanup(srv.Close)
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/data", map[string]string{"Host": "spoofed.invalid", "X-Req": "yes"}, nil, 0)
	denied := httpReqBytes(t, "GET", "http://127.0.0.1:1/", nil, nil, 0)
	p, err := h.Load(ctx, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPOutboundModule(req, denied, "X-Test", 200, ErrCodePermissionDenied))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_http"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "req-ok", "status-ok", "header-ok", "body-ok", "close-ok", "denied-ok")
	if !strings.Contains(out, "hello-http") {
		t.Fatalf("body missing from log: %q", out)
	}
	if !strings.Contains(out, "probe-value") {
		t.Fatalf("header value missing from log: %q", out)
	}
	if strings.Contains(out, "host-spoofed") {
		t.Fatalf("plugin-spoofed Host header reached the server: %q", out)
	}
}

func TestHTTPOutboundPostEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "POST", srv.URL+"/echo", nil, []byte("echo-payload-123"), 0)
	p, err := h.Load(ctx, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPOutboundModule(req, nil, "X-Unused", 200, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_http"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "req-ok", "status-ok", "body-ok", "close-ok")
	if !strings.Contains(out, "echo-payload-123") {
		t.Fatalf("echoed body missing from log: %q", out)
	}
}

func TestHTTPOutboundRedirectGranted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("final-content"))
	}))
	t.Cleanup(srv.Close)
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/redir", nil, nil, 0)
	p, err := h.Load(ctx, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPOutboundModule(req, nil, "X-Unused", 200, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_http"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "req-ok", "status-ok", "body-ok", "close-ok")
	if !strings.Contains(out, "final-content") {
		t.Fatalf("redirected body missing from log: %q", out)
	}
}

func TestHTTPOutboundRedirectDenied(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/redir", nil, nil, 0)
	installModule(t, h, httpManifest(httpHostPort(srv)), wasmgen.HTTPProbeModule(req, ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundInvalidRequests(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	m := httpManifest("example.com")
	installModule(t, h, m, wasmgen.HTTPProbeModule(
		httpReqBytes(t, "GET", "file:///etc/passwd", nil, nil, 0), ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.HTTPProbeModule(
		httpReqBytes(t, "GET", "ftp://example.com/x", nil, nil, 0), ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.HTTPProbeModule(
		httpReqBytes(t, "WAT", "http://example.com/", nil, nil, 0), ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.HTTPProbeModule([]byte{0xff, 0xfe, 0xfd}, ErrCodeInvalidArgument))
	if got := strings.Count(buf.String(), "probe-ok"); got != 4 {
		t.Fatalf("probe-ok count = %d, want 4: %q", got, buf.String())
	}
}

func TestHTTPOutboundTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte("too-late"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/slow", nil, nil, 50)
	installModule(t, h, httpManifest(httpHostPort(srv)), wasmgen.HTTPProbeModule(req, ErrCodeTimeout))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundHandleBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	// 16 responses fit the budget; the 17th open must fail with -12.
	installModule(t, h, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPOpenLoopModule(req, 17, ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "budget-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundStaleHandle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	installModule(t, h, httpManifest(httpHostPort(srv)), wasmgen.HTTPStaleModule(req))
	if !strings.Contains(buf.String(), "stale-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestHTTPOutboundAbsentHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	installModule(t, h, httpManifest(httpHostPort(srv)), wasmgen.HTTPAbsentHeaderModule(req, "X-Missing"))
	if !strings.Contains(buf.String(), "absent-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

// countingBody tallies Close calls so tests can observe leaked-response
// cleanup.
type countingBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b countingBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

type countingTransport struct {
	base   http.RoundTripper
	closed *atomic.Int32
}

func (rt countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = countingBody{ReadCloser: resp.Body, closed: rt.closed}
	return resp, nil
}

func TestHTTPOutboundLeakClosesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("leaked"))
	}))
	t.Cleanup(srv.Close)
	var closed atomic.Int32
	client := &http.Client{Transport: countingTransport{base: http.DefaultTransport, closed: &closed}}
	h, buf := testHost(t, HostConfig{HTTPClient: client})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	// per_request instances release on every call; closeAll must close the
	// leaked response body.
	installModule(t, h, httpManifest(httpHostPort(srv)), wasmgen.HTTPLeakModule(req))
	if !strings.Contains(buf.String(), "leak-ok") {
		t.Fatalf("log %q", buf.String())
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("body closes = %d, want 1", got)
	}
}

// rewriteTransport points every request at the test server regardless of
// the URL host, so default-port allowlist rules can be exercised without
// binding port 80.
type rewriteTransport struct {
	target string // host:port of the test server
}

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = "http"
	r2.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(r2)
}

func TestHTTPOutboundHostOnlyGrantDefaultPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("default-port-ok"))
	}))
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: rewriteTransport{target: httpHostPort(srv)}}
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{HTTPClient: client})
	// A host-only grant must match the URL's default port (explicit :80).
	req := httpReqBytes(t, "GET", "http://example.com:80/data", nil, nil, 0)
	p, err := h.Load(ctx, httpManifest("example.com"),
		wasmgen.HTTPOutboundModule(req, nil, "X-Unused", 200, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_http"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "req-ok", "status-ok", "body-ok", "close-ok")
	if !strings.Contains(out, "default-port-ok") {
		t.Fatalf("body missing from log: %q", out)
	}
}
