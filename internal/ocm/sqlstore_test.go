package ocm

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
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

func TestSQLStore_CRUDAndIsolation(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	in := &Incoming{
		UserID: alice.ID, UserUID: "alice", Name: "hello-remote.txt",
		Remote: "https://remote.example.com", RemoteID: "42", Owner: "alice@https://remote.example.com",
		Token: "ocmtok001", ItemType: "file",
	}
	if err := store.Insert(ctx, in); err != nil || in.ID == 0 {
		t.Fatalf("insert = %+v %v", in, err)
	}
	if err := store.Insert(ctx, &Incoming{
		UserID: alice.ID, UserUID: "alice", Name: "other.txt",
		Remote: "https://remote.example.com", RemoteID: "42", Owner: "alice@https://remote.example.com",
		Token: "x", ItemType: "file",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup = %v", err)
	}
	if _, err := store.Get(ctx, bob.ID, in.ID); err == nil {
		t.Fatal("bob must not see alice share")
	}
	listed, err := store.List(ctx, alice.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}
	mounts, err := store.ListIncoming(ctx, "alice")
	if err != nil || len(mounts) != 1 || !mounts[0].Remote || mounts[0].Mount != "/hello-remote.txt" {
		t.Fatalf("incoming = %+v %v", mounts, err)
	}
	if err := store.Delete(ctx, alice.ID, in.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryAndIncomingPOST(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	admin := &users.User{UID: "admin", DisplayName: "admin", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, admin); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	store.Clock = func() time.Time { return time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC) }

	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/.well-known/ocm", nil)
	req.Host = "cloud.example.com"
	rr := httptest.NewRecorder()
	Discovery(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"endPoint":"http://cloud.example.com/ocm"`) {
		t.Fatalf("discovery = %d %s", rr.Code, rr.Body.String())
	}

	body := `{"shareWith":"admin","name":"hello-remote.txt","providerId":"42","owner":"alice@https://remote.example.com","shareType":"user","resourceType":"file","protocol":{"name":"webdav","options":{"sharedSecret":"ocmtok001"}}}`
	post := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/shares", strings.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	IncomingHandler{Store: store, Users: us}.ServeHTTP(rr2, post)
	if rr2.Code != http.StatusCreated {
		t.Fatalf("incoming = %d %s", rr2.Code, rr2.Body.String())
	}

	unknown := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/shares", strings.NewReader(strings.ReplaceAll(body, `"admin"`, `"nobody"`)))
	rr3 := httptest.NewRecorder()
	IncomingHandler{Store: store, Users: us}.ServeHTTP(rr3, unknown)
	if rr3.Code != http.StatusBadRequest {
		t.Fatalf("unknown = %d", rr3.Code)
	}

	h := RemoteSharesHandler{Store: store, Users: us, Version: ocs.V2}
	list := httptest.NewRequestWithContext(ctx, http.MethodGet, ocsRemotePrefixV2+"?format=json", nil)
	list = list.WithContext(auth.WithUser(list.Context(), &auth.Principal{UID: "admin", Enabled: true}))
	rr4 := httptest.NewRecorder()
	h.ServeHTTP(rr4, list)
	if rr4.Code != http.StatusOK || !strings.Contains(rr4.Body.String(), `"hello-remote.txt"`) {
		t.Fatalf("remote shares = %d %s", rr4.Code, rr4.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(rr4.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestLocalUIDAndName(t *testing.T) {
	if got := localUID("admin"); got != "admin" {
		t.Fatalf("plain = %q", got)
	}
	if got := localUID("admin@https://cloud.example.com"); got != "admin" {
		t.Fatalf("cloudid = %q", got)
	}
	if remoteFromOwner("alice@https://remote.example.com") != "https://remote.example.com" {
		t.Fatal("remote")
	}
	if validShareName("a/b") || validShareName("") || validShareName("..") {
		t.Fatal("bad names accepted")
	}
}
