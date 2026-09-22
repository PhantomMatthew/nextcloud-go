package plugins

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// errEgressPrivateIP marks a plugin outbound dial refused because the
// resolved target IP is loopback, private, link-local, or unspecified
// (ADR-0057). client.Do wraps it in a *url.Error.
var errEgressPrivateIP = errors.New("plugins: http egress to blocked IP")

// blockedEgressIP reports whether ip is off-limits for plugin HTTP egress
// unless the plugin holds http.outbound_allow_private. The IPv4-mapped form
// is unmapped first so ::ffff:127.0.0.1 is judged as 127.0.0.1. CGNAT
// (100.64.0.0/10) and multicast are deliberately NOT blocked: Tailscale and
// carrier-grade deployments legitimately serve APIs from CGNAT space, and
// multicast has no SSRF value over TCP.
func blockedEgressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// egressIPGuard is a net.Dialer.Control hook invoked with the already
// resolved IP:port before the kernel connect, so literal-IP URLs and DNS
// answers are checked at the same point with no TOCTOU window. An
// unparseable address fails closed: the guard cannot vouch for what it
// cannot read.
func egressIPGuard(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: unparseable dial address %q", errEgressPrivateIP, address)
	}
	if ip := ap.Addr().Unmap(); blockedEgressIP(ip) {
		return fmt.Errorf("%w: %s", errEgressPrivateIP, ip)
	}
	return nil
}

// egressGuardedDialContext returns a DialContext matching the standard
// library's default transport dialer (30s connect timeout, 30s keep-alive)
// plus the egressIPGuard control hook.
func egressGuardedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   egressIPGuard,
	}).DialContext
}
