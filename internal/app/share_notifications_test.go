package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// TestShareNotificationsOCS pins the ADR-0082 wiring end to end: a user share
// created through the OCS share endpoint lands in the sharee's OCS
// notifications list, and unsharing dismisses it.
func TestShareNotificationsOCS(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := DevConfig()
	cfg.Database.DSN = "file:ncgo-share-notif?mode=memory&cache=shared"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: t.TempDir()},
	}
	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(ctx) })

	hash, err := a.hasher.Hash("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Users.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := a.davFS.Write(ctx, "admin", "/hello.txt", strings.NewReader("hello world\n"), &mt); err != nil {
		t.Fatal(err)
	}

	do := func(method, path, uid, pass, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		} else {
			rdr = strings.NewReader("")
		}
		req := httptest.NewRequestWithContext(ctx, method, path, rdr)
		req.SetBasicAuth(uid, pass)
		req.Header.Set("OCS-APIRequest", "true")
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr
	}
	listNotifs := func(uid string) []any {
		t.Helper()
		rr := do(http.MethodGet, "/ocs/v2.php/apps/notifications/api/v2/notifications?format=json", uid, uid, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s notifications status = %d body=%s", uid, rr.Code, rr.Body.String())
		}
		var env map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		data, ok := env["ocs"].(map[string]any)["data"].([]any)
		if !ok {
			t.Fatalf("data shape = %s", rr.Body.String())
		}
		return data
	}

	rr := do(http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", "admin", "admin", "path=/hello.txt&shareType=0&shareWith=bob")
	if rr.Code != http.StatusOK {
		t.Fatalf("create share status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	shareID, ok := env["ocs"].(map[string]any)["data"].(map[string]any)["id"].(string)
	if !ok || shareID == "" {
		t.Fatalf("share id = %s", rr.Body.String())
	}

	notifs := listNotifs("bob")
	if len(notifs) != 1 {
		t.Fatalf("bob notifications = %v", notifs)
	}
	n, _ := notifs[0].(map[string]any)
	if n["app"] != "files_sharing" || n["object_type"] != "share" || n["object_id"] != "ocinternal:"+shareID {
		t.Errorf("notification = %v", n)
	}
	if n["subject"] != "You received /hello.txt as a share by admin" {
		t.Errorf("subject = %v", n["subject"])
	}
	if n["shouldNotify"] != true {
		t.Errorf("shouldNotify = %v", n["shouldNotify"])
	}

	rr = do(http.MethodDelete, "/ocs/v2.php/apps/files_sharing/api/v1/shares/"+shareID+"?format=json", "admin", "admin", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("delete share status = %d body=%s", rr.Code, rr.Body.String())
	}
	if notifs := listNotifs("bob"); len(notifs) != 0 {
		t.Fatalf("bob notifications after unshare = %v", notifs)
	}
}
