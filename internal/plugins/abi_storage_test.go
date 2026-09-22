package plugins

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const testSystemPrefix = "appdata_test/plugins"

type storageFixture struct {
	dav     *files.DAV
	root    string
	sysSt   storage.Storage
	sysRoot string
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st, err := localfs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	sysRoot := t.TempDir()
	sysSt, err := localfs.New(sysRoot)
	if err != nil {
		t.Fatal(err)
	}
	return &storageFixture{dav: files.NewDAV(st, files.NewSQLStore(db), us), root: root, sysSt: sysSt, sysRoot: sysRoot}
}

func (f *storageFixture) hostConfig() HostConfig {
	return HostConfig{Files: f.dav, SystemStorage: f.sysSt, SystemPrefix: testSystemPrefix}
}

// mkdirAndWrite creates parent dirs and a file for alice through the DAV.
func (f *storageFixture) mkdirAndWrite(t *testing.T, dir, path, content string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.dav.Mkdir(ctx, "alice", dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.dav.Write(ctx, "alice", path, bytes.NewReader([]byte(content)), nil); err != nil {
		t.Fatal(err)
	}
}

// systemWrite places content directly in the system backend for plugin id.
func (f *storageFixture) systemWrite(t *testing.T, pluginID, rel, content string) {
	t.Helper()
	full := testSystemPrefix + "/" + pluginID + "/" + rel
	wc, err := f.sysSt.Create(context.Background(), full, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
}

func storageManifest(read, write []string) *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{Storage: StorageCapabilities{Read: read, Write: write}}
	return m
}

func aliceCtx() context.Context {
	return WithCallContext(context.Background(), CallContext{UserID: "alice"})
}

func requireLogMarkers(t *testing.T, out string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if !strings.Contains(out, m) {
			t.Fatalf("marker %q missing from log: %q", m, out)
		}
	}
}

func TestStorageUserEndToEnd(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()
	if _, err := f.dav.Mkdir(ctx, "alice", "/ncgo-test"); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, f.hostConfig())
	p, err := h.Load(ctx, storageManifest([]string{"user"}, []string{"user"}),
		wasmgen.StorageModule(
			"user:/ncgo-test/hello.txt", "user:/ncgo-test/renamed.txt", "user:/ncgo-test",
			"hello world", "system:/etc/x", ErrCodePermissionDenied,
		))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if _, err := p.Call(aliceCtx(), "do_storage"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out,
		"commit-ok", "stat-ok", "read-ok", "list-ok", "rename-ok", "stat2-ok", "delete-ok", "gone-ok", "denied-ok")
	if !strings.Contains(out, "hello world") {
		t.Fatalf("read-back content missing: %q", out)
	}
	if !strings.Contains(out, "hello.txt") || !strings.Contains(out, "renamed.txt") {
		t.Fatalf("stat/list payloads missing paths: %q", out)
	}
	// The script deleted the file at the end: gone from disk and filecache.
	if _, err := os.Stat(filepath.Join(f.root, "alice", "ncgo-test", "renamed.txt")); !os.IsNotExist(err) {
		t.Fatalf("renamed.txt still on disk: %v", err)
	}
	if _, err := f.dav.Stat(ctx, "alice", "/ncgo-test/renamed.txt"); err == nil {
		t.Fatal("renamed.txt still in filecache")
	}
}

func TestStorageUserWriteLandsOnDisk(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()
	if _, err := f.dav.Mkdir(ctx, "alice", "/docs"); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, aliceCtx(), h, storageManifest([]string{"user"}, []string{"user"}),
		wasmgen.StorageWriteProbeModule("user:/docs/landing.txt", "landed-content", int32(len("landed-content"))))
	if !strings.Contains(buf.String(), "spool-ok") {
		t.Fatalf("log %q", buf.String())
	}
	// The content really landed in the localfs under <uid>/...
	raw, err := os.ReadFile(filepath.Join(f.root, "alice", "docs", "landing.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "landed-content" {
		t.Fatalf("disk content = %q", raw)
	}
	// ...and the filecache saw the commit (etag/size consistent).
	ent, err := f.dav.Stat(ctx, "alice", "/docs/landing.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.Size != int64(len("landed-content")) || ent.ETag == "" {
		t.Fatalf("filecache entry = %+v", ent)
	}
	// A second guest sees the committed file through the DAV.
	installModuleCtx(t, aliceCtx(), h, storageManifest([]string{"user"}, nil),
		wasmgen.StorageReadProbeModule("user:/docs/landing.txt", "user:/docs"))
	out := buf.String()
	requireLogMarkers(t, out, "stat-ok", "open-ok", "list-ok")
	if !strings.Contains(out, "landed-content") {
		t.Fatalf("second guest read-back missing: %q", out)
	}
}

func TestStorageSystemEndToEnd(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()
	h, buf := testHost(t, f.hostConfig())
	p, err := h.Load(ctx, storageManifest([]string{"system"}, []string{"system"}),
		wasmgen.StorageModule(
			"system:/ncgo-test/hello.txt", "system:/ncgo-test/renamed.txt", "system:/ncgo-test",
			"system-data", "user:/etc/x", ErrCodePermissionDenied,
		))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if _, err := p.Call(aliceCtx(), "do_storage"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out,
		"commit-ok", "stat-ok", "read-ok", "list-ok", "rename-ok", "stat2-ok", "delete-ok", "gone-ok", "denied-ok")
	if !strings.Contains(out, "system-data") {
		t.Fatalf("read-back content missing: %q", out)
	}
	pluginDir := filepath.Join(f.sysRoot, testSystemPrefix, "com.example.probe")
	if _, err := os.Stat(filepath.Join(pluginDir, "ncgo-test", "renamed.txt")); !os.IsNotExist(err) {
		t.Fatalf("renamed.txt still on disk: %v", err)
	}
}

func TestStorageSystemWriteLandsOnDisk(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"system"}),
		wasmgen.StorageWriteProbeModule("system:/conf/app.json", "sys-content", int32(len("sys-content"))))
	if !strings.Contains(buf.String(), "spool-ok") {
		t.Fatalf("log %q", buf.String())
	}
	raw, err := os.ReadFile(filepath.Join(f.sysRoot, testSystemPrefix, "com.example.probe", "conf", "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "sys-content" {
		t.Fatalf("disk content = %q", raw)
	}
}

func TestStorageSystemIsolationBetweenPlugins(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	am := storageManifest(nil, []string{"system"})
	am.Plugin.ID = "com.example.a"
	installModuleCtx(t, aliceCtx(), h, am,
		wasmgen.StorageWriteProbeModule("system:/conf/a.json", "a-data", int32(len("a-data"))))
	if _, err := os.Stat(filepath.Join(f.sysRoot, testSystemPrefix, "com.example.a", "conf", "a.json")); err != nil {
		t.Fatal(err)
	}
	// Plugin B cannot see plugin A's system tree.
	bm := storageManifest([]string{"system"}, nil)
	bm.Plugin.ID = "com.example.b"
	installModuleCtx(t, aliceCtx(), h, bm,
		wasmgen.StorageStatProbeModule("system:/conf/a.json", ErrCodeNotFound))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestStorageDeniedNoGrants(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, aliceCtx(), h, probeManifest(), wasmgen.StorageStatProbeModule("user:/docs/a.txt", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, probeManifest(), wasmgen.StorageStatProbeModule("system:/x", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, probeManifest(), wasmgen.StorageOpProbeModule("create", "user:/x.txt", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, probeManifest(), wasmgen.StorageOpProbeModule("delete", "user:/docs/a.txt", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, probeManifest(), wasmgen.StorageOpProbeModule("rename", "user:/docs/a.txt", "user:/docs/b.txt", ErrCodePermissionDenied))
	if n := strings.Count(buf.String(), "probe-ok"); n != 5 {
		t.Fatalf("probe-ok count = %d, want 5: %q", n, buf.String())
	}
}

func TestStorageUserReadOnly(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"user"}, nil)
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageReadProbeModule("user:/docs/a.txt", "user:/docs"))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("create", "user:/docs/b.txt", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("delete", "user:/docs/a.txt", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("rename", "user:/docs/a.txt", "user:/docs/b.txt", ErrCodePermissionDenied))
	out := buf.String()
	requireLogMarkers(t, out, "stat-ok", "open-ok", "list-ok")
	if n := strings.Count(out, "probe-ok"); n != 3 {
		t.Fatalf("probe-ok count = %d, want 3: %q", n, out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("read content missing: %q", out)
	}
}

func TestStorageSystemReadOnly(t *testing.T) {
	f := newStorageFixture(t)
	f.systemWrite(t, "com.example.probe", "conf/app.json", "cfg")
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"system"}, nil)
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageReadProbeModule("system:/conf/app.json", "system:/conf"))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("create", "system:/conf/b.json", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("delete", "system:/conf/app.json", "", ErrCodePermissionDenied))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageOpProbeModule("rename", "system:/conf/app.json", "system:/conf/b.json", ErrCodePermissionDenied))
	out := buf.String()
	requireLogMarkers(t, out, "stat-ok", "open-ok", "list-ok")
	if n := strings.Count(out, "probe-ok"); n != 3 {
		t.Fatalf("probe-ok count = %d, want 3: %q", n, out)
	}
	if !strings.Contains(out, "cfg") {
		t.Fatalf("read content missing: %q", out)
	}
}

func TestStorageUnknownSchemeRejected(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"user", "system"}, []string{"user", "system"})
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("bogus:/x", ErrCodeInvalidArgument))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestStorageDotDotPathRejected(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"user", "system"}, []string{"user", "system"})
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("user:/../etc/passwd", ErrCodeInvalidArgument))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("../escape", ErrCodeInvalidArgument))
	if n := strings.Count(buf.String(), "probe-ok"); n != 2 {
		t.Fatalf("probe-ok count = %d, want 2: %q", n, buf.String())
	}
}

func TestStorageEmptyUserIDUnavailable(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	m := storageManifest([]string{"user"}, []string{"user"})
	// No CallContext: the user scope has no identity.
	installModule(t, h, m, wasmgen.StorageStatProbeModule("user:/x", ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestStorageNilFilesUnavailable(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, HostConfig{SystemStorage: f.sysSt, SystemPrefix: testSystemPrefix})
	m := storageManifest([]string{"user", "system"}, []string{"user", "system"})
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("user:/x", ErrCodeUnavailable))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("system:/x", ErrCodeNotFound))
	if n := strings.Count(buf.String(), "probe-ok"); n != 2 {
		t.Fatalf("probe-ok count = %d, want 2: %q", n, buf.String())
	}
}

func TestStorageNilSystemUnavailable(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	h, buf := testHost(t, HostConfig{Files: f.dav})
	m := storageManifest([]string{"user", "system"}, []string{"user", "system"})
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageStatProbeModule("system:/x", ErrCodeUnavailable))
	installModuleCtx(t, aliceCtx(), h, m, wasmgen.StorageReadProbeModule("user:/docs/a.txt", "user:/docs"))
	out := buf.String()
	requireLogMarkers(t, out, "probe-ok", "stat-ok", "open-ok", "list-ok")
}

func TestStorageStreamBudget(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, aliceCtx(), h, storageManifest([]string{"user"}, nil),
		wasmgen.StorageOpenLoopModule("user:/docs/a.txt", 65, ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "budget-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func countSpoolFiles(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "ncgo-plugin-spool-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func TestStorageCloseAllCleansLeakedHandles(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")
	h, buf := testHost(t, f.hostConfig())
	before := countSpoolFiles(t)
	installModuleCtx(t, aliceCtx(), h, storageManifest([]string{"user"}, []string{"user"}),
		wasmgen.StorageLeakModule("user:/docs/a.txt", "user:/docs/leaked.txt", "leak-content"))
	if !strings.Contains(buf.String(), "leak-ok") {
		t.Fatalf("log %q", buf.String())
	}
	// per_request instances are destroyed on release: the leaked spool must
	// be discarded (never committed) and its temp file removed.
	if after := countSpoolFiles(t); after != before {
		t.Fatalf("spool temp files before=%d after=%d", before, after)
	}
	if _, err := f.dav.Stat(context.Background(), "alice", "/docs/leaked.txt"); err == nil {
		t.Fatal("leaked spool must not be committed")
	}
	if _, err := os.Stat(filepath.Join(f.root, "alice", "docs", "leaked.txt")); !os.IsNotExist(err) {
		t.Fatalf("leaked.txt on disk: %v", err)
	}
}

func TestStorageOversizeSpoolRejected(t *testing.T) {
	f := newStorageFixture(t)
	cfg := f.hostConfig()
	cfg.MaxSpoolBytes = 8
	h, buf := testHost(t, cfg)
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageWriteProbeModule("user:/big.txt", "0123456789", ErrCodeTooLarge))
	if !strings.Contains(buf.String(), "spool-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestStorageEventUserIDEnablesUserScope(t *testing.T) {
	f := newStorageFixture(t)
	f.mkdirAndWrite(t, "/evt", "/evt/note.txt", "event-data")
	bus := events.NewBus(nil)
	cfg := f.hostConfig()
	cfg.Bus = bus
	h, buf := testHost(t, cfg)
	m := probeManifest()
	m.Plugin.ID = "com.example.evt"
	m.EntryPoints.OnEvent = "ncgo_on_event"
	m.Capabilities = Capabilities{
		Events:  EventsCapabilities{Subscribe: []string{"files.*"}},
		Storage: StorageCapabilities{Read: []string{"user"}},
	}
	attachModule(t, h, m, wasmgen.StorageEventStatModule("user:/evt/note.txt"))

	// The event's UserID becomes the delivery call's user identity.
	bus.Publish(context.Background(), events.Event{Topic: "files.uploaded", UserID: "alice", Source: "host"})
	out := buf.String()
	if !strings.Contains(out, "user-ok") || !strings.Contains(out, "note.txt") {
		t.Fatalf("event-driven user stat failed: %q", out)
	}

	// Without an event UserID the user scope has no identity: no delivery stat.
	bus.Publish(context.Background(), events.Event{Topic: "files.uploaded", Source: "host"})
	if n := strings.Count(buf.String(), "user-ok"); n != 1 {
		t.Fatalf("user-ok count = %d, want 1: %q", n, buf.String())
	}
}

func TestStorageEventExplicitCallContextWins(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()
	f.mkdirAndWrite(t, "/evt", "/evt/alice.txt", "a")
	bus := events.NewBus(nil)
	cfg := f.hostConfig()
	cfg.Bus = bus
	h, buf := testHost(t, cfg)
	m := probeManifest()
	m.Plugin.ID = "com.example.evt2"
	m.EntryPoints.OnEvent = "ncgo_on_event"
	m.Capabilities = Capabilities{
		Events:  EventsCapabilities{Subscribe: []string{"files.*"}},
		Storage: StorageCapabilities{Read: []string{"user"}},
	}
	attachModule(t, h, m, wasmgen.StorageEventStatModule("user:/evt/alice.txt"))

	// Explicit call metadata on the publish ctx (bob) beats the event's
	// UserID (alice): bob cannot see alice's file, so no user-ok.
	us := f.dav.Users
	if err := us.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	publishCtx := WithCallContext(ctx, CallContext{UserID: "bob"})
	bus.Publish(publishCtx, events.Event{Topic: "files.uploaded", UserID: "alice", Source: "host"})
	if strings.Contains(buf.String(), "user-ok") {
		t.Fatalf("explicit call context must win over event UserID: %q", buf.String())
	}
}
