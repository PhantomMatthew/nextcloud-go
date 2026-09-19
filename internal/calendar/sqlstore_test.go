package calendar

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
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

func seedUser(t *testing.T, db database.DB) int64 {
	t.Helper()
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := users.NewSQLStore(db).Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

const sampleICS = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//nextcloud-go//EN\nBEGIN:VEVENT\nUID:ncgo-event-001\nDTSTAMP:20250501T120000Z\nDTSTART:20250501T140000Z\nDTEND:20250501T150000Z\nSUMMARY:Phase 3a\nEND:VEVENT\nEND:VCALENDAR\n"

func TestSQLStore_HomeAndObject(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	uid := seedUser(t, db)

	if err := store.EnsureHome(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHome(ctx, uid); err != nil {
		t.Fatal(err)
	}
	cals, err := store.ListCalendars(ctx, uid)
	if err != nil || len(cals) != 1 || cals[0].URI != DefaultCalendarURI {
		t.Fatalf("cals=%v err=%v", cals, err)
	}

	obj := &Object{URI: "ncgo-event-001.ics", Data: []byte(sampleICS)}
	created, err := store.PutObject(ctx, uid, DefaultCalendarURI, obj)
	if err != nil || !created || obj.ETag == "" {
		t.Fatalf("put = created=%v %+v %v", created, obj, err)
	}
	got, err := store.GetObject(ctx, uid, DefaultCalendarURI, obj.URI)
	if err != nil || got.UID != "ncgo-event-001" {
		t.Fatalf("get = %+v %v", got, err)
	}

	dup := &Object{URI: "other.ics", Data: []byte(sampleICS)}
	if _, err := store.PutObject(ctx, uid, DefaultCalendarURI, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("uid conflict = %v", err)
	}

	start := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	in, err := store.ObjectsInRange(ctx, uid, DefaultCalendarURI, start, end)
	if err != nil || len(in) != 1 {
		t.Fatalf("range hit = %v %v", in, err)
	}
	missStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	miss, err := store.ObjectsInRange(ctx, uid, DefaultCalendarURI, missStart, missStart.Add(24*time.Hour))
	if err != nil || len(miss) != 0 {
		t.Fatalf("range miss = %v %v", miss, err)
	}

	if err := store.DeleteObject(ctx, uid, DefaultCalendarURI, obj.URI); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetObject(ctx, uid, DefaultCalendarURI, obj.URI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted = %v", err)
	}
}
