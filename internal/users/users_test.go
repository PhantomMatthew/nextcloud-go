package users

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
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

func hasher() *auth.Argon2id {
	return auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
}

func TestSQLStoreUsers(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	hash, err := h.Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	u := &User{UID: "alice", DisplayName: "Alice", PasswordHash: hash, Enabled: true}
	if err := store.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 {
		t.Error("expected last insert id")
	}
	got, err := store.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Alice" || !got.Enabled {
		t.Errorf("%+v", got)
	}
	byID, err := store.GetByID(ctx, got.ID)
	if err != nil || byID.UID != "alice" {
		t.Fatalf("GetByID = %+v %v", byID, err)
	}
	if err := store.Create(ctx, u); !errors.Is(err, ErrExists) {
		t.Errorf("dup = %v", err)
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Errorf("count = %d %v", n, err)
	}
	newHash, err := h.Hash("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePasswordHash(ctx, got.ID, newHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByUID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestPasswordVerifier(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	hash, err := h.Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &User{UID: "alice", DisplayName: "Alice", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &User{UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	v := NewPasswordVerifier(store, h)
	if _, err := v.Verify(ctx, "", "secret"); !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("empty: %v", err)
	}
	p, err := v.Verify(ctx, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "alice" || p.DisplayName != "Alice" {
		t.Errorf("%+v", p)
	}
	if _, err := v.Verify(ctx, "alice", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong pw: %v", err)
	}
	if _, err := v.Verify(ctx, "nobody", "secret"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("missing: %v", err)
	}
	if _, err := v.Verify(ctx, "bob", "secret"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("disabled: %v", err)
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{}, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrNoUsers) {
		t.Errorf("empty bootstrap: %v", err)
	}
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{UID: "admin", Password: "admin", DisplayName: "Admin"}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("count = %d %v", n, err)
	}
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{UID: "other", Password: "x"}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	n, err = store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("second bootstrap count = %d %v", n, err)
	}
}
