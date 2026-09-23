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

func TestListRecent(t *testing.T) {
	ctx := t.Context()
	store := NewSQLStore(testDB(t))
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	nowMs := now.UnixMilli()

	// Seed one row per state shape: queued (neither timestamp), running
	// (started only), done (completed), failed (error text, not completed —
	// the shape Fail leaves behind after it resets started_at). Insert only
	// writes the claim-facing columns, so the timestamp states land via SQL.
	seed := []Row{
		{Name: "queued.job", RunAt: nowMs, CreatedAt: nowMs},
		{Name: "running.job", RunAt: nowMs, CreatedAt: nowMs},
		{Name: "done.job", RunAt: nowMs, CreatedAt: nowMs},
		{Name: "failed.job", RunAt: nowMs, LastError: "boom", Attempts: 2, CreatedAt: nowMs},
	}
	for i := range seed {
		r := seed[i]
		if err := store.Insert(ctx, &r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(ctx, `UPDATE jobs SET started_at = ? WHERE name = ?`, nowMs, "running.job"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(ctx, `UPDATE jobs SET started_at = ?, completed_at = ? WHERE name = ?`, nowMs, nowMs, "done.job"); err != nil {
		t.Fatal(err)
	}

	recent, err := store.ListRecent(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != len(seed) {
		t.Fatalf("ListRecent = %d rows, want %d", len(recent), len(seed))
	}
	for i := 1; i < len(recent); i++ {
		if recent[i-1].ID < recent[i].ID {
			t.Fatalf("ListRecent not id DESC: %+v", recent)
		}
	}
	if recent[0].Name != "failed.job" || recent[0].LastError != "boom" || recent[0].Attempts != 2 {
		t.Errorf("newest row = %+v", recent[0])
	}
	// Every state shape round-trips: nullable timestamps and error text.
	var running, done Row
	for _, r := range recent {
		switch r.Name {
		case "running.job":
			running = r
		case "done.job":
			done = r
		}
	}
	if running.StartedAt != nowMs || running.CompletedAt != 0 {
		t.Errorf("running row = %+v", running)
	}
	if done.CompletedAt != nowMs {
		t.Errorf("done row = %+v", done)
	}

	// Limit is honored and a non-positive limit falls back to the default.
	one, err := store.ListRecent(ctx, 1)
	if err != nil || len(one) != 1 || one[0].Name != "failed.job" {
		t.Fatalf("ListRecent(1) = %+v %v", one, err)
	}
	def, err := store.ListRecent(ctx, 0)
	if err != nil || len(def) != len(seed) {
		t.Fatalf("ListRecent(0) = %d %v", len(def), err)
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
