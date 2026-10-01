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

func TestSQLStoreDeleteAndExpiredTokens(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	for _, tok := range []*Token{
		{Token: "tok-old-1", UID: "alice", FileID: 1, ExpiresAt: now.Add(-time.Minute)},
		{Token: "tok-old-2", UID: "bob", FileID: 2, ExpiresAt: now}, // expiry AT now is expired (<=)
		{Token: "tok-live", UID: "alice", FileID: 3, CanWrite: true, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := store.Insert(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	// ExpiredTokens names exactly the expired rows (the GC reads it to reap
	// the matching key wraps first, ADR-0107).
	expired, err := store.ExpiredTokens(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 2 {
		t.Fatalf("ExpiredTokens = %v, want 2 rows", expired)
	}
	seen := map[string]bool{expired[0]: true, expired[1]: true}
	if !seen["tok-old-1"] || !seen["tok-old-2"] {
		t.Errorf("ExpiredTokens = %v, want [tok-old-1 tok-old-2]", expired)
	}

	// Delete removes exactly one row; a missing row is a no-op (the mint
	// rollback and the GC both rely on it being safe).
	if err := store.Delete(ctx, "tok-old-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByToken(ctx, "tok-old-1", now.Add(-2*time.Minute)); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("deleted token = %v, want ErrTokenNotFound", err)
	}
	if err := store.Delete(ctx, "tok-old-1"); err != nil {
		t.Errorf("second Delete = %v, want no-op nil", err)
	}
	if err := store.Delete(ctx, "never-existed"); err != nil {
		t.Errorf("Delete of a missing token = %v, want no-op nil", err)
	}
	if _, err := store.GetByToken(ctx, "tok-live", now); err != nil {
		t.Errorf("Delete took the live token: %v", err)
	}
	expired, err = store.ExpiredTokens(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0] != "tok-old-2" {
		t.Errorf("ExpiredTokens after Delete = %v, want [tok-old-2]", expired)
	}
}
