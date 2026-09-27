package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
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
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
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
	for uid, email := range map[string]string{"admin": "admin@example.com", "bob": "bob@example.com"} {
		if _, err := a.DB.Exec(context.Background(), `UPDATE users SET email = ? WHERE uid = ?`, email, uid); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Users.CreateGroup(context.Background(), &users.Group{GID: "engineers", DisplayName: "Engineers"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Users.AddGroupMember(context.Background(), "engineers", "bob"); err != nil {
		t.Fatal(err)
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
	st, _, _, err := openStorage(cfg, nil)
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
	if _, _, _, err := openStorage(cfg, nil); err == nil {
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
	if req.URL.Hostname() == "lookup.nextcloud.com" {
		hdr := make(http.Header)
		hdr.Set("Content-Type", "application/json")
		body := "[]"
		if req.URL.Query().Get("search") == "bob" {
			body = `[{"federationId":"bob@https://remote.example.com","name":"Bob Remote"}]`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	if req.URL.Hostname() != "remote.example.com" {
		return nil, errors.New("connection refused")
	}
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	path := req.URL.Path
	switch {
	case req.Method == http.MethodGet && (path == "/.well-known/ocm" || path == "/ocm-provider"):
		body := `{"enabled":true,"apiVersion":"1.0-proposal1","endPoint":"https://remote.example.com/ocm"}`
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	case req.Method == http.MethodPost && path == "/ocm/shares":
		return &http.Response{StatusCode: http.StatusCreated, Header: hdr, Body: io.NopCloser(strings.NewReader(`{"recipientDisplayName":"bob"}`)), Request: req}, nil
	case req.Method == http.MethodPost && path == "/ocm/notifications":
		return &http.Response{StatusCode: http.StatusCreated, Header: hdr, Body: io.NopCloser(strings.NewReader("[]")), Request: req}, nil
	case req.Method == "PROPFIND" && strings.HasPrefix(path, "/public.php/webdav"):
		body := remotePublicPropfindXML(path)
		hdr = make(http.Header)
		hdr.Set("Content-Type", "application/xml")
		return &http.Response{StatusCode: http.StatusMultiStatus, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	case req.Method == http.MethodGet && (path == "/public.php/webdav/child.txt"):
		body := "child\n"
		hdr = make(http.Header)
		hdr.Set("Content-Type", "text/plain")
		hdr.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		hdr.Set("ETag", `"child"`)
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	case req.Method == http.MethodPut && path == "/public.php/webdav/child.txt":
		hdr = make(http.Header)
		hdr.Set("ETag", `"put"`)
		return &http.Response{StatusCode: http.StatusCreated, Header: hdr, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	case req.Method == http.MethodGet && (path == "/public.php/webdav" || path == "/public.php/webdav/"):
		body := "hello from remote\n"
		hdr = make(http.Header)
		hdr.Set("Content-Type", "text/plain")
		hdr.Set("Content-Length", fmt.Sprintf("%d", len(body)))
		hdr.Set("ETag", `"ocm-remote"`)
		hdr.Set("Last-Modified", "Thu, 01 May 2025 12:00:00 GMT")
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req}, nil
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Header: hdr, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
}

func remotePublicPropfindXML(path string) string {
	child := `<d:response><d:href>/public.php/webdav/child.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>6</d:getcontentlength><d:getetag>&quot;child&quot;</d:getetag><d:getcontenttype>text/plain</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	self := `<d:response><d:href>/public.php/webdav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	if strings.HasSuffix(path, "child.txt") {
		return `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">` + child + `</d:multistatus>`
	}
	return `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">` + self + child + `</d:multistatus>`
}

func TestUseHTTPClientNil(t *testing.T) {
	var a *App
	a.UseHTTPClient(&http.Client{})
	(&App{}).UseHTTPClient(&http.Client{})
}

func writeTestMasterKey(t *testing.T) string {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "master.key")
	key := make([]byte, encrypt.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

// TestOpenStorageEncryptionNoResolver pins the ADR-0098 wiring hazard:
// encryption enabled with per_user_keys off must pass a NIL KeyResolver to
// the encrypt FS — a typed nil *SQLResolver would become a non-nil
// interface value and the FS would dispatch per-user allocation to a nil
// receiver on the first write.
func TestOpenStorageEncryptionNoResolver(t *testing.T) {
	cfg := DevConfig()
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Encryption.Enabled = true
	cfg.Encryption.MasterKeyPath = writeTestMasterKey(t)
	st, _, res, err := openStorage(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != nil {
		t.Fatalf("resolver = %v, want nil with per_user_keys off", res)
	}
	ctx := context.Background()
	wc, err := st.Create(ctx, "alice/a.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte("sealed but not per-user")); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	rc, err := st.Open(ctx, "alice/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "sealed but not per-user" {
		t.Fatalf("round trip = %q", body)
	}
}

// TestNewAppPerUserKeysWiresKeySharer pins the ADR-0098 app wiring: with
// per-user keys on, one KeySharer reaches the DAV write path, the sharing
// service, and the group-membership hook.
func TestNewAppPerUserKeysWiresKeySharer(t *testing.T) {
	ctx := context.Background()
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-keysharer-wiring?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Encryption.Enabled = true
	cfg.Encryption.MasterKeyPath = writeTestMasterKey(t)
	cfg.Encryption.PerUserKeys = true
	a, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })
	dav, ok := a.davFS.(*files.DAV)
	if !ok || dav.KeySharer == nil {
		t.Error("DAV.KeySharer not wired in per-user mode")
	}
	if a.shares.Keys == nil {
		t.Error("sharing service Keys not wired in per-user mode")
	}
	us, ok := a.Users.(*users.SQLStore)
	if !ok || us.MemberKeys == nil {
		t.Error("users MemberKeys hook not wired in per-user mode")
	}
	if dav.DirKeys != nil {
		t.Error("DAV.DirKeys wired without filename_encryption (ADR-0104 flag off)")
	}
	// ADR-0104 phase 2, flag off: no translating wrapper anywhere — the raw
	// stores keep the never-enable carve-out bit-identical.
	if _, translated := dav.Meta.(*files.TranslatingStore); translated {
		t.Error("DAV.Meta wrapped without filename_encryption (ADR-0104 flag off)")
	}
	if _, translated := a.fileMeta.(*files.TranslatingStore); translated {
		t.Error("app fileMeta wrapped without filename_encryption")
	}
	if _, translated := dav.Locks.(*files.TranslatingLockStore); translated {
		t.Error("DAV.Locks wrapped without filename_encryption")
	}
	if _, translated := a.trashFS.Sessions.(*files.TranslatingTrashStore); translated {
		t.Error("trash store wrapped without filename_encryption")
	}
	if _, translated := a.versionsFS.Meta.(*files.TranslatingVersionStore); translated {
		t.Error("versions store wrapped without filename_encryption")
	}
	// ADR-0104 phase 3a, flag off: zero share-metadata codec wiring — share
	// rows stay plaintext bit-identically.
	if a.shares.NameCodec != nil {
		t.Error("sharing NameCodec wired without filename_encryption")
	}
	if a.publicFS.NameCodec != nil {
		t.Error("public-link NameCodec wired without filename_encryption")
	}
	if dav.Names != nil || dav.RawMeta != nil || dav.ShareResealer != nil {
		t.Error("DAV phase-3a seams wired without filename_encryption")
	}
	// ADR-0104 phase 3b, flag off: upload sessions keep plaintext
	// destinations and the notifications render has no decrypt seam.
	if up, ok := a.uploadsFS.(*files.Uploads); ok {
		if _, wrapped := up.Sessions.(*files.TranslatingUploadStore); wrapped {
			t.Error("upload store wrapped without filename_encryption")
		}
	} else {
		t.Error("uploadsFS is not *files.Uploads")
	}
	if a.notifSubjects != nil {
		t.Error("notification subject decryptor wired without filename_encryption")
	}
	// ADR-0104 phase 4, flag off: no name sweep, so no login-conversion hook.
	if a.nameSweep != nil {
		t.Error("name sweep built without filename_encryption")
	}
}

// TestNewAppFilenameEncryptionWiresDirKeys pins the ADR-0104 phase-1 app
// wiring: with filename encryption on (validation requires per-user keys),
// the DAV directory-key minter is the same resolver. Phase 2 adds the
// translating decorator: DAV.Meta, the app's fileMeta (search/quota
// handlers), and the lock/trash/version satellite stores all wrap the raw
// stores over one shared translation core, and the user-creation hook flips
// users.name_scheme (the write switch). Phase 3a moves the KeySharer's meta
// seam back to the RAW store (share rows carry ciphertext paths now — the
// translating wrapper would double-encrypt) and wires the share-metadata
// codec into the sharing service, the public-link jail, and the DAV
// rename re-seal hook. Phase 3b wraps the upload-session store (tokenized
// destinations) and wires the notifications subject decryptor.
func TestNewAppFilenameEncryptionWiresDirKeys(t *testing.T) {
	ctx := context.Background()
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-dirkeys-wiring?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	cfg.Encryption.Enabled = true
	cfg.Encryption.MasterKeyPath = writeTestMasterKey(t)
	cfg.Encryption.PerUserKeys = true
	cfg.Encryption.FilenameEncryption = true
	a, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })
	dav, ok := a.davFS.(*files.DAV)
	if !ok || dav.DirKeys == nil {
		t.Fatal("DAV.DirKeys not wired with filename_encryption on")
	}
	if dav.DirKeys != a.keyResolver {
		t.Error("DAV.DirKeys must be the app's SQLResolver (the DK wrap rows live in file_keys)")
	}

	// Phase 2: the translating decorator reaches every files-store consumer.
	tMeta, ok := dav.Meta.(*files.TranslatingStore)
	if !ok {
		t.Fatal("DAV.Meta is not the translating store with filename_encryption on")
	}
	if a.fileMeta != dav.Meta {
		t.Error("app fileMeta (search/quota) must be the same translating store as DAV.Meta")
	}
	if dav.KeySharer == nil {
		t.Fatal("DAV.KeySharer not wired in per-user mode")
	}
	// Phase 3a: share paths are ciphertext, so the KeySharer meta seam is the
	// RAW store (the translating wrapper would double-encrypt).
	ksMeta, ok := dav.KeySharer.Meta.(*files.SQLStore)
	if !ok || ksMeta != tMeta.Raw() {
		t.Error("KeySharer.Meta must be the raw filecache store (ciphertext share paths, no double-encryption)")
	}
	// Phase 3a seams: the share-metadata codec and the rename re-seal hook.
	if a.shares.NameCodec == nil {
		t.Error("sharing service NameCodec not wired with filename_encryption on")
	}
	if a.publicFS.NameCodec == nil {
		t.Error("public-link jail NameCodec not wired with filename_encryption on")
	}
	if dav.Names == nil || dav.RawMeta == nil {
		t.Error("DAV ciphertext-mount seams (Names/RawMeta) not wired")
	}
	if dav.ShareResealer == nil {
		t.Error("DAV share-meta reseal hook not wired")
	}
	if _, ok := dav.Locks.(*files.TranslatingLockStore); !ok {
		t.Error("DAV.Locks is not the translating wrapper")
	}
	if _, ok := a.trashFS.Sessions.(*files.TranslatingTrashStore); !ok {
		t.Error("trash store is not the translating wrapper")
	}
	if _, ok := a.versionsFS.Meta.(*files.TranslatingVersionStore); !ok {
		t.Error("versions store is not the translating wrapper")
	}
	// Phase 3b: the upload-session store translates destinations, and the
	// notifications render seam decrypts §9 subject tokens in the viewer ctx.
	up, ok := a.uploadsFS.(*files.Uploads)
	if !ok {
		t.Fatal("uploadsFS is not *files.Uploads")
	}
	uploadStore, ok := up.Sessions.(*files.TranslatingUploadStore)
	if !ok {
		t.Fatal("upload store is not the translating wrapper")
	}
	if uploadStore.Raw() == nil {
		t.Error("translating upload store lost its raw store")
	}
	if a.notifSubjects == nil {
		t.Error("notification subject decryptor not wired with filename_encryption on")
	}
	// Phase 4: the per-user name sweep exists so the login-conversion hook
	// (enrolled users, bootstrap admin) can close over it.
	if a.nameSweep == nil {
		t.Error("name sweep not built with filename_encryption on")
	}
	// One shared core: the satellite wrappers and the files decorator hold
	// the same translator (per-call caches only — no cross-request key
	// caching, see NameTranslator).
	if tMeta.Raw() == nil {
		t.Error("translating store lost its raw store")
	}

	// The write switch: a user created while the mode is on starts at
	// users.name_scheme = 1 (the bootstrap admin, created before the wiring,
	// stays scheme 0 until the phase-4 sweep — ADR-0104 §11).
	u := &users.User{UID: "fresh", DisplayName: "Fresh", PasswordHash: "x", Enabled: true}
	if err := a.Users.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	scheme, err := a.Users.(*users.SQLStore).UserNameScheme(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if scheme != 1 {
		t.Errorf("fresh user name_scheme = %d, want 1 (server creation hook)", scheme)
	}
	admin, err := a.Users.GetByUID(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if scheme, err := a.Users.(*users.SQLStore).UserNameScheme(ctx, admin.ID); err != nil || scheme != 0 {
		t.Errorf("bootstrap admin name_scheme = %d %v, want 0 (pre-wiring creation)", scheme, err)
	}
}

// TestNewAppPerUserKeysPreviewAndAppdata is the ADR-0105 end-to-end pin:
// with per-user keys on, (1) the preview generator carries the self-sealing
// seams, (2) a preview request on a v3 file works through the HTTP layer and
// lands an NCGOPV1 blob under previews_enc on the raw backend, and (3) an
// appdata (plugin-storage-shaped) write succeeds — pre-ADR-0105 it failed
// loudly with "user not found".
func TestNewAppPerUserKeysPreviewAndAppdata(t *testing.T) {
	ctx := context.Background()
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-appdata-e2e?mode=memory&cache=shared"
	root := t.TempDir()
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: root},
	}
	cfg.Encryption.Enabled = true
	cfg.Encryption.MasterKeyPath = writeTestMasterKey(t)
	cfg.Encryption.PerUserKeys = true
	cfg.Previews.Enabled = true
	a, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	if a.previewGen == nil {
		t.Fatal("preview generator not built")
	}
	if a.previewGen.CacheRaw == nil || a.previewGen.SourceKeys == nil || a.previewGen.Keys == nil {
		t.Error("ADR-0105 self-sealing seams not wired in per-user mode")
	}

	// (3) appdata write + read round trip through the decorated storage.
	dav, ok := a.davFS.(*files.DAV)
	if !ok {
		t.Fatal("davFS is not *files.DAV")
	}
	wc, err := dav.Storage.Create(ctx, "appdata_"+a.instanceID+"/plugins/p1/state.bin", 0)
	if err != nil {
		t.Fatalf("appdata create: %v", err)
	}
	if _, err := wc.Write([]byte("plugin state")); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("appdata write: %v", err)
	}
	rc, err := dav.Storage.Open(ctx, "appdata_"+a.instanceID+"/plugins/p1/state.bin")
	if err != nil {
		t.Fatalf("appdata open: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "plugin state" {
		t.Fatalf("appdata round trip = %q, %v", body, err)
	}

	// (2) upload a PNG and request its preview, both through the HTTP layer.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	put := httptest.NewRequestWithContext(ctx, http.MethodPut, "/remote.php/dav/files/admin/photo.png", &pngBuf)
	put.SetBasicAuth("admin", "admin")
	putRec := httptest.NewRecorder()
	a.Router.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusCreated && putRec.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body %q", putRec.Code, putRec.Body.String())
	}

	get := httptest.NewRequestWithContext(ctx, http.MethodGet, "/index.php/core/preview?file=/photo.png&x=32&y=32", nil)
	get.SetBasicAuth("admin", "admin")
	getRec := httptest.NewRecorder()
	a.Router.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, body %q", getRec.Code, getRec.Body.String())
	}
	if ct := getRec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("preview Content-Type = %q", ct)
	}

	// The sealed blob lives under previews_enc on the raw backend and names
	// the source file's key UUID.
	encDir := filepath.Join(root, "appdata_"+a.instanceID, "previews_enc")
	entries, err := os.ReadDir(encDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("previews_enc entries = %v, %v", len(entries), err)
	}
	blob, err := os.ReadFile(filepath.Join(encDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(blob), "NCGOPV1") {
		t.Error("previews_enc blob lacks the NCGOPV1 magic")
	}
	adminRow, err := a.fileMeta.GetByPath(ctx, 1, "/photo.png")
	if err != nil {
		t.Fatal(err)
	}
	if len(adminRow.KeyUUID) != 16 || !bytes.Contains(blob, adminRow.KeyUUID) {
		t.Error("previews_enc blob does not name the source file's key UUID")
	}
	if _, err := os.ReadDir(filepath.Join(root, "appdata_"+a.instanceID, "previews")); !os.IsNotExist(err) {
		t.Errorf("v3-source preview leaked into the decorated previews/ prefix (err = %v)", err)
	}
}
