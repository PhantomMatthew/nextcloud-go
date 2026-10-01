package wopi

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
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

func TestSQLStoreTokens(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	live := &Token{Token: "tok-live", UID: "alice", FileID: 42, CanWrite: true, ExpiresAt: now.Add(time.Hour)}
	if err := store.Insert(ctx, live); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByToken(ctx, "tok-live", now)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "alice" || got.FileID != 42 || !got.CanWrite || !got.ExpiresAt.Equal(live.ExpiresAt) {
		t.Fatalf("got = %+v", got)
	}

	// Read-only grant round-trips as false.
	if err := store.Insert(ctx, &Token{Token: "tok-ro", UID: "bob", FileID: 43, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ro, err := store.GetByToken(ctx, "tok-ro", now)
	if err != nil || ro.CanWrite {
		t.Fatalf("read-only token = %+v %v", ro, err)
	}

	// Missing and expired both yield the not-found sentinel.
	if _, err := store.GetByToken(ctx, "tok-missing", now); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing = %v", err)
	}
	expired := &Token{Token: "tok-old", UID: "alice", FileID: 44, CanWrite: true, ExpiresAt: now.Add(-time.Minute)}
	if err := store.Insert(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByToken(ctx, "tok-old", now); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("expired = %v, want ErrTokenNotFound", err)
	}

	// GC deletes only expired rows.
	n, err := store.DeleteExpired(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpired = %d %v, want 1", n, err)
	}
	if _, err := store.GetByToken(ctx, "tok-live", now); err != nil {
		t.Fatalf("live token deleted: %v", err)
	}
	if n, err := store.DeleteExpired(ctx, now); err != nil || n != 0 {
		t.Fatalf("second DeleteExpired = %d %v, want 0", n, err)
	}
}
