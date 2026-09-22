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
