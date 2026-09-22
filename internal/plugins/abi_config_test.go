package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func configManifest(read, write []string) *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{Config: ConfigCapabilities{Read: read, Write: write}}
	return m
}

func TestConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := appconfig.NewStore(testDB(t))
	h, buf := testHost(t, HostConfig{AppConfig: store})
	m := configManifest([]string{"tokens.*"}, []string{"tokens.*"})
	p, err := h.Load(ctx, m, wasmgen.ConfigModule("tokens.x", "secret-123", "tokens.x", "tokens.missing", "other", ErrCodePermissionDenied))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_config"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "set-ok", "get-ok", "missing-ok", "denied-ok")
	if !strings.Contains(out, "secret-123") {
		t.Fatalf("value missing from log: %q", out)
	}
	// The row is namespaced under appid "plugin" as <plugin_id>.<key>.
	val, err := store.Get(ctx, configAppID, "com.example.probe.tokens.x")
	if err != nil || val != "secret-123" {
		t.Fatalf("stored = %q, err = %v", val, err)
	}
}

func TestConfigNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	store := appconfig.NewStore(testDB(t))
	h, bufA := testHost(t, HostConfig{AppConfig: store})

	// Plugin A writes foo.
	a := configManifest([]string{"*"}, []string{"*"})
	a.Plugin.ID = "com.example.a"
	pa, err := h.Load(ctx, a, wasmgen.ConfigModule("foo", "a-value", "foo", "", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pa.Close(ctx) })
	if _, err := pa.Call(ctx, "do_config"); err != nil {
		t.Fatal(err)
	}
	requireLogMarkers(t, bufA.String(), "set-ok", "get-ok")

	// Plugin B, granted *, cannot see A's foo: the namespaced keys differ.
	h2, bufB := testHost(t, HostConfig{AppConfig: store})
	b := configManifest([]string{"*"}, nil)
	b.Plugin.ID = "com.example.b"
	pb, err := h2.Load(ctx, b, wasmgen.ConfigModule("", "", "", "foo", "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pb.Close(ctx) })
	if _, err := pb.Call(ctx, "do_config"); err != nil {
		t.Fatal(err)
	}
	requireLogMarkers(t, bufB.String(), "missing-ok")

	// Direct store inspection confirms the physical namespacing.
	val, err := store.Get(ctx, configAppID, "com.example.a.foo")
	if err != nil || val != "a-value" {
		t.Fatalf("stored = %q, err = %v", val, err)
	}
	if _, err := store.Get(ctx, configAppID, "com.example.b.foo"); err == nil {
		t.Fatal("com.example.b.foo unexpectedly present")
	}
}

func TestConfigReadDenied(t *testing.T) {
	store := appconfig.NewStore(testDB(t))
	h, buf := testHost(t, HostConfig{AppConfig: store})
	// Write granted, read not: read of any key is -3.
	installModule(t, h, configManifest(nil, []string{"*"}),
		wasmgen.ConfigGetProbeModule("foo", 4096, ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestConfigWriteDenied(t *testing.T) {
	store := appconfig.NewStore(testDB(t))
	h, buf := testHost(t, HostConfig{AppConfig: store})
	// Read granted, write not.
	installModule(t, h, configManifest([]string{"*"}, nil),
		wasmgen.ConfigSetProbeModule("foo", 3, ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestConfigNilStore(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	m := configManifest([]string{"*"}, []string{"*"})
	installModule(t, h, m, wasmgen.ConfigGetProbeModule("foo", 4096, ErrCodeUnavailable))
	installModule(t, h, m, wasmgen.ConfigSetProbeModule("foo", 3, ErrCodeUnavailable))
	if got := strings.Count(buf.String(), "probe-ok"); got != 2 {
		t.Fatalf("probe-ok count = %d, want 2: %q", got, buf.String())
	}
}

func TestConfigEmptyKey(t *testing.T) {
	store := appconfig.NewStore(testDB(t))
	h, buf := testHost(t, HostConfig{AppConfig: store})
	m := configManifest([]string{"*"}, []string{"*"})
	installModule(t, h, m, wasmgen.ConfigGetProbeModule("", 4096, ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.ConfigSetProbeModule("", 3, ErrCodeInvalidArgument))
	if got := strings.Count(buf.String(), "probe-ok"); got != 2 {
		t.Fatalf("probe-ok count = %d, want 2: %q", got, buf.String())
	}
}

func TestConfigOversizedValue(t *testing.T) {
	store := appconfig.NewStore(testDB(t))
	h, buf := testHost(t, HostConfig{AppConfig: store})
	// maxStringArg + 1 → -11; the host checks the length before reading
	// guest memory, so no real payload is needed.
	installModule(t, h, configManifest(nil, []string{"*"}),
		wasmgen.ConfigSetProbeModule("foo", maxStringArg+1, ErrCodeTooLarge))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestConfigGetSmallBuffer(t *testing.T) {
	ctx := context.Background()
	store := appconfig.NewStore(testDB(t))
	if err := store.Set(ctx, configAppID, "com.example.probe.foo", "0123456789"); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, HostConfig{AppConfig: store})
	// 10-byte value, 4-byte out buffer → -11 (writeBytes).
	installModule(t, h, configManifest([]string{"*"}, nil),
		wasmgen.ConfigGetProbeModule("foo", 4, ErrCodeTooLarge))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestCanConfigGlobs(t *testing.T) {
	c := &Capabilities{Config: ConfigCapabilities{Read: []string{"tokens.*", "plain"}, Write: []string{"tokens.*"}}}
	for _, tc := range []struct {
		key  string
		read bool
	}{
		{"tokens.x", true},
		{"tokens.", true},
		{"plain", true},
		{"tokens", false},
		{"other", false},
		{"Tokens.X", false},
	} {
		if got := c.canConfigRead(tc.key); got != tc.read {
			t.Errorf("canConfigRead(%q) = %v, want %v", tc.key, got, tc.read)
		}
	}
	if !c.canConfigWrite("tokens.x") || c.canConfigWrite("plain") {
		t.Error("canConfigWrite glob mismatch")
	}
	var nilCaps *Capabilities
	if nilCaps.canConfigRead("tokens.x") || nilCaps.canConfigWrite("tokens.x") {
		t.Error("nil capabilities must deny")
	}
}
