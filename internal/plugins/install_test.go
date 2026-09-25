package plugins

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

func installManifest(version, caps string) []byte {
	return []byte(`
[plugin]
id = "com.example.inst"
name = "Inst"
version = "` + version + `"
abi = "ncgo-abi/1"
` + caps + `
[entry_points]
module = "hello.wasm"
on_install = "ncgo_on_install"
`)
}

// upgradeManifest is installManifest with an on_upgrade entry point.
func upgradeManifest(version, caps string) []byte {
	return []byte(`
[plugin]
id = "com.example.inst"
name = "Inst"
version = "` + version + `"
abi = "ncgo-abi/1"
` + caps + `
[entry_points]
module = "hello.wasm"
on_install = "ncgo_on_install"
on_upgrade = "ncgo_on_upgrade"
`)
}

func testInstaller(t *testing.T, trusted []ed25519.PublicKey) *Installer {
	t.Helper()
	h, err := NewHost(context.Background(), HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return &Installer{
		Host:        h,
		Registry:    NewRegistry(testDB(t)),
		InstallDir:  t.TempDir(),
		TrustedKeys: trusted,
		Logger:      slog.New(slog.DiscardHandler),
	}
}

func buildArchive(t *testing.T, manifest []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	return buildArchiveModule(t, manifest, wasmgen.HelloModule("installed"), priv)
}

func buildArchiveModule(t *testing.T, manifest, module []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	members := map[string][]byte{
		"plugin.toml": manifest,
		"hello.wasm":  module,
	}
	if priv != nil {
		sig, err := SignMembers(members, priv)
		if err != nil {
			t.Fatal(err)
		}
		members[SignatureFile] = sig
	}
	raw, err := WriteArchive(members)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInstallSigned(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	row, err := in.Install(context.Background(), raw, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "com.example.inst" || row.Version != "1.0.0" || !row.Enabled {
		t.Fatalf("row = %+v", row)
	}
	if row.SignatureKeyID != KeyIDFromPublic(pub) {
		t.Fatalf("keyid = %q", row.SignatureKeyID)
	}
	if _, err := os.Stat(filepath.Join(in.InstallDir, "com.example.inst", "1.0.0.ncplugin")); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
}

func TestInstallUnsignedPolicy(t *testing.T) {
	in := testInstaller(t, nil)
	raw := buildArchive(t, installManifest("1.0.0", ""), nil)
	if _, err := in.Install(context.Background(), raw, InstallOptions{}); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v", err)
	}
	row, err := in.Install(context.Background(), raw, InstallOptions{ForceUnsigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.SignatureKeyID != "" {
		t.Fatalf("keyid = %q", row.SignatureKeyID)
	}
}

func TestInstallUntrustedSignature(t *testing.T) {
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, nil) // no trusted keys
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(context.Background(), raw, InstallOptions{}); !errors.Is(err, ErrSignatureUntrusted) {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallUpgradeCapsChange(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()

	v1 := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(ctx, v1, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	// Same capabilities: upgrade allowed without re-approval.
	v1Same := buildArchive(t, installManifest("1.0.1", ""), priv)
	if _, err := in.Install(ctx, v1Same, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	// Changed capabilities: requires ApproveCaps.
	caps := `
[capabilities]
events.publish = ["inst.done"]
`
	v2 := buildArchive(t, installManifest("2.0.0", caps), priv)
	if _, err := in.Install(ctx, v2, InstallOptions{}); !errors.Is(err, ErrCapsChanged) {
		t.Fatalf("err = %v", err)
	}
	row, err := in.Install(ctx, v2, InstallOptions{ApproveCaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != "2.0.0" {
		t.Fatalf("row = %+v", row)
	}
}

func TestUninstall(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Registry.Get(ctx, "com.example.inst"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(in.InstallDir, "com.example.inst")); !os.IsNotExist(err) {
		t.Fatalf("install dir still present: %v", err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestUninstallDeletesRoutes(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, err := NewHost(ctx, HostConfig{Registry: reg}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	in := &Installer{
		Host:        h,
		Registry:    reg,
		InstallDir:  t.TempDir(),
		TrustedKeys: []ed25519.PublicKey{pub},
		Logger:      slog.New(slog.DiscardHandler),
	}
	caps := `
[capabilities]
routes.register = ["/apps/com.example.inst/*"]
`
	members := map[string][]byte{
		"plugin.toml": installManifest("1.0.0", caps),
		"hello.wasm": wasmgen.RouteRegModule(false, true,
			[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.inst/api", Handler: "handleApi"}}, ErrCodeOK),
	}
	sig, err := SignMembers(members, priv)
	if err != nil {
		t.Fatal(err)
	}
	members[SignatureFile] = sig
	raw, err := WriteArchive(members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.RoutesForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); err != nil {
		t.Fatal(err)
	}
	recs, err = reg.RoutesForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 0 {
		t.Fatalf("routes survive uninstall: %+v %v", recs, err)
	}
}

// TestInstallUpgradeHook proves the G2 upgrade path: installing a new
// version over an existing one clears the old route/prop registrations and
// invokes on_upgrade with the from-version string instead of re-running
// on_install.
func TestInstallUpgradeHook(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	var buf bytes.Buffer
	h, err := NewHost(ctx, HostConfig{Registry: reg}, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	in := &Installer{
		Host:        h,
		Registry:    reg,
		InstallDir:  t.TempDir(),
		TrustedKeys: []ed25519.PublicKey{pub},
		Logger:      slog.New(slog.DiscardHandler),
	}
	caps := `
[capabilities]
routes.register = ["/apps/com.example.inst/*"]
webdav.props = ["x:a"]
`
	module := wasmgen.UpgradeModule("/apps/com.example.inst/a", "/apps/com.example.inst/b", "x:a")

	v1 := buildArchiveModule(t, upgradeManifest("1.0.0", caps), module, priv)
	if _, err := in.Install(ctx, v1, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.RoutesForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 1 || recs[0].Path != "/apps/com.example.inst/a" {
		t.Fatalf("v1 routes = %+v %v", recs, err)
	}
	props, err := reg.PropsForPlugin(ctx, "com.example.inst")
	if err != nil || len(props) != 1 || props[0].Name != "x:a" {
		t.Fatalf("v1 props = %+v %v", props, err)
	}

	v2 := buildArchiveModule(t, upgradeManifest("2.0.0", caps), module, priv)
	row, err := in.Install(ctx, v2, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != "2.0.0" {
		t.Fatalf("row = %+v", row)
	}
	// on_upgrade registered route B; route A was cleared, not re-registered.
	recs, err = reg.RoutesForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 1 || recs[0].Path != "/apps/com.example.inst/b" {
		t.Fatalf("v2 routes = %+v %v", recs, err)
	}
	// The old prop registration was cleared and on_upgrade did not renew it.
	props, err = reg.PropsForPlugin(ctx, "com.example.inst")
	if err != nil || len(props) != 0 {
		t.Fatalf("v2 props = %+v %v", props, err)
	}
	// The guest received the previous version string.
	if !strings.Contains(buf.String(), "upgrade 1.0.0") {
		t.Fatalf("from-version not delivered, log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "install-ok") {
		t.Fatalf("v1 on_install did not run, log:\n%s", buf.String())
	}
}

// TestInstallUpgradeFallback proves a plugin without an on_upgrade entry
// point falls back to on_install on upgrade, so its registrations survive.
func TestInstallUpgradeFallback(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, err := NewHost(ctx, HostConfig{Registry: reg}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	in := &Installer{
		Host:        h,
		Registry:    reg,
		InstallDir:  t.TempDir(),
		TrustedKeys: []ed25519.PublicKey{pub},
		Logger:      slog.New(slog.DiscardHandler),
	}
	caps := `
[capabilities]
routes.register = ["/apps/com.example.inst/*"]
`
	module := wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.inst/api", Handler: "handleApi"}}, ErrCodeOK)
	for _, version := range []string{"1.0.0", "2.0.0"} {
		raw := buildArchiveModule(t, installManifest(version, caps), module, priv)
		if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}
	row, err := reg.Get(ctx, "com.example.inst")
	if err != nil || row.Version != "2.0.0" {
		t.Fatalf("row = %+v %v", row, err)
	}
	// Cleared by the upgrade, then re-registered by the on_install fallback.
	recs, err := reg.RoutesForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 1 || recs[0].Path != "/apps/com.example.inst/api" {
		t.Fatalf("routes after upgrade = %+v %v", recs, err)
	}
}

// TestUninstallCleanup proves G3: uninstall removes the plugin's queued job
// rows, its appconfig keys, its "plugin:<id>:" cache keys, and its
// system-storage tree — not just the registry row, routes, and install dir.
func TestUninstallCleanup(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db := testDB(t)
	reg := NewRegistry(db)
	jobStore := jobs.NewSQLStore(db)
	acfg := appconfig.NewStore(db)
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mc, err := cache.NewMemory(cache.MemoryConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mc.Close)
	const prefix = "appdata_octest/plugins"
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	in := &Installer{
		Host:          h,
		Registry:      reg,
		InstallDir:    t.TempDir(),
		TrustedKeys:   []ed25519.PublicKey{pub},
		Logger:        slog.New(slog.DiscardHandler),
		JobStore:      jobStore,
		AppConfig:     acfg,
		SystemStorage: st,
		SystemPrefix:  prefix,
		Cache:         mc,
	}
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	// Fixtures: one queued plugin job row plus a control row, plugin config
	// keys plus prefix-boundary and appid controls, and a nested
	// system-storage tree.
	now := time.Now().UTC().UnixMilli()
	if err := jobStore.Insert(ctx, &jobs.Row{Name: "plugin.com.example.inst", RunAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := jobStore.Insert(ctx, &jobs.Row{Name: "shares.expire", RunAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := acfg.Set(ctx, "plugin", "com.example.inst.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if err := acfg.Set(ctx, "plugin", "com.example.inst2.other", "keep"); err != nil {
		t.Fatal(err)
	}
	if err := acfg.Set(ctx, "core", "com.example.inst.fake", "keep"); err != nil {
		t.Fatal(err)
	}
	// Cache fixtures: keys under the plugin's namespace plus a
	// prefix-boundary control ("com.example.inst2" must not match) and a
	// counter entry. Literals pin the cache-key namespace on disk.
	if err := mc.Set(ctx, "plugin:com.example.inst:foo", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := mc.Set(ctx, "plugin:com.example.inst2:bar", []byte("keep"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Increment(ctx, "plugin:com.example.inst:hits", 3); err != nil {
		t.Fatal(err)
	}
	writeFile := func(p, content string) {
		t.Helper()
		w, err := st.Create(ctx, p, int64(len(content)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(prefix+"/com.example.inst/top.txt", "top")
	writeFile(prefix+"/com.example.inst/sub/deep/data.bin", "deep")

	if err := in.Uninstall(ctx, "com.example.inst"); err != nil {
		t.Fatal(err)
	}

	count := func(query string, args ...any) int {
		t.Helper()
		row := db.QueryRow(ctx, query, args...)
		var n int
		if err := row.Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM jobs WHERE name = ?`, "plugin.com.example.inst"); n != 0 {
		t.Fatalf("plugin job rows survive uninstall: %d", n)
	}
	if n := count(`SELECT count(*) FROM jobs WHERE name = ?`, "shares.expire"); n != 1 {
		t.Fatalf("control job row lost: %d", n)
	}
	if _, err := acfg.Get(ctx, "plugin", "com.example.inst.theme"); !errors.Is(err, appconfig.ErrNotFound) {
		t.Fatalf("plugin config survives uninstall: %v", err)
	}
	if val, err := acfg.Get(ctx, "plugin", "com.example.inst2.other"); err != nil || val != "keep" {
		t.Fatalf("prefix-boundary config lost: %q %v", val, err)
	}
	if val, err := acfg.Get(ctx, "core", "com.example.inst.fake"); err != nil || val != "keep" {
		t.Fatalf("other appid config lost: %q %v", val, err)
	}
	if _, err := st.Stat(ctx, prefix+"/com.example.inst"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("system storage tree survives uninstall: %v", err)
	}
	if _, err := mc.Get(ctx, "plugin:com.example.inst:foo"); !errors.Is(err, cache.ErrMiss) {
		t.Fatalf("plugin cache key survives uninstall: %v", err)
	}
	if v, err := mc.Increment(ctx, "plugin:com.example.inst:hits", 0); err != nil || v != 0 {
		t.Fatalf("plugin cache counter survives uninstall: %d %v", v, err)
	}
	if val, err := mc.Get(ctx, "plugin:com.example.inst2:bar"); err != nil || string(val) != "keep" {
		t.Fatalf("prefix-boundary cache key lost: %q %v", val, err)
	}
	if _, err := reg.Get(ctx, "com.example.inst"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("registry row survives uninstall: %v", err)
	}
}

func TestLoadTrustedKeys(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(pub)
	if err := os.WriteFile(filepath.Join(dir, "ops.pub"), []byte(enc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadTrustedKeys(dir)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v %v", keys, err)
	}
	if KeyIDFromPublic(keys[0]) != KeyIDFromPublic(pub) {
		t.Fatal("key mismatch")
	}
	// Missing directory yields no keys.
	keys, err = LoadTrustedKeys(filepath.Join(dir, "nope"))
	if err != nil || keys != nil {
		t.Fatalf("keys = %v %v", keys, err)
	}
	// Malformed key file is rejected.
	if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("!!!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustedKeys(dir); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}

// archiveDirNames returns the sorted entry names of one plugin install dir.
func archiveDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// TestInstallUpgradeArchiveGC proves ADR-0089: each successful upgrade
// keeps exactly the current and previous archives in <InstallDir>/<id>/,
// collects anything strictly older, and the registry row points at the new
// archive file. A same-version reinstall is a GC no-op — the previous
// distinct version's archive is the last rollback artifact and must
// survive — and the next real upgrade converges the directory again.
func TestInstallUpgradeArchiveGC(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()
	dir := filepath.Join(in.InstallDir, "com.example.inst")

	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0"} {
		raw := buildArchive(t, installManifest(version, ""), priv)
		if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}
	if names := archiveDirNames(t, dir); !slices.Equal(names, []string{"2.0.0.ncplugin", "3.0.0.ncplugin"}) {
		t.Fatalf("dir after v1->v2->v3 = %v", names)
	}
	row, err := in.Registry.Get(ctx, "com.example.inst")
	if err != nil {
		t.Fatal(err)
	}
	if row.ArchivePath != filepath.Join(dir, "3.0.0.ncplugin") {
		t.Fatalf("archive path = %q", row.ArchivePath)
	}

	// Same-version reinstall: GC is a no-op, both archives survive.
	raw := buildArchive(t, installManifest("3.0.0", ""), priv)
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if names := archiveDirNames(t, dir); !slices.Equal(names, []string{"2.0.0.ncplugin", "3.0.0.ncplugin"}) {
		t.Fatalf("dir after same-version reinstall = %v", names)
	}

	// The next real upgrade converges the directory to current+previous.
	v4 := buildArchive(t, installManifest("4.0.0", ""), priv)
	if _, err := in.Install(ctx, v4, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if names := archiveDirNames(t, dir); !slices.Equal(names, []string{"3.0.0.ncplugin", "4.0.0.ncplugin"}) {
		t.Fatalf("dir after v4 upgrade = %v", names)
	}
}

// TestInstallFreshInstallNoGC pins the GC scope: fresh installs never sweep,
// and one plugin's install never touches another plugin's directory.
func TestInstallFreshInstallNoGC(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()

	for _, version := range []string{"1.0.0", "2.0.0"} {
		raw := buildArchive(t, installManifest(version, ""), priv)
		if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
	}
	dirA := filepath.Join(in.InstallDir, "com.example.inst")
	before := archiveDirNames(t, dirA)
	if !slices.Equal(before, []string{"1.0.0.ncplugin", "2.0.0.ncplugin"}) {
		t.Fatalf("plugin A dir = %v", before)
	}

	manifestB := []byte(strings.Replace(string(installManifest("1.0.0", "")),
		"com.example.inst", "com.example.other", 1))
	rawB := buildArchive(t, manifestB, priv)
	if _, err := in.Install(ctx, rawB, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	if names := archiveDirNames(t, dirA); !slices.Equal(names, before) {
		t.Fatalf("plugin A dir changed by B's fresh install: %v -> %v", before, names)
	}
	dirB := filepath.Join(in.InstallDir, "com.example.other")
	if names := archiveDirNames(t, dirB); !slices.Equal(names, []string{"1.0.0.ncplugin"}) {
		t.Fatalf("plugin B dir = %v", names)
	}
}

// TestGCOldArchives sweeps a synthetic directory directly: strictly-older
// archives go, the keep-set stays, and non-archives and subdirectories are
// never touched — including a subdirectory with an archive-looking name,
// which must survive via the IsDir skip. A missing directory is a
// Warn-and-return, never a panic, even with a nil logger.
func TestGCOldArchives(t *testing.T) {
	in := &Installer{Logger: slog.New(slog.DiscardHandler)}
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1.0.0.ncplugin")
	write("2.0.0.ncplugin")
	write("3.0.0.ncplugin")
	write("notes.txt")
	if err := os.Mkdir(filepath.Join(dir, "0.9.0.ncplugin"), 0o750); err != nil {
		t.Fatal(err)
	}

	in.gcOldArchives(dir, "3.0.0", "2.0.0")

	want := []string{"0.9.0.ncplugin", "2.0.0.ncplugin", "3.0.0.ncplugin", "notes.txt"}
	if names := archiveDirNames(t, dir); !slices.Equal(names, want) {
		t.Fatalf("dir = %v, want %v", names, want)
	}

	in.gcOldArchives(filepath.Join(dir, "nope"), "3.0.0", "2.0.0")
	(&Installer{}).gcOldArchives(filepath.Join(dir, "nope"), "3.0.0", "2.0.0")
}
