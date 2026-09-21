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
	if mounts[0].RemoteOrigin != "https://remote.example.com" || mounts[0].RemoteToken != "ocmtok001" {
		t.Fatalf("incoming creds = %+v", mounts[0])
	}
	if err := store.Delete(ctx, alice.ID, in.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSQLStore_DeleteByRemoteIDAndToken(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	in := &Incoming{
		UserID: alice.ID, UserUID: "alice", Name: "hello-remote.txt",
		Remote: "https://remote.example.com", RemoteID: "42", Owner: "alice@https://remote.example.com",
		Token: "ocmtok001", ItemType: "file",
	}
	if err := store.Insert(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteByRemoteIDAndToken(ctx, "42", "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token = %v", err)
	}
	if err := store.DeleteByRemoteIDAndToken(ctx, "99", "ocmtok001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong id = %v", err)
	}
	if err := store.DeleteByRemoteIDAndToken(ctx, "42", "ocmtok001"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, alice.ID, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
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
	writeBody := `{"shareWith":"admin","name":"remote-dir","providerId":"43","owner":"alice@https://remote.example.com","shareType":"user","resourceType":"folder","protocol":{"name":"webdav","options":{"sharedSecret":"ocmtok002","permissions":15}}}`
	postW := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/shares", strings.NewReader(writeBody))
	postW.Header.Set("Content-Type", "application/json")
	rrW := httptest.NewRecorder()
	IncomingHandler{Store: store, Users: us}.ServeHTTP(rrW, postW)
	if rrW.Code != http.StatusCreated {
		t.Fatalf("folder incoming = %d %s", rrW.Code, rrW.Body.String())
	}
	mounts, err := store.ListIncoming(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	foundWrite := false
	for _, m := range mounts {
		if m.Mount == "/remote-dir" && m.ItemType == "folder" && m.Permissions == 15 {
			foundWrite = true
		}
	}
	if !foundWrite {
		t.Fatalf("mounts = %+v", mounts)
	}
	var parsed map[string]any
	if err := json.Unmarshal(rr4.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
}

func TestIncomingSHARE_UNSHARED(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	admin := &users.User{UID: "admin", DisplayName: "admin", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, admin); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	in := &Incoming{
		UserID: admin.ID, UserUID: "admin", Name: "hello-remote.txt",
		Remote: "https://remote.example.com", RemoteID: "42", Owner: "alice@https://remote.example.com",
		Token: "ocmtok001", ItemType: "file",
	}
	if err := store.Insert(ctx, in); err != nil {
		t.Fatal(err)
	}
	h := IncomingHandler{Store: store, Users: us}

	missing := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/notifications", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, missing)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing = %d %s", rr.Code, rr.Body.String())
	}

	unknownType := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/notifications", strings.NewReader(`{"notificationType":"SHARE_ACCEPTED","resourceType":"file","providerId":"42","notification":{"sharedSecret":"ocmtok001"}}`))
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, unknownType)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("unknown type = %d %s", rr2.Code, rr2.Body.String())
	}

	unknownShare := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/notifications", strings.NewReader(`{"notificationType":"SHARE_UNSHARED","resourceType":"file","providerId":"99","notification":{"sharedSecret":"ocmtok001"}}`))
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, unknownShare)
	if rr3.Code != http.StatusBadRequest {
		t.Fatalf("unknown share = %d %s", rr3.Code, rr3.Body.String())
	}

	ok := httptest.NewRequestWithContext(ctx, http.MethodPost, "/ocm/notifications", strings.NewReader(`{"notificationType":"SHARE_UNSHARED","resourceType":"file","providerId":"42","notification":{"sharedSecret":"ocmtok001"}}`))
	rr4 := httptest.NewRecorder()
	h.ServeHTTP(rr4, ok)
	if rr4.Code != http.StatusCreated || rr4.Body.String() != "[]" {
		t.Fatalf("unshare = %d %s", rr4.Code, rr4.Body.String())
	}
	if _, err := store.Get(ctx, admin.ID, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after unshare = %v", err)
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
