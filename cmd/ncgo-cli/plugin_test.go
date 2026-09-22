package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

// cliPluginEnv writes a config file with a localfs storage backend and a
// fixed instance id (so the system-storage prefix is deterministic),
// returning the config and storage root paths.
func cliPluginEnv(t *testing.T) (cfgPath, storageRoot string) {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "cli.db")
	storageRoot = filepath.Join(dir, "storage")
	cfgPath = filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
database:
  driver: sqlite
  dsn: %q
instance:
  id: testinst
storage:
  default_backend: local
  backends:
    local:
      type: localfs
      root: %q
`, dsn, storageRoot)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storageRoot
}

// TestInstallHostConfig asserts the CLI install host wires every subsystem
// lifecycle hooks need (G1). The wasm-driven install/upgrade flow itself
// cannot be tested at this layer: cmd cannot import
// internal/plugins/internal/wasmgen (Go's internal visibility rules allow
// it only inside the internal/plugins subtree), so no guest module is
// available here — that coverage lives in internal/plugins instead.
func TestInstallHostConfig(t *testing.T) {
	cfgPath, _ := cliPluginEnv(t)
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := openDB(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	st, err := openStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}

	hc := installHostConfig(cfg, db, st)
	if hc.DB == nil || hc.Bus == nil || hc.Registry == nil || hc.Files == nil ||
		hc.SystemStorage == nil || hc.AppConfig == nil || hc.Jobs == nil {
		t.Fatalf("install host missing subsystems: %+v", hc)
	}
	// Cache is deliberately nil (the CLI is a management surface; cache is
	// runtime state) and metrics stay off in the CLI.
	if hc.Cache != nil || hc.Metrics != nil {
		t.Fatalf("cache/metrics must stay nil: %+v", hc)
	}
	if hc.SystemPrefix != "appdata_testinst/plugins" {
		t.Fatalf("SystemPrefix = %q", hc.SystemPrefix)
	}

	h, err := newInstallHost(ctx, cfg, db, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestCLIInstanceID mirrors app.New's fallback: a configured instance.id is
// used verbatim; an unset one yields an ephemeral "oc"+hex id.
func TestCLIInstanceID(t *testing.T) {
	cfg := &config.Config{}
	cfg.Instance.ID = "ocfixed"
	if id := cliInstanceID(cfg); id != "ocfixed" {
		t.Fatalf("configured id = %q", id)
	}
	cfg.Instance.ID = ""
	id := cliInstanceID(cfg)
	if !strings.HasPrefix(id, "oc") || len(id) != 12 {
		t.Fatalf("ephemeral id = %q", id)
	}
}
