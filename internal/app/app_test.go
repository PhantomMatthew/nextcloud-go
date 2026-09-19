package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

	"github.com/PhantomMatthew/nextcloud-go/internal/activity"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
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
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var areas []string
	for _, ent := range entries {
		if ent.IsDir() {
			areas = append(areas, ent.Name())
		}
	}
	sort.Strings(areas)
	if len(areas) == 0 {
		t.Fatal("no golden areas")
	}
	for _, area := range areas {
		t.Run(area, func(t *testing.T) {
			areaCfg := DevConfig()
			areaCfg.Database.DSN = "file:ncgo-replay-" + area + "?mode=memory&cache=shared"
			areaCfg.Storage.Backends = map[string]config.BackendConfig{
				"local": {Type: "localfs", Root: t.TempDir()},
			}
			a, err := New(ctx, areaCfg, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.Close(ctx) })
			a.UseHTTPClient(&http.Client{Timeout: 15 * time.Second, Transport: remoteOCMRoundTripper{}})
			seedPhase1DAV(t, a)
			if area == "ocm" {
				freezeOCMShareTokens(a)
			}
			dirs, err := goldentest.Discover(filepath.Join(root, area))
			if err != nil {
				t.Fatal(err)
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
	if a.calendarStore != nil {
		a.calendarStore.Clock = func() time.Time { return freeze }
	}
	if a.calendarFS != nil {
		a.calendarFS.Clock = func() time.Time { return freeze }
	}
	if a.contactsStore != nil {
		a.contactsStore.Clock = func() time.Time { return freeze }
	}
	if a.contactsFS != nil {
		a.contactsFS.Clock = func() time.Time { return freeze }
	}
	if a.notifStore != nil {
		a.notifStore.Clock = func() time.Time { return freeze }
	}
	if a.activityStore != nil {
		a.activityStore.Clock = func() time.Time { return freeze }
	}
	if a.principalFS != nil {
		a.principalFS.Clock = func() time.Time { return freeze }
	}
	if a.davRootFS != nil {
		a.davRootFS.Clock = func() time.Time { return freeze }
	}
	mt := freeze
	if _, _, err := a.davFS.Write(context.Background(), "admin", "/hello.txt", strings.NewReader("hello world\n"), &mt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Users.GetByUID(context.Background(), "bob"); err != nil {
		hash, herr := a.hasher.Hash("bob")
		if herr != nil {
			t.Fatal(herr)
		}
		if err := a.Users.Create(context.Background(), &users.User{
			UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.davFS.Stat(context.Background(), "bob", "/"); err != nil {
		t.Fatal(err)
	}
	bobU, err := a.Users.GetByUID(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	root, err := a.fileMeta.GetByPath(context.Background(), bobU.ID, "/")
	if err != nil {
		t.Fatal(err)
	}
	root.Mtime = freeze
	if err := a.fileMeta.UpdateMeta(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	adminU, err := a.Users.GetByUID(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if a.notifStore != nil {
		if err := a.notifStore.Insert(context.Background(), &notifications.Notification{
			UserID:                adminU.ID,
			App:                   "files_sharing",
			UserUID:               "admin",
			ObjectType:            "share",
			ObjectID:              "1",
			Subject:               "You received a share of hello.txt",
			SubjectRich:           "You received a share of {file}",
			SubjectRichParameters: `{"file":{"type":"file","id":"2","name":"hello.txt"}}`,
			ShouldNotify:          true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if a.activityStore != nil {
		if err := a.activityStore.Insert(context.Background(), &activity.Event{
			UserID:                adminU.ID,
			ActorUID:              "admin",
			App:                   "files",
			Type:                  "file_created",
			Subject:               "admin created hello.txt",
			SubjectRich:           "{user} created {file}",
			SubjectRichParameters: `{"user":{"type":"user","id":"admin","name":"admin"},"file":{"type":"file","id":"2","name":"hello.txt","path":"/hello.txt"}}`,
			ObjectType:            "files",
			ObjectID:              2,
			ObjectName:            "/hello.txt",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCaptureWebDAVGoldens(t *testing.T) {
	if os.Getenv("UPDATE_GOLDEN") != "1" {
		t.Skip("set UPDATE_GOLDEN=1 to rewrite testdata/golden/webdav responses")
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	for _, area := range []string{"activity", "search", "sharing", "capabilities", "webdav", "caldav", "carddav", "notifications", "ocm"} {
		if want := os.Getenv("GOLDEN_AREA"); want != "" && want != area {
			continue
		}
		areaCfg := DevConfig()
		areaCfg.Database.DSN = "file:ncgo-golden-" + area + "?mode=memory&cache=shared"
		areaCfg.Storage.Backends = map[string]config.BackendConfig{
			"local": {Type: "localfs", Root: t.TempDir()},
		}
		a, err := New(ctx, areaCfg, logger)
		if err != nil {
			t.Fatal(err)
		}
		a.UseHTTPClient(&http.Client{Timeout: 15 * time.Second, Transport: remoteOCMRoundTripper{}})
		seedPhase1DAV(t, a)
		if area == "ocm" {
			freezeOCMShareTokens(a)
		}
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

func freezeOCMShareTokens(a *App) {
	if a == nil || a.shares == nil {
		return
	}
	n := 0
	a.shares.NewToken = func() string {
		n++
		return fmt.Sprintf("ncgopublic%05d", n)
	}
}

type remoteOCMRoundTripper struct{}

func (remoteOCMRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != "remote.example.com" {
		return nil, errors.New("connection refused")
	}
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	switch {
	case req.Method == http.MethodGet && (req.URL.Path == "/.well-known/ocm" || req.URL.Path == "/ocm-provider"):
		body := `{"enabled":true,"apiVersion":"1.0-proposal1","endPoint":"https://remote.example.com/ocm"}`
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	case req.Method == http.MethodPost && req.URL.Path == "/ocm/shares":
		return &http.Response{StatusCode: http.StatusCreated, Header: hdr, Body: io.NopCloser(strings.NewReader(`{"recipientDisplayName":"bob"}`)), Request: req}, nil
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Header: hdr, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
}

func TestUseHTTPClientNil(t *testing.T) {
	var a *App
	a.UseHTTPClient(&http.Client{})
	(&App{}).UseHTTPClient(&http.Client{})
}
