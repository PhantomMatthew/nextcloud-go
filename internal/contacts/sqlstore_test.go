package contacts

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
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

func seedUser(t *testing.T, db database.DB) int64 {
	t.Helper()
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := users.NewSQLStore(db).Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

const sampleVCard = "BEGIN:VCARD\nVERSION:3.0\nUID:ncgo-contact-001\nFN:Ada Lovelace\nN:Lovelace;Ada;;;\nEMAIL:ada@example.com\nEND:VCARD\n"

func TestSQLStore_HomeAndObject(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	uid := seedUser(t, db)

	if err := store.EnsureHome(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHome(ctx, uid); err != nil {
		t.Fatal(err)
	}
	books, err := store.ListBooks(ctx, uid)
	if err != nil || len(books) != 1 || books[0].URI != DefaultBookURI {
		t.Fatalf("books=%v err=%v", books, err)
	}
	if books[0].DisplayName != DefaultDisplayName {
		t.Errorf("displayname = %q", books[0].DisplayName)
	}

	obj := &Object{URI: "ncgo-contact-001.vcf", Data: []byte(sampleVCard)}
	created, err := store.PutObject(ctx, uid, DefaultBookURI, obj)
	if err != nil || !created || obj.ETag == "" {
		t.Fatalf("put = created=%v %+v %v", created, obj, err)
	}
	got, err := store.GetObject(ctx, uid, DefaultBookURI, obj.URI)
	if err != nil || got.UID != "ncgo-contact-001" || got.FN != "Ada Lovelace" {
		t.Fatalf("get = %+v %v", got, err)
	}

	dup := &Object{URI: "other.vcf", Data: []byte(sampleVCard)}
	if _, err := store.PutObject(ctx, uid, DefaultBookURI, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("uid conflict = %v", err)
	}

	created, err = store.PutObject(ctx, uid, DefaultBookURI, obj)
	if err != nil || created {
		t.Fatalf("update = created=%v %v", created, err)
	}

	if err := store.DeleteObject(ctx, uid, DefaultBookURI, obj.URI); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetObject(ctx, uid, DefaultBookURI, obj.URI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted = %v", err)
	}
}
