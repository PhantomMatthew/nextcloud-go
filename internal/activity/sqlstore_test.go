package activity

import (
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

func TestSQLStore_ListSinceAndSort(t *testing.T) {
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
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := store.Insert(ctx, &Event{
			UserID: alice.ID, ActorUID: "alice", App: "files", Type: "file_created",
			Subject: "admin created " + name, ObjectType: "files", ObjectName: "/" + name,
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.List(ctx, alice.ID, 0, 50, "desc")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %v %v", all, err)
	}
	if all[0].ID < all[1].ID {
		t.Fatalf("want desc, got %d then %d", all[0].ID, all[1].ID)
	}
	page, err := store.List(ctx, alice.ID, all[0].ID, 50, "desc")
	if err != nil || len(page) != 1 || page[0].ID != all[1].ID {
		t.Fatalf("since = %v %v", page, err)
	}
	if _, err := store.List(ctx, alice.ID, 0, 10, "sideways"); err == nil {
		t.Fatal("bad sort")
	}
}

func TestHandler_ListAndUnknownFilter(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	if err := store.Insert(ctx, &Event{
		UserID: alice.ID, ActorUID: "alice", App: "files", Type: "file_created",
		Subject: "admin created hello.txt", SubjectRich: "{user} created {file}",
		SubjectRichParameters: `{"user":{"type":"user","id":"alice","name":"Alice"},"file":{"type":"file","id":"2","name":"hello.txt","path":"/hello.txt"}}`,
		ObjectType:            "files", ObjectID: 2, ObjectName: "/hello.txt",
	}); err != nil {
		t.Fatal(err)
	}
	h := Handler{Store: store, Users: us, Version: ocs.V2}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"?format=json", nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Activity-Last-Given") == "" {
		t.Fatal("missing last-given")
	}
	if !strings.Contains(rr.Body.String(), `"activity_id":`) {
		t.Fatalf("body = %s", rr.Body.String())
	}

	filt := httptest.NewRequestWithContext(ctx, http.MethodGet, ocsPrefixV2+"/files?format=json", nil)
	filt = filt.WithContext(auth.WithUser(filt.Context(), &auth.Principal{UID: "alice", Enabled: true}))
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, filt)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("filter = %d", rr2.Code)
	}
}
