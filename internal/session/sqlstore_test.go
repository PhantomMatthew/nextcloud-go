package session

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

func insertUser(t *testing.T, db database.DB, uid string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(ctx, `INSERT INTO users (uid, display_name, password_hash, enabled, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)`,
		uid, uid, "x", now, now); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = ?`, uid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSQLStoreSessions(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	userID := insertUser(t, db, "alice")
	store := NewSQLStore(db)
	now := time.Now().UTC()
	sess, err := store.Create(ctx, userID, "ua", "127.0.0.1", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.ID) != 64 {
		t.Fatalf("id len = %d", len(sess.ID))
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != userID || got.IP != "127.0.0.1" {
		t.Fatalf("got %+v", got)
	}
	if err := store.Touch(ctx, sess.ID, now.Add(time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func TestSQLStoreExpired(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	userID := insertUser(t, db, "bob")
	store := NewSQLStore(db)
	past := time.Now().UTC().Add(-2 * time.Hour)
	sess, err := store.Create(ctx, userID, "", "", time.Hour, past)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, sess.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired = %v", err)
	}
}
