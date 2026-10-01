package mail

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
)

// testDB mirrors internal/wopi/store_test.go:14-33: in-memory sqlite with
// the real migration chain (0027 included).
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

func testAccount(uid string) *Account {
	return &Account{
		UserID:         uid,
		Name:           "Work",
		Email:          uid + "@example.com",
		IMAPHost:       "imap.example.com",
		IMAPPort:       993,
		IMAPSSLMode:    SSLModeSSL,
		IMAPUser:       uid + "@example.com",
		SMTPHost:       "smtp.example.com",
		SMTPPort:       465,
		SMTPSSLMode:    SSLModeSSL,
		SMTPUser:       uid + "@example.com",
		PasswordSealed: []byte("sealed-blob"),
	}
}

func TestSQLStoreCRUD(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	freeze := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }

	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.ID <= 0 {
		t.Fatalf("id = %d after create", a.ID)
	}
	if !a.CreatedAt.Equal(freeze) || !a.UpdatedAt.Equal(freeze) {
		t.Errorf("timestamps = %v %v", a.CreatedAt, a.UpdatedAt)
	}

	got, err := store.GetByID(ctx, "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Work" || got.Email != "alice@example.com" || got.IMAPPort != 993 ||
		got.IMAPSSLMode != SSLModeSSL || got.SMTPPort != 465 || string(got.PasswordSealed) != "sealed-blob" {
		t.Errorf("got = %+v", got)
	}

	// A second account lists in id order; the missing user lists empty.
	b := testAccount("alice")
	b.Name = "Home"
	if err := store.Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListByUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("list = %+v", list)
	}
	empty, err := store.ListByUser(ctx, "nobody")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %+v %v", empty, err)
	}

	// Update rewrites fields and moves updated_at.
	later := freeze.Add(time.Hour)
	store.Clock = func() time.Time { return later }
	got.Name = "Renamed"
	got.IMAPPort = 143
	got.IMAPSSLMode = SSLModeStartTLS
	got.PasswordSealed = []byte("resealed")
	if err := store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetByID(ctx, "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Renamed" || updated.IMAPPort != 143 || updated.IMAPSSLMode != SSLModeStartTLS ||
		string(updated.PasswordSealed) != "resealed" || !updated.UpdatedAt.Equal(later) || !updated.CreatedAt.Equal(freeze) {
		t.Errorf("updated = %+v", updated)
	}

	if err := store.Delete(ctx, "alice", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, "alice", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: err = %v", err)
	}
	if err := store.Delete(ctx, "alice", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
	if err := store.Update(ctx, got); !errors.Is(err, ErrNotFound) {
		t.Errorf("update after delete: err = %v", err)
	}
}

func TestSQLStoreUserScoping(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)

	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	// Bob cannot see, list, update, or delete Alice's row — every path is
	// the same undifferentiated not-found.
	if _, err := store.GetByID(ctx, "bob", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user get: err = %v", err)
	}
	list, err := store.ListByUser(ctx, "bob")
	if err != nil || len(list) != 0 {
		t.Fatalf("cross-user list = %+v %v", list, err)
	}
	evil := testAccount("bob")
	evil.ID = a.ID
	if err := store.Update(ctx, evil); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user update: err = %v", err)
	}
	if err := store.Delete(ctx, "bob", a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user delete: err = %v", err)
	}
	// Alice's row is untouched.
	got, err := store.GetByID(ctx, "alice", a.ID)
	if err != nil || got.Name != "Work" {
		t.Fatalf("row after cross-user attempts = %+v %v", got, err)
	}
}

func TestSQLStoreDefaultsAndInvalid(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)

	// Empty ssl modes default to 'ssl' (the column defaults).
	a := testAccount("alice")
	a.IMAPSSLMode, a.SMTPSSLMode = "", ""
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if a.IMAPSSLMode != SSLModeSSL || a.SMTPSSLMode != SSLModeSSL {
		t.Errorf("ssl mode defaults = %q %q", a.IMAPSSLMode, a.SMTPSSLMode)
	}

	if err := store.Create(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil create: err = %v", err)
	}
	noSeal := testAccount("alice")
	noSeal.PasswordSealed = nil
	if err := store.Create(ctx, noSeal); !errors.Is(err, ErrInvalid) {
		t.Errorf("create without sealed password: err = %v", err)
	}
	noUser := testAccount("")
	if err := store.Create(ctx, noUser); !errors.Is(err, ErrInvalid) {
		t.Errorf("create without user id: err = %v", err)
	}
	if err := store.Update(ctx, &Account{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("update zero account: err = %v", err)
	}
}
