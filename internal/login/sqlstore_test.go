package login

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

func TestSQLStoreFlows(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := &Flow{
		PollToken:  "poll1",
		LoginToken: "login1",
		ClientName: "ua",
		State:      StatePending,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
	}
	if err := store.Insert(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByPoll(ctx, "poll1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LoginToken != "login1" || got.State != StatePending {
		t.Errorf("%+v", got)
	}
	got.State = StateGranted
	got.StateToken = "state1"
	got.Server = "https://x"
	got.LoginName = "alice"
	got.AppPassword = "pw"
	if err := store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	byState, err := store.GetByState(ctx, "state1")
	if err != nil {
		t.Fatal(err)
	}
	if byState.LoginName != "alice" {
		t.Errorf("%+v", byState)
	}
	byLogin, err := store.GetByLogin(ctx, "login1")
	if err != nil {
		t.Fatal(err)
	}
	if byLogin.Server != "https://x" {
		t.Errorf("%+v", byLogin)
	}

	expired := &Flow{
		PollToken:  "poll-old",
		LoginToken: "login-old",
		ClientName: "ua",
		CreatedAt:  now.Add(-2 * time.Hour),
		ExpiresAt:  now.Add(-time.Hour),
	}
	if err := store.Insert(ctx, expired); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteExpired(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted = %d", n)
	}
	if _, err := store.GetByPoll(ctx, "poll-old"); !errors.Is(err, ErrFlowNotFound) {
		t.Errorf("expired still present: %v", err)
	}
}
