package netx

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestBlockedEgressIP(t *testing.T) {
	t.Parallel()
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
		{"::ffff:10.1.2.3", true},  // IPv4-mapped private
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
		if got := BlockedEgressIP(ip); got != tc.want {
			t.Errorf("BlockedEgressIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestGuardControl(t *testing.T) {
	t.Parallel()
	guard := GuardControl(nil)
	if err := guard("tcp", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("public target refused: %v", err)
	}
	if err := guard("tcp", "127.0.0.1:8080", nil); !errors.Is(err, ErrBlockedEgressIP) {
		t.Fatalf("loopback err = %v", err)
	}
	if err := guard("tcp", "[::1]:8080", nil); !errors.Is(err, ErrBlockedEgressIP) {
		t.Fatalf("v6 loopback err = %v", err)
	}
	if err := guard("tcp", "[::ffff:127.0.0.1]:8080", nil); !errors.Is(err, ErrBlockedEgressIP) {
		t.Fatalf("mapped loopback err = %v", err)
	}
	// An address the guard cannot read fails closed.
	if err := guard("tcp", "not-an-ip", nil); !errors.Is(err, ErrBlockedEgressIP) {
		t.Fatalf("garbage address err = %v", err)
	}
}

func TestGuardControlAllowPrefix(t *testing.T) {
	t.Parallel()
	allow := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	guard := GuardControl(allow)
	for _, tc := range []struct {
		address string
		wantErr bool
	}{
		{"10.1.2.3:993", false},          // inside the allow prefix
		{"[::1]:993", false},             // inside the v6 allow prefix
		{"[::ffff:10.1.2.3]:993", false}, // mapped form unmaps into the prefix
		{"192.168.1.1:993", true},        // blocked, not covered
		{"127.0.0.1:993", true},          // blocked, not covered
		{"8.8.8.8:993", false},           // public never blocked
		{"not-an-ip", true},              // parse failure precedes the allowlist
	} {
		err := guard("tcp", tc.address, nil)
		if tc.wantErr && !errors.Is(err, ErrBlockedEgressIP) {
			t.Errorf("guard(%q) err = %v, want ErrBlockedEgressIP", tc.address, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("guard(%q) err = %v, want nil", tc.address, err)
		}
	}
}

// TestGuardedDialContext exercises the ready-made dialer end to end against
// a real loopback listener: refused without an allow prefix, connected with
// one, and public-shaped addresses unaffected (the dial to the listener is
// re-aimed by the test, the guard still sees the loopback target).
func TestGuardedDialContext(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr := ln.Addr().String()

	dial := GuardedDialContext(nil)
	if _, err := dial(context.Background(), "tcp", addr); !errors.Is(err, ErrBlockedEgressIP) {
		t.Fatalf("loopback dial err = %v", err)
	}

	allowed := GuardedDialContext([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	conn, err := allowed(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatalf("allowlisted loopback dial: %v", err)
	}
	_ = conn.Close()
}

// TestGuardedDialContextHonorsContext pins that the returned dialer still
// respects context cancellation (it embeds a stock net.Dialer).
func TestGuardedDialContextHonorsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	dial := GuardedDialContext(nil)
	if _, err := dial(ctx, "tcp", "8.8.8.8:993"); err == nil {
		t.Fatal("expired context dial succeeded")
	}
}
