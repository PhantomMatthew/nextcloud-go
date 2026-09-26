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

// stubTokenKeysHook records OnTokenDeleted calls (ADR-0102).
type stubTokenKeysHook struct {
	ids []string
	err error
}

func (s *stubTokenKeysHook) OnTokenDeleted(_ context.Context, appPasswordID string) error {
	s.ids = append(s.ids, appPasswordID)
	return s.err
}

// TestSQLStoreDeleteByHashTokenKeysHook pins the ADR-0102 revocation hook:
// DeleteByHash fires it with the deleted row's id; a hook error never fails
// the delete; a missing token stays ErrTokenNotFound and fires nothing.
func TestSQLStoreDeleteByHashTokenKeysHook(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	hook := &stubTokenKeysHook{}
	store.TokenKeys = hook
	insert := func(id, hash string) {
		t.Helper()
		if err := store.Insert(ctx, &Token{
			ID: id, Hash: hash, UID: "alice", LoginName: "alice",
			Type: TokenTypePermanent, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The hook fires with the row's id after the delete.
	insert("tok-hook", "hash-hook")
	if err := store.DeleteByHash(ctx, "hash-hook"); err != nil {
		t.Fatal(err)
	}
	if len(hook.ids) != 1 || hook.ids[0] != "tok-hook" {
		t.Fatalf("hook ids = %v, want [tok-hook]", hook.ids)
	}
	if _, err := store.GetByHash(ctx, "hash-hook"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("deleted token still readable: %v", err)
	}

	// A hook error does not fail the delete (best-effort; reconcile's orphan
	// purge is the safety net).
	hook.err = errors.New("resolver gone")
	insert("tok-hook-2", "hash-hook-2")
	if err := store.DeleteByHash(ctx, "hash-hook-2"); err != nil {
		t.Errorf("delete with a failing hook = %v, want nil (best-effort hook)", err)
	}
	if len(hook.ids) != 2 || hook.ids[1] != "tok-hook-2" {
		t.Errorf("hook ids = %v, want [tok-hook tok-hook-2]", hook.ids)
	}
	if _, err := store.GetByHash(ctx, "hash-hook-2"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("token surviving a failed hook: %v", err)
	}

	// ErrTokenNotFound is unchanged, and the hook never fires for it.
	if err := store.DeleteByHash(ctx, "missing"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("missing delete = %v, want ErrTokenNotFound", err)
	}
	if len(hook.ids) != 2 {
		t.Errorf("hook fired for a missing token: %v", hook.ids)
	}
}
