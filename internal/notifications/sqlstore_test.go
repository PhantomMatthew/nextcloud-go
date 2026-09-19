package notifications

import (
	"encoding/json"
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
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }

	n := &Notification{
		UserID:                alice.ID,
		App:                   "files_sharing",
		UserUID:               "alice",
		ObjectType:            "share",
		ObjectID:              "1",
		Subject:               "You received a share of hello.txt",
		SubjectRich:           "You received a share of {file}",
		SubjectRichParameters: `{"file":{"type":"file","id":"2","name":"hello.txt"}}`,
		ShouldNotify:          true,
	}
	if err := store.Insert(ctx, n); err != nil || n.ID == 0 {
		t.Fatalf("insert = %+v %v", n, err)
	}
	got, err := store.Get(ctx, alice.ID, n.ID)
	if err != nil || got.Subject != n.Subject {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := store.Get(ctx, bob.ID, n.ID); err == nil {
		t.Fatal("bob must not see alice notification")
	}
	listed, err := store.List(ctx, alice.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}
	if err := store.Delete(ctx, alice.ID, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, alice.ID, n.ID); err == nil {
		t.Fatal("deleted")
	}
}

func TestHandler_ListGetDeleteAndETag(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	n := &Notification{
		UserID: alice.ID, App: "files_sharing", UserUID: "alice",
		Subject: "You received a share of hello.txt", ShouldNotify: true,
	}
	if err := store.Insert(ctx, n); err != nil {
		t.Fatal(err)
	}
	h := Handler{Store: store, Users: us, Version: ocs.V2}

	req := withAlice(httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"?format=json", nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", rr.Code, rr.Body.String())
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing etag")
	}
	if !strings.Contains(rr.Body.String(), `"notification_id":`) {
		t.Fatalf("body = %s", rr.Body.String())
	}

	inm := withAlice(httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"?format=json", nil))
	inm.Header.Set("If-None-Match", etag)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, inm)
	if rr2.Code != http.StatusNotModified {
		t.Fatalf("etag status = %d", rr2.Code)
	}

	one := withAlice(httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"/1?format=json", nil))
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, one)
	if rr3.Code != http.StatusOK {
		t.Fatalf("get status = %d %s", rr3.Code, rr3.Body.String())
	}

	del := withAlice(httptest.NewRequestWithContext(ctx, http.MethodDelete, ocsPrefixV2+"/1?format=json", nil))
	rr4 := httptest.NewRecorder()
	h.ServeHTTP(rr4, del)
	if rr4.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rr4.Code)
	}

	gone := withAlice(httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"/1?format=json", nil))
	rr5 := httptest.NewRecorder()
	h.ServeHTTP(rr5, gone)
	if rr5.Code != http.StatusNotFound {
		t.Fatalf("gone status = %d", rr5.Code)
	}
}

func TestHandler_DeleteAllEmptyList(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	if err := store.Insert(ctx, &Notification{UserID: alice.ID, App: "files_sharing", UserUID: "alice", Subject: "x", ShouldNotify: true}); err != nil {
		t.Fatal(err)
	}
	h := Handler{Store: store, Users: us, Version: ocs.V2}
	del := withAlice(httptest.NewRequestWithContext(ctx, http.MethodDelete, ocsPrefixV2+"?format=json", nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, del)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete all = %d", rr.Code)
	}
	list := withAlice(httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"?format=json", nil))
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, list)
	if rr2.Code != http.StatusOK {
		t.Fatalf("list empty = %d %s", rr2.Code, rr2.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(rr2.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	ocsObj, _ := parsed["ocs"].(map[string]any)
	data, _ := ocsObj["data"].([]any)
	if len(data) != 0 {
		t.Fatalf("data = %#v", data)
	}
}

func withAlice(r *http.Request) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: "alice", Enabled: true}))
}
