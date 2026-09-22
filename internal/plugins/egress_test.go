package plugins

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func TestBlockedEgressIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.15.0.1", false}, // just outside RFC1918 172.16.0.0/12
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata endpoint
		{"fe80::1", true},
		{"0.0.0.0", true},
		{"::", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback is unmapped first
		{"::ffff:8.8.8.8", false},  // mapped public stays public
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"100.64.0.1", false},      // CGNAT is deliberately not blocked
		{"2606:4700::1111", false}, // public v6 (Cloudflare DNS)
		{"224.0.0.1", false},       // multicast deliberately not blocked
		{"fc00::1", true},          // ULA
		{"2001:db8::1", false},     // documentation range is not private per netip
	} {
		ip, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", tc.ip, err)
		}
		if got := blockedEgressIP(ip); got != tc.want {
			t.Errorf("blockedEgressIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestEgressIPGuard(t *testing.T) {
	if err := egressIPGuard("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("public target refused: %v", err)
	}
	if err := egressIPGuard("tcp", "127.0.0.1:8080", nil); !errors.Is(err, errEgressPrivateIP) {
		t.Fatalf("loopback err = %v", err)
	}
	if err := egressIPGuard("tcp", "[::ffff:127.0.0.1]:8080", nil); !errors.Is(err, errEgressPrivateIP) {
		t.Fatalf("mapped loopback err = %v", err)
	}
	// An address the guard cannot read fails closed.
	if err := egressIPGuard("tcp", "not-an-ip", nil); !errors.Is(err, errEgressPrivateIP) {
		t.Fatalf("garbage address err = %v", err)
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGuardedHTTPClientFallbacks(t *testing.T) {
	ctx := context.Background()
	// A custom RoundTripper cannot take the guard; the client comes back
	// unchanged (operator owns egress policy).
	custom := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})}
	if got := guardedHTTPClient(ctx, custom, nil); got != custom {
		t.Fatal("custom RoundTripper client must be returned unchanged")
	}
	// An operator-set dial hook also disables the guard.
	dialed := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{}).DialContext}}
	if got := guardedHTTPClient(ctx, dialed, nil); got != dialed {
		t.Fatal("custom DialContext client must be returned unchanged")
	}
	// The default shape gets a guarded clone with the hook installed.
	def := &http.Client{}
	got := guardedHTTPClient(ctx, def, nil)
	if got == def {
		t.Fatal("default client must be cloned, not returned as-is")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatalf("guard not installed on default client clone: %T", got.Transport)
	}
	// A plain *http.Transport without a dial hook is guarded on the clone,
	// leaving the operator's transport untouched.
	plain := &http.Client{Transport: &http.Transport{}}
	got = guardedHTTPClient(ctx, plain, nil)
	tr, ok = got.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("guard not installed on cloned transport")
	}
	orig, ok := plain.Transport.(*http.Transport)
	if !ok || orig.DialContext != nil {
		t.Fatal("operator transport was mutated")
	}
}

func TestHTTPClientFor(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	var nilCaps *Capabilities
	if got := h.httpClientFor(nilCaps); got != h.httpGuarded {
		t.Fatal("nil capabilities must get the guarded client")
	}
	if got := h.httpClientFor(&Capabilities{}); got != h.httpGuarded {
		t.Fatal("ungranted plugin must get the guarded client")
	}
	open := &Capabilities{HTTP: HTTPCapabilities{OutboundAllowPrivate: true}}
	if got := h.httpClientFor(open); got != h.cfg.HTTPClient {
		t.Fatal("http.outbound_allow_private plugin must get the configured client")
	}
}

// TestGuardedClientBlocksLoopbackDial exercises the guard through the real
// dial path (no wasm): the resolved 127.0.0.1 of an httptest server is
// refused in Control, and the sentinel survives the *url.Error wrapping.
func TestGuardedClientBlocksLoopbackDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never"))
	}))
	t.Cleanup(srv.Close)
	h, _ := testHost(t, HostConfig{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.httpGuarded.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the egress guard to refuse the loopback dial")
	}
	if !errors.Is(err, errEgressPrivateIP) {
		t.Fatalf("err = %v", err)
	}
}

// egressManifest grants outbound to hostport, optionally with private-target
// dialing.
func egressManifest(hostport string, allowPrivate bool) *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{HTTP: HTTPCapabilities{
		Outbound:             []string{hostport},
		OutboundAllowPrivate: allowPrivate,
	}}
	return m
}

// TestEgressGuardDeniesLiteralIP: the hostname allowlist passes the literal
// 127.0.0.1 grant, but the dial-time guard refuses it without
// http.outbound_allow_private.
func TestEgressGuardDeniesLiteralIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	installModule(t, h, egressManifest(httpHostPort(srv), false),
		wasmgen.HTTPProbeModule(req, ErrCodePermissionDenied))
	out := buf.String()
	if !strings.Contains(out, "probe-ok") {
		t.Fatalf("log %q", out)
	}
	if !strings.Contains(out, "private-IP egress guard") || !strings.Contains(out, "com.example.probe") {
		t.Fatalf("security warning with plugin id missing from log: %q", out)
	}
}

// TestEgressGuardAllowPrivate: the same literal-IP grant succeeds once
// http.outbound_allow_private is granted.
func TestEgressGuardAllowPrivate(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("private-ok"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	req := httpReqBytes(t, "GET", srv.URL+"/", nil, nil, 0)
	installModule(t, h, egressManifest(httpHostPort(srv), true),
		wasmgen.HTTPProbeModule(req, ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server hits = %d, want 1", got)
	}
}

// TestEgressGuardDeniesLocalhostDNS: a hostname grant exercises the DNS
// path — localhost resolves to ::1/127.0.0.1 and every candidate is refused
// at dial time without the private grant.
func TestEgressGuardDeniesLocalhostDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("never"))
	}))
	t.Cleanup(srv.Close)
	h, buf := testHost(t, HostConfig{})
	localURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	req := httpReqBytes(t, "GET", localURL+"/", nil, nil, 0)
	installModule(t, h, egressManifest("localhost:"+srvPort(srv), false),
		wasmgen.HTTPProbeModule(req, ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

// srvPort extracts the port from an httptest server URL.
func srvPort(srv *httptest.Server) string {
	u := srv.URL
	return u[strings.LastIndex(u, ":")+1:]
}
