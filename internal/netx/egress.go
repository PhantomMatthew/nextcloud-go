// Package netx holds network primitives shared across subsystems — today
// the ADR-0057 egress IP guard that the plugin HTTP host (internal/plugins)
// and the Mail epic's IMAP/SMTP dials (ADR-0108 §4) both install on their
// outbound connections.
package netx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedEgressIP marks an outbound dial refused because the resolved
// target IP is loopback, private, link-local, or unspecified (ADR-0057) and
// no allow prefix covered it. Dialers surface it inside *net.OpError or
// *url.Error wrappers; errors.Is reaches it through both.
var ErrBlockedEgressIP = errors.New("netx: egress to blocked IP")

// BlockedEgressIP reports whether ip is off-limits for guarded egress. The
// IPv4-mapped form is unmapped first so ::ffff:127.0.0.1 is judged as
// 127.0.0.1. CGNAT (100.64.0.0/10) and multicast are deliberately NOT
// blocked: Tailscale and carrier-grade deployments legitimately serve from
// CGNAT space, and multicast has no SSRF value over TCP.
func BlockedEgressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// GuardControl returns a net.Dialer.Control hook that refuses blocked
// target IPs (BlockedEgressIP) unless the target falls inside one of the
// allow prefixes — the mail private-host allowlist mechanism (ADR-0108 §4);
// plugins pass nil and keep their own capability-based bypass. The hook runs
// on the already-resolved IP:port before the kernel connect, so literal-IP
// targets and DNS answers are checked at the same point with no TOCTOU
// window. An unparseable address fails closed: the guard cannot vouch for
// what it cannot read.
func GuardControl(allow []netip.Prefix) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: unparseable dial address %q", ErrBlockedEgressIP, address)
		}
		ip := ap.Addr().Unmap()
		if !BlockedEgressIP(ip) {
			return nil
		}
		for _, p := range allow {
			if p.Contains(ip) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s", ErrBlockedEgressIP, ip)
	}
}

// GuardedDialContext returns a DialContext matching the standard library's
// default transport dialer (30s connect timeout, 30s keep-alive) plus the
// GuardControl hook for allow.
func GuardedDialContext(allow []netip.Prefix) func(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   GuardControl(allow),
	}).DialContext
}
