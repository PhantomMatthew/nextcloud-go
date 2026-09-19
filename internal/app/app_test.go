package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestGoldenReplay(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })
	seedPhase1DAV(t, a)

	maintCfg := DevConfig()
	maintCfg.Database.DSN = "file:ncgo-dev-maint?mode=memory&cache=shared"
	maintCfg.Maintenance.Enabled = true
	maintCfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	ma, err := New(ctx, maintCfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ma.Close(ctx) })

	root := filepath.Join(repoRoot(t), "testdata", "golden")
	dirs, err := goldentest.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no golden cases")
	}
	for _, dir := range dirs {
		c, err := goldentest.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		h := a.Handler()
		for _, tag := range c.Tags {
			if tag == "maintenance" {
				h = ma.Handler()
				break
			}
		}
		t.Run(c.ID, func(t *testing.T) {
			goldentest.RunHandler(t, c, h)
		})
	}
}

func seedPhase1DAV(t *testing.T, a *App) {
	t.Helper()
	d, ok := a.davFS.(*files.DAV)
	if !ok {
		t.Fatal("davFS is not *files.DAV")
	}
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	d.Clock = func() time.Time { return freeze }
	d.NewToken = func() string { return "opaquelocktoken:ncgo0000000000000000000000000001" }
	if a.shares != nil {
		a.shares.Clock = func() time.Time { return freeze }
		a.shares.NewToken = func() string { return "ncgopublic00001" }
	}
	if up, ok := a.uploadsFS.(*files.Uploads); ok {
		up.Clock = func() time.Time { return freeze }
	}
	if a.trashFS != nil {
		a.trashFS.Clock = func() time.Time { return freeze }
	}
	if a.versionsFS != nil {
		a.versionsFS.Clock = func() time.Time { return freeze }
	}
	mt := freeze
	if _, _, err := a.davFS.Write(context.Background(), "admin", "/hello.txt", strings.NewReader("hello world\n"), &mt); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureWebDAVGoldens(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to rewrite testdata/golden/webdav responses")
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	for _, area := range []string{"search", "sharing", "capabilities", "webdav"} {
		areaCfg := DevConfig()
		areaCfg.Database.DSN = "file:ncgo-golden-" + area + "?mode=memory&cache=shared"
		areaCfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		a, err := New(ctx, areaCfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		seedPhase1DAV(t, a)
		h := a.Handler()
		root := filepath.Join(repoRoot(t), "testdata", "golden", area)
		dirs, err := goldentest.Discover(root)
		if err != nil {
			_ = a.Close(ctx)
			t.Fatal(err)
		}
		for _, dir := range dirs {
			c, err := goldentest.Load(dir)
			if err != nil {
				_ = a.Close(ctx)
				t.Fatal(err)
			}
			got, err := goldentest.Execute(ctx, c, func(req *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				return rec.Result(), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "HTTP/1.1 %d %s\n", got.Status, http.StatusText(got.Status))
			fmt.Fprintf(&buf, "Server: nginx\n")
			keys := make([]string, 0, len(got.Headers))
			for k := range got.Headers {
				switch http.CanonicalHeaderKey(k) {
				case "Date", "X-Request-Id", "Set-Cookie", "Server":
					continue
				}
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if http.CanonicalHeaderKey(k) == "Content-Length" {
					continue
				}
				for _, v := range got.Headers.Values(k) {
					fmt.Fprintf(&buf, "%s: %s\n", k, v)
				}
			}
			buf.WriteByte('\n')
			buf.Write(got.Body)
			out := filepath.Join(dir, "response.http")
			if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenStorageS3(t *testing.T) {
	cfg := DevConfig()
	cfg.Storage.DefaultBackend = "s3_primary"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"s3_primary": {
			Type:            "s3",
			Endpoint:        "http://127.0.0.1:9",
			Bucket:          "ncgo",
			AccessKeyID:     "ak",
			SecretAccessKey: "sk",
			Region:          "us-east-1",
		},
	}
	st, err := openStorage(cfg)
	if err != nil || st == nil {
		t.Fatalf("openStorage s3 = %v %v", st, err)
	}
}

func TestOpenStorageUnknown(t *testing.T) {
	cfg := DevConfig()
	cfg.Storage.DefaultBackend = "mystery"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"mystery": {Type: "mystery"},
	}
	if _, err := openStorage(cfg); err == nil {
		t.Fatal("expected unsupported type")
	}
}
