package jobs

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
	ctx := t.Context()
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

func TestSQLStoreInsertClaimCompleteFail(t *testing.T) {
	ctx := t.Context()
	store := NewSQLStore(testDB(t))
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, &Row{Name: "shares.expire", RunAt: now.UnixMilli(), CreatedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDue(ctx, now.Add(-time.Hour), 8)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("early claim = %v %v", claimed, err)
	}
	claimed, err = store.ClaimDue(ctx, now, 8)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %v %v", claimed, err)
	}
	again, err := store.ClaimDue(ctx, now, 8)
	if err != nil || len(again) != 0 {
		t.Fatalf("second claim = %v %v", again, err)
	}
	if err := store.Complete(ctx, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, &Row{Name: "locks.expire", RunAt: now.UnixMilli(), CreatedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	failed, err := store.ClaimDue(ctx, now, 1)
	if err != nil || len(failed) != 1 {
		t.Fatalf("claim fail row = %v %v", failed, err)
	}
	retry := now.Add(time.Minute)
	if err := store.Fail(ctx, failed[0].ID, "boom", retry); err != nil {
		t.Fatal(err)
	}
	still, err := store.ClaimDue(ctx, now, 8)
	if err != nil || len(still) != 0 {
		t.Fatalf("claimed before retry = %v %v", still, err)
	}
	retried, err := store.ClaimDue(ctx, retry, 8)
	if err != nil || len(retried) != 1 || retried[0].Attempts != 1 {
		t.Fatalf("retry claim = %+v %v", retried, err)
	}
}

func TestClaimDueNoDouble(t *testing.T) {
	ctx := t.Context()
	store := NewSQLStore(testDB(t))
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, &Row{Name: "shares.expire", RunAt: now.UnixMilli(), CreatedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make(chan []Row, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := store.ClaimDue(ctx, now, 8)
			if err != nil {
				t.Error(err)
				return
			}
			got <- rows
		}()
	}
	wg.Wait()
	close(got)
	var total int
	for rows := range got {
		total += len(rows)
	}
	if total != 1 {
		t.Fatalf("claimed %d rows, want 1", total)
	}
}
