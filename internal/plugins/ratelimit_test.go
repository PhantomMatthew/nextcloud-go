package plugins

import (
	"testing"
	"time"
)

func TestTokenBucketBurstThenDeny(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(60, 3)
	for i := range 3 {
		if !b.allow() {
			t.Fatalf("call %d denied, want allow (burst)", i)
		}
	}
	if b.allow() {
		t.Fatal("call past burst allowed, want deny")
	}
}

func TestTokenBucketRefill(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(60, 1) // 1 token per second
	now := time.Now()
	b.now = func() time.Time { return now }
	b.last = now
	if !b.allow() {
		t.Fatal("first call denied, want allow")
	}
	if b.allow() {
		t.Fatal("second call allowed, want deny")
	}
	now = now.Add(1100 * time.Millisecond)
	if !b.allow() {
		t.Fatal("call after 1.1s denied, want refill to allow")
	}
}

// A long idle gap must not stockpile more than the burst capacity.
func TestTokenBucketRefillCapsAtBurst(t *testing.T) {
	t.Parallel()
	b := newTokenBucket(6000, 2) // 100 tokens per second
	now := time.Now()
	b.now = func() time.Time { return now }
	b.last = now
	if !b.allow() {
		t.Fatal("first burst call denied, want allow")
	}
	if !b.allow() {
		t.Fatal("second burst call denied, want allow")
	}
	if b.allow() {
		t.Fatal("call past burst allowed, want deny")
	}
	now = now.Add(time.Minute)
	allowed := 0
	for b.allow() {
		allowed++
	}
	if allowed != 2 {
		t.Fatalf("allowed after long idle = %d, want burst-capped 2", allowed)
	}
}

// Buckets are keyed by plugin id: one plugin's exhaustion never throttles
// another, and an unseen id gets a fresh full bucket.
func TestHTTPRatePerPluginIsolation(t *testing.T) {
	h, _ := testHost(t, HostConfig{HTTPRatePerMinute: 60, HTTPRateBurst: 1})
	if !h.allowHTTPRequest("plugin.a") {
		t.Fatal("plugin.a first call denied, want allow")
	}
	if h.allowHTTPRequest("plugin.a") {
		t.Fatal("plugin.a second call allowed, want deny")
	}
	if !h.allowHTTPRequest("plugin.b") {
		t.Fatal("plugin.b first call denied, want isolated bucket")
	}
	if got := len(h.httpRate); got != 2 {
		t.Fatalf("buckets = %d, want 2", got)
	}
}

func TestHostConfigHTTPLimitDefaults(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	if h.cfg.HTTPRatePerMinute != defaultHTTPRatePerMinute {
		t.Errorf("HTTPRatePerMinute = %d, want %d", h.cfg.HTTPRatePerMinute, defaultHTTPRatePerMinute)
	}
	if h.cfg.HTTPRateBurst != defaultHTTPBurst {
		t.Errorf("HTTPRateBurst = %d, want %d", h.cfg.HTTPRateBurst, defaultHTTPBurst)
	}
	if h.cfg.MaxHTTPResponseBytes != defaultMaxHTTPResponseBytes {
		t.Errorf("MaxHTTPResponseBytes = %d, want %d", h.cfg.MaxHTTPResponseBytes, defaultMaxHTTPResponseBytes)
	}
}
