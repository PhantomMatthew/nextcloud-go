package appconfig

import (
	"context"
	"errors"
	"log/slog"
	"testing"

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

func TestStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testDB(t))

	if _, err := s.Get(ctx, "core", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	if err := s.Set(ctx, "core", "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(ctx, "core", "theme")
	if err != nil || val != "dark" {
		t.Fatalf("val = %q, err = %v", val, err)
	}

	// Overwrite.
	if err := s.Set(ctx, "core", "theme", "light"); err != nil {
		t.Fatal(err)
	}
	val, err = s.Get(ctx, "core", "theme")
	if err != nil || val != "light" {
		t.Fatalf("val = %q, err = %v", val, err)
	}

	// Empty value round-trips.
	if err := s.Set(ctx, "core", "empty", ""); err != nil {
		t.Fatal(err)
	}
	val, err = s.Get(ctx, "core", "empty")
	if err != nil || val != "" {
		t.Fatalf("val = %q, err = %v", val, err)
	}

	// Delete; deleting again is not an error.
	if err := s.Delete(ctx, "core", "theme"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "core", "theme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := s.Delete(ctx, "core", "theme"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDeleteByPrefix(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testDB(t))

	seed := func(appid, key, val string) {
		t.Helper()
		if err := s.Set(ctx, appid, key, val); err != nil {
			t.Fatal(err)
		}
	}
	gone := func(appid, key string) {
		t.Helper()
		if _, err := s.Get(ctx, appid, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s/%s should be gone: %v", appid, key, err)
		}
	}
	kept := func(appid, key string) {
		t.Helper()
		if _, err := s.Get(ctx, appid, key); err != nil {
			t.Errorf("%s/%s should survive: %v", appid, key, err)
		}
	}

	seed("plugin", "com.example.a.x", "1")
	seed("plugin", "com.example.a.y", "2")
	seed("plugin", "com.example.a", "3")    // exact key, no trailing dot
	seed("plugin", "com.example.ab.z", "4") // boundary: no trailing dot
	seed("core", "com.example.a.x", "5")    // appid isolation
	seed("plugin", "other.k", "6")

	// The uninstall caller passes the plugin id plus a trailing dot, which
	// is what keeps "com.example.ab.*" safe from a "com.example.a" cleanup.
	if err := s.DeleteByPrefix(ctx, "plugin", "com.example.a."); err != nil {
		t.Fatal(err)
	}
	gone("plugin", "com.example.a.x")
	gone("plugin", "com.example.a.y")
	kept("plugin", "com.example.a")
	kept("plugin", "com.example.ab.z")
	kept("core", "com.example.a.x")
	kept("plugin", "other.k")

	// No matching rows is not an error.
	if err := s.DeleteByPrefix(ctx, "plugin", "com.example.a."); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDeleteByPrefixEscapesLike(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testDB(t))

	seed := func(key string) {
		t.Helper()
		if err := s.Set(ctx, "plugin", key, "v"); err != nil {
			t.Fatal(err)
		}
	}
	gone := func(key string) {
		t.Helper()
		if _, err := s.Get(ctx, "plugin", key); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q should be gone: %v", key, err)
		}
	}
	kept := func(key string) {
		t.Helper()
		if _, err := s.Get(ctx, "plugin", key); err != nil {
			t.Errorf("%q should survive: %v", key, err)
		}
	}

	// LIKE wildcards (% and _) and the escape character (!) in the prefix
	// must match literally, never as wildcards.
	seed("a%b_c1")  // literal prefix match
	seed("a%b_c99") // literal prefix match
	seed("axb_c1")  // % would match "x" unescaped
	seed("a%bXc1")  // _ would match "X" unescaped
	seed("a!b1")    // escape char in a later prefix
	seed("anb1")    // ! would match "n" unescaped

	if err := s.DeleteByPrefix(ctx, "plugin", "a%b_c"); err != nil {
		t.Fatal(err)
	}
	gone("a%b_c1")
	gone("a%b_c99")
	kept("axb_c1")
	kept("a%bXc1")

	if err := s.DeleteByPrefix(ctx, "plugin", "a!b"); err != nil {
		t.Fatal(err)
	}
	gone("a!b1")
	kept("anb1")
}

func TestStoreAppidIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewStore(testDB(t))

	if err := s.Set(ctx, "app-a", "key", "a-value"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "app-b", "key", "b-value"); err != nil {
		t.Fatal(err)
	}
	val, err := s.Get(ctx, "app-a", "key")
	if err != nil || val != "a-value" {
		t.Fatalf("val = %q, err = %v", val, err)
	}
	val, err = s.Get(ctx, "app-b", "key")
	if err != nil || val != "b-value" {
		t.Fatalf("val = %q, err = %v", val, err)
	}

	// Deleting one appid's row leaves the other untouched.
	if err := s.Delete(ctx, "app-a", "key"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "app-a", "key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	val, err = s.Get(ctx, "app-b", "key")
	if err != nil || val != "b-value" {
		t.Fatalf("val = %q, err = %v", val, err)
	}
}
