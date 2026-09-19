package calendar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func TestCalendarDAV_RoundTrip(t *testing.T) {
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
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	dav := &DAV{Store: store, Users: us, Clock: func() time.Time { return freeze }}

	home, err := dav.Stat(ctx, "alice", "/")
	if err != nil || !home.IsDir {
		t.Fatalf("home = %+v %v", home, err)
	}
	kids, err := dav.List(ctx, "alice", "/")
	if err != nil || len(kids) != 1 || !kids[0].IsCalendar {
		t.Fatalf("list home = %v %v", kids, err)
	}

	ent, created, err := dav.Write(ctx, "alice", "/personal/ncgo-event-001.ics", bytes.NewReader([]byte(sampleICS)), nil)
	if err != nil || !created || ent.ETag == "" {
		t.Fatalf("write = %+v created=%v err=%v", ent, created, err)
	}
	rc, got, err := dav.Read(ctx, "alice", "/personal/ncgo-event-001.ics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != sampleICS {
		t.Fatalf("read = %q", body)
	}
	if got.ContentType != "text/calendar; charset=utf-8" {
		t.Errorf("ct = %q", got.ContentType)
	}

	entries, err := dav.Report(ctx, "alice", "/personal", webdav.ReportRequest{
		Name: "calendar-query",
		Body: []byte(`<c:calendar-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="20250501T000000Z" end="20250601T000000Z"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`),
	})
	if err != nil || len(entries) != 1 {
		t.Fatalf("query = %v %v", entries, err)
	}

	mg, err := dav.Report(ctx, "alice", "/personal", webdav.ReportRequest{
		Name: "calendar-multiget",
		Body: []byte(`<c:calendar-multiget xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:href>/remote.php/dav/calendars/alice/personal/ncgo-event-001.ics</d:href><d:href>/remote.php/dav/calendars/alice/personal/missing.ics</d:href></c:calendar-multiget>`),
	})
	if err != nil || len(mg) != 2 {
		t.Fatalf("multiget = %v %v", mg, err)
	}
	if mg[1].Status != http.StatusNotFound {
		t.Errorf("missing status = %d", mg[1].Status)
	}

	if _, err := dav.Report(ctx, "alice", "/personal/ncgo-event-001.ics", webdav.ReportRequest{Name: "calendar-query"}); !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("object report = %v", err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/a", "alice", "/b", false); !errors.Is(err, webdav.ErrMethodNotAllowed) {
		t.Fatalf("move = %v", err)
	}
	if err := dav.Remove(ctx, "alice", "/"); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("remove home = %v", err)
	}
}

func TestPrincipalAndRoot(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	p := &PrincipalDAV{Users: us, Clock: func() time.Time { return freeze }}
	e, err := p.Stat(ctx, "alice", "/")
	if err != nil || !e.IsPrincipal || e.CalendarHomeSet == "" {
		t.Fatalf("principal = %+v %v", e, err)
	}
	root := &RootDAV{Users: us, Clock: func() time.Time { return freeze }}
	re, err := root.Stat(ctx, "alice", "/")
	if err != nil || re.CurrentUserPrincipal == "" {
		t.Fatalf("root = %+v %v", re, err)
	}
	kids, err := root.List(ctx, "alice", "/")
	if err != nil || len(kids) != 3 {
		t.Fatalf("root list = %v %v", kids, err)
	}
	if _, err := root.Stat(ctx, "alice", "/unknown"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
}
