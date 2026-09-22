package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// addQuotaUser creates a user with a byte quota (ADR-0061 user scope).
func (f *storageFixture) addQuotaUser(t *testing.T, uid string, quota int64) {
	t.Helper()
	if err := f.dav.Users.Create(context.Background(), &users.User{
		UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true, QuotaBytes: &quota,
	}); err != nil {
		t.Fatal(err)
	}
}

func userCtx(uid string) context.Context {
	return WithCallContext(context.Background(), CallContext{UserID: uid})
}

func TestSystemTreeUsage(t *testing.T) {
	ctx := context.Background()
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, st, "root/a.txt", "aaa")                  // 3
	writeTreeFile(t, st, "root/sub/b.txt", "bbbb")             // 4
	writeTreeFile(t, st, "root/sub/deep/c.txt", "ccccc")       // 5
	writeTreeFile(t, st, "sibling/outside.txt", "not-counted") // outside root

	tree, old, err := systemTreeUsage(ctx, st, "root", "root/sub/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if tree != 12 || old != 4 {
		t.Fatalf("tree=%d old=%d, want 12/4", tree, old)
	}

	// Missing target Stat: old = 0, tree unchanged.
	if _, old, err = systemTreeUsage(ctx, st, "root", "root/new.bin"); err != nil || old != 0 {
		t.Fatalf("missing target: old=%d err=%v", old, err)
	}
	// A directory target contributes 0 to the overwrite delta.
	if _, old, err = systemTreeUsage(ctx, st, "root", "root/sub"); err != nil || old != 0 {
		t.Fatalf("dir target: old=%d err=%v", old, err)
	}
	// Empty tree: existing but empty root sums to 0.
	if err := st.Mkdir(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	if tree, _, err = systemTreeUsage(ctx, st, "empty", "empty/x"); err != nil || tree != 0 {
		t.Fatalf("empty tree: tree=%d err=%v", tree, err)
	}
	// Missing root is not an error: the plugin never wrote system storage.
	if tree, old, err = systemTreeUsage(ctx, st, "never/existed", "never/existed/x"); err != nil || tree != 0 || old != 0 {
		t.Fatalf("missing root: tree=%d old=%d err=%v", tree, old, err)
	}
}

func TestPluginSystemQuotaDefaulting(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	if h.cfg.PluginSystemQuotaBytes != defaultPluginSystemQuotaBytes {
		t.Errorf("PluginSystemQuotaBytes = %d, want %d", h.cfg.PluginSystemQuotaBytes, defaultPluginSystemQuotaBytes)
	}
	h2, _ := testHost(t, HostConfig{PluginSystemQuotaBytes: 4096})
	if h2.cfg.PluginSystemQuotaBytes != 4096 {
		t.Errorf("PluginSystemQuotaBytes = %d, want 4096", h2.cfg.PluginSystemQuotaBytes)
	}
}

func TestStorageUserQuotaCreateRejected(t *testing.T) {
	f := newStorageFixture(t)
	f.addQuotaUser(t, "carol", 15)
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, userCtx("carol"), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageCreateSizeProbeModule("user:/big.bin", 20, ErrCodeQuotaExceeded))
	requireLogMarkers(t, buf.String(), "probe-ok",
		"plugins: storage quota exceeded", "plugin=com.example.probe", "scope=user", "quota_bytes=15")
	if _, err := f.dav.Stat(context.Background(), "carol", "/big.bin"); err == nil {
		t.Fatal("quota-refused create must not produce a file")
	}
}

func TestStorageUserQuotaCommitRejected(t *testing.T) {
	f := newStorageFixture(t)
	f.addQuotaUser(t, "carol", 15)
	h, buf := testHost(t, f.hostConfig())
	// Declared size unknown (-1): only the commit backstop can refuse.
	installModuleCtx(t, userCtx("carol"), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageWriteCloseProbeModule("user:/big.txt", "01234567890123456789", ErrCodeQuotaExceeded))
	requireLogMarkers(t, buf.String(), "close-ok", "plugins: storage quota exceeded", "write_bytes=20")
	if _, err := f.dav.Stat(context.Background(), "carol", "/big.txt"); err == nil {
		t.Fatal("quota-refused commit must not produce a filecache entry")
	}
	if _, err := os.Stat(filepath.Join(f.root, "carol", "big.txt")); !os.IsNotExist(err) {
		t.Fatalf("quota-refused commit must not land on disk: %v", err)
	}
}

func TestStorageUserQuotaNilUnlimited(t *testing.T) {
	f := newStorageFixture(t)
	h, buf := testHost(t, f.hostConfig())
	// alice has no quota row value (nil = unlimited): even a declared size
	// far above any reasonable cap passes both checkpoints.
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageCreateSizeProbeModule("user:/unlimited.bin", 1<<40, 0))
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageWriteCloseProbeModule("user:/free.txt", "no-quota", 0))
	out := buf.String()
	requireLogMarkers(t, out, "probe-ok", "close-ok")
	if strings.Contains(out, "quota exceeded") {
		t.Fatalf("nil quota must never refuse: %q", out)
	}
}

func TestStorageUserQuotaOverwriteDelta(t *testing.T) {
	f := newStorageFixture(t)
	ctx := context.Background()
	f.addQuotaUser(t, "dave", 15)
	// Pre-existing 10-byte file: usage 10, old 10, so a 12-byte overwrite
	// costs only the delta (10 - 10 + 12 = 12 <= 15).
	if _, _, err := f.dav.Write(ctx, "dave", "/note.txt", strings.NewReader("0123456789"), nil); err != nil {
		t.Fatal(err)
	}
	h, buf := testHost(t, f.hostConfig())
	installModuleCtx(t, userCtx("dave"), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageWriteCloseProbeModule("user:/note.txt", "0123456789ab", 0))
	requireLogMarkers(t, buf.String(), "close-ok")
	ent, err := f.dav.Stat(ctx, "dave", "/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.Size != 12 {
		t.Fatalf("overwritten size = %d, want 12", ent.Size)
	}
	// Growing past the quota (12 - 12 + 16 = 16 > 15) refuses at commit.
	installModuleCtx(t, userCtx("dave"), h, storageManifest(nil, []string{"user"}),
		wasmgen.StorageWriteCloseProbeModule("user:/note.txt", "0123456789abcdef", ErrCodeQuotaExceeded))
	requireLogMarkers(t, buf.String(), "plugins: storage quota exceeded")
	if ent, err := f.dav.Stat(ctx, "dave", "/note.txt"); err != nil || ent.Size != 12 {
		t.Fatalf("refused overwrite must leave the old file: ent=%+v err=%v", ent, err)
	}
}

func TestStorageSystemQuotaCreateRejected(t *testing.T) {
	f := newStorageFixture(t)
	// 8 bytes already in the plugin tree; quota 10.
	f.systemWrite(t, "com.example.probe", "conf/app.json", "12345678")
	cfg := f.hostConfig()
	cfg.PluginSystemQuotaBytes = 10
	h, buf := testHost(t, cfg)
	m := storageManifest(nil, []string{"system"})
	// Declared 20 over an empty remainder and declared 5 over the 8-byte
	// tree (8 + 5 = 13 > 10) both refuse at create.
	installModuleCtx(t, aliceCtx(), h, m,
		wasmgen.StorageCreateSizeProbeModule("system:/big.bin", 20, ErrCodeQuotaExceeded))
	installModuleCtx(t, aliceCtx(), h, m,
		wasmgen.StorageCreateSizeProbeModule("system:/small.bin", 5, ErrCodeQuotaExceeded))
	out := buf.String()
	if n := strings.Count(out, "probe-ok"); n != 2 {
		t.Fatalf("probe-ok count = %d, want 2: %q", n, out)
	}
	requireLogMarkers(t, out, "plugins: storage quota exceeded", "scope=system", "quota_bytes=10", "usage_bytes=8")
	if _, err := f.sysSt.Stat(context.Background(), testSystemPrefix+"/com.example.probe/big.bin"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("quota-refused create must not produce a file: %v", err)
	}
}

func TestStorageSystemQuotaCommitRejected(t *testing.T) {
	f := newStorageFixture(t)
	cfg := f.hostConfig()
	cfg.PluginSystemQuotaBytes = 10
	h, buf := testHost(t, cfg)
	// Declared size unknown: the 12 actual bytes cross quota 10 at close.
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"system"}),
		wasmgen.StorageWriteCloseProbeModule("system:/x.bin", "0123456789ab", ErrCodeQuotaExceeded))
	requireLogMarkers(t, buf.String(), "close-ok", "plugins: storage quota exceeded", "write_bytes=12")
	if _, err := f.sysSt.Stat(context.Background(), testSystemPrefix+"/com.example.probe/x.bin"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("quota-refused commit must remove the partial file: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(f.sysRoot, testSystemPrefix, "com.example.probe", ".ncgo-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("backend temp files leaked: %v", matches)
	}
}

func TestStorageSystemQuotaOverwriteDelta(t *testing.T) {
	f := newStorageFixture(t)
	// Existing 8-byte target; quota 10: a 6-byte overwrite costs only the
	// delta (8 - 8 + 6 = 6 <= 10).
	f.systemWrite(t, "com.example.probe", "conf/app.json", "12345678")
	cfg := f.hostConfig()
	cfg.PluginSystemQuotaBytes = 10
	h, buf := testHost(t, cfg)
	installModuleCtx(t, aliceCtx(), h, storageManifest(nil, []string{"system"}),
		wasmgen.StorageWriteCloseProbeModule("system:/conf/app.json", "ABCDEF", 0))
	requireLogMarkers(t, buf.String(), "close-ok")
	raw, err := os.ReadFile(filepath.Join(f.sysRoot, testSystemPrefix, "com.example.probe", "conf", "app.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ABCDEF" {
		t.Fatalf("overwritten content = %q", raw)
	}
}
