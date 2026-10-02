package plugins

import (
	"context"
	"net"
	"net/netip"

	"github.com/PhantomMatthew/nextcloud-go/internal/netx"
)

// The ADR-0057 egress guard primitives live in internal/netx (shared with
// the Mail epic's IMAP/SMTP dials, ADR-0108 §4); this file keeps the plugin
// aliases. Plugin behavior is unchanged: plugins bypass the guard through
// the http.outbound_allow_private capability selecting the unguarded client,
// never through an allowlist, so every hook here is built with nil allow.

// errEgressPrivateIP marks a plugin outbound dial refused because the
// resolved target IP is loopback, private, link-local, or unspecified
// (ADR-0057). client.Do wraps it in a *url.Error.
var errEgressPrivateIP = netx.ErrBlockedEgressIP

// blockedEgressIP reports whether ip is off-limits for plugin HTTP egress
// unless the plugin holds http.outbound_allow_private. CGNAT
// (100.64.0.0/10) and multicast are deliberately NOT blocked: Tailscale and
// carrier-grade deployments legitimately serve APIs from CGNAT space, and
// multicast has no SSRF value over TCP.
func blockedEgressIP(ip netip.Addr) bool {
	return netx.BlockedEgressIP(ip)
}

// egressIPGuard is a net.Dialer.Control hook invoked with the already
// resolved IP:port before the kernel connect, so literal-IP URLs and DNS
// answers are checked at the same point with no TOCTOU window. An
// unparseable address fails closed: the guard cannot vouch for what it
// cannot read.
var egressIPGuard = netx.GuardControl(nil)

// egressGuardedDialContext returns a DialContext matching the standard
// library's default transport dialer (30s connect timeout, 30s keep-alive)
// plus the egressIPGuard control hook.
func egressGuardedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return netx.GuardedDialContext(nil)
}
