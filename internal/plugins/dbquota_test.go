package plugins

import "testing"

func TestDBSlotQuotaBoundary(t *testing.T) {
	t.Parallel()
	h, _ := testHost(t, HostConfig{DBMaxConcurrentPerPlugin: 2})
	r1, ok := h.acquireDBSlot("p1")
	if !ok {
		t.Fatal("first acquire denied, want allow")
	}
	r2, ok := h.acquireDBSlot("p1")
	if !ok {
		t.Fatal("second acquire denied, want allow")
	}
	if _, ok := h.acquireDBSlot("p1"); ok {
		t.Fatal("third acquire allowed, want deny at quota")
	}
	r1()
	r3, ok := h.acquireDBSlot("p1")
	if !ok {
		t.Fatal("acquire after release denied, want allow")
	}
	r2()
	r3()
	if got := len(h.dbConc); got != 0 {
		t.Fatalf("dbConc entries = %d, want 0 (zero count deletes the key)", got)
	}
}

// Quotas are keyed by plugin id: one plugin's exhaustion never blocks
// another.
func TestDBSlotPerPluginIsolation(t *testing.T) {
	t.Parallel()
	h, _ := testHost(t, HostConfig{DBMaxConcurrentPerPlugin: 1})
	r1, ok := h.acquireDBSlot("plugin.a")
	if !ok {
		t.Fatal("plugin.a first acquire denied, want allow")
	}
	defer r1()
	if _, ok := h.acquireDBSlot("plugin.a"); ok {
		t.Fatal("plugin.a second acquire allowed, want deny")
	}
	r2, ok := h.acquireDBSlot("plugin.b")
	if !ok {
		t.Fatal("plugin.b acquire denied, want isolated quota")
	}
	r2()
	if got := len(h.dbConc); got != 1 {
		t.Fatalf("dbConc entries = %d, want 1 (only plugin.a held)", got)
	}
}

func TestHostConfigDBQuotaDefault(t *testing.T) {
	t.Parallel()
	h, _ := testHost(t, HostConfig{})
	if h.cfg.DBMaxConcurrentPerPlugin != defaultDBMaxConcurrentPerPlugin {
		t.Errorf("DBMaxConcurrentPerPlugin = %d, want %d", h.cfg.DBMaxConcurrentPerPlugin, defaultDBMaxConcurrentPerPlugin)
	}
}
