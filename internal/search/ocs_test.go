package search

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
	ctx := t.Context()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func testHandler(t *testing.T) (Handler, *files.File) {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	meta := files.NewSQLStore(db)
	if _, err := meta.EnsureRoot(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.EnsureRoot(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	hello := &files.File{UserID: alice.ID, Path: "/hello.txt", Size: 12, MIME: "text/plain", Permissions: 27}
	if err := meta.Insert(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := meta.Insert(ctx, &files.File{UserID: bob.ID, Path: "/hello.txt", Size: 1, MIME: "text/plain", Permissions: 27}); err != nil {
		t.Fatal(err)
	}
	h := Handler{
		Providers: []Provider{NewFilesProvider(meta, us)},
		Version:   ocs.V2,
	}
	return h, hello
}

func withUser(r *http.Request, uid string) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: uid, DisplayName: uid, Enabled: true}))
}

func TestProvidersListAndSearch(t *testing.T) {
	h, hello := testHandler(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers?format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"id":"files"`) || !strings.Contains(rr.Body.String(), `"name":"Files"`) {
		t.Fatalf("list body = %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers/files/search?format=json&term=hello", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("search status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	ocsObj, _ := env["ocs"].(map[string]any)
	data, _ := ocsObj["data"].(map[string]any)
	entries, _ := data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", data["entries"])
	}
	entry, _ := entries[0].(map[string]any)
	wantURL := "https://cloud.example.com/index.php/f/" + strconv.FormatInt(hello.ID, 10)
	if entry["title"] != "hello.txt" || entry["subline"] != "/hello.txt" || entry["resourceUrl"] != wantURL {
		t.Fatalf("entry = %#v want url %s", entry, wantURL)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers/files/search?format=json&term=", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusOK {
		t.Fatalf("empty term status = %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), `"entries":[]`) {
		t.Fatalf("empty term body = %s", body)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers/files/search?format=json&term=hello", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req, "bob"))
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	ocsObj, _ = env["ocs"].(map[string]any)
	data, _ = ocsObj["data"].(map[string]any)
	entries, _ = data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("bob entries = %#v", data["entries"])
	}
	entry, _ = entries[0].(map[string]any)
	if entry["resourceUrl"] == wantURL {
		t.Fatal("bob saw alice file id")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/search/providers/mail/search?format=json&term=hello", nil)
	h.ServeHTTP(rr, withUser(req, "alice"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown provider = %d body=%s", rr.Code, rr.Body.String())
	}
}
