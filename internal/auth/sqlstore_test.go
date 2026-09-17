package auth

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
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(ctx, `INSERT INTO users (uid, display_name, password_hash, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		"alice", "Alice", "x", now, now); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLStoreAppPasswords(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	tok := &Token{
		ID:        "tok1",
		Hash:      "hash-abc",
		UID:       "alice",
		LoginName: "alice",
		Name:      "desktop",
		Type:      TokenTypePermanent,
		CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	if err := store.Insert(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByHash(ctx, tok.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.UID != "alice" || got.Name != "desktop" || got.ID != "tok1" {
		t.Errorf("got %+v", got)
	}
	if _, err := store.GetByHash(ctx, "missing"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := store.DeleteByHash(ctx, tok.Hash); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteByHash(ctx, tok.Hash); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("second delete = %v", err)
	}
}
