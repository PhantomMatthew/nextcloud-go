package calendar

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func TestParseRRule(t *testing.T) {
	r, err := parseRRule("FREQ=DAILY;INTERVAL=2;COUNT=5")
	if err != nil {
		t.Fatal(err)
	}
	if r.Freq != "DAILY" || r.Interval != 2 || r.Count != 5 {
		t.Fatalf("r = %+v", r)
	}
	r, err = parseRRule("FREQ=WEEKLY;UNTIL=20250601T000000Z;BYDAY=MO,WE")
	if err != nil {
		t.Fatal(err)
	}
	if r.Freq != "WEEKLY" || r.Interval != 1 || r.Until.Format(icsTimeFormat) != "20250601T000000Z" {
		t.Fatalf("r = %+v", r)
	}
	if _, err := parseRRule("FREQ=HOURLY"); err == nil {
		t.Fatal("expected unsupported FREQ error")
	}
	if _, err := parseRRule("FREQ=DAILY;INTERVAL=0"); err == nil {
		t.Fatal("expected INTERVAL error")
	}
	if _, err := parseRRule(""); err == nil {
		t.Fatal("expected empty error")
	}
}

func busyEv(start, rrule string, durMin int) *parsedEvent {
	s, _ := time.Parse(icsTimeFormat, start)
	ev := &parsedEvent{
		UID:        "x",
		Component:  ComponentVEVENT,
		FirstOccur: s,
		Dur:        time.Duration(durMin) * time.Minute,
	}
	if rrule != "" {
		ev.Recurring = true
		ev.RRule = rrule
	}
	return ev
}

func TestExpandBusyNonRecurring(t *testing.T) {
	win0, _ := time.Parse(icsTimeFormat, "20250501T000000Z")
	win1, _ := time.Parse(icsTimeFormat, "20250601T000000Z")
	ivs := expandBusy(busyEv("20250501T140000Z", "", 60), win0, win1)
	if len(ivs) != 1 || ivs[0][1].Format(icsTimeFormat) != "20250501T150000Z" {
		t.Fatalf("ivs = %v", ivs)
	}
	outside := expandBusy(busyEv("20260701T140000Z", "", 60), win0, win1)
	if len(outside) != 0 {
		t.Fatalf("outside = %v", outside)
	}
}

func TestExpandBusyDailyCount(t *testing.T) {
	win0, _ := time.Parse(icsTimeFormat, "20250501T000000Z")
	win1, _ := time.Parse(icsTimeFormat, "20250510T000000Z")
	ivs := expandBusy(busyEv("20250501T090000Z", "FREQ=DAILY;COUNT=3", 30), win0, win1)
	if len(ivs) != 3 {
		t.Fatalf("ivs = %v", ivs)
	}
	if ivs[2][0].Format(icsTimeFormat) != "20250503T090000Z" {
		t.Fatalf("third = %v", ivs[2][0])
	}
}

func TestExpandBusyWeeklyIntervalWindowClip(t *testing.T) {
	win0, _ := time.Parse(icsTimeFormat, "20250510T000000Z")
	win1, _ := time.Parse(icsTimeFormat, "20250601T000000Z")
	ivs := expandBusy(busyEv("20250501T090000Z", "FREQ=WEEKLY;INTERVAL=2", 60), win0, win1)
	// occurrences: 05-01, 05-15, 05-29; window clips to the last two
	if len(ivs) != 2 || ivs[0][0].Format(icsTimeFormat) != "20250515T090000Z" {
		t.Fatalf("ivs = %v", ivs)
	}
}

func TestExpandBusyUntil(t *testing.T) {
	win0, _ := time.Parse(icsTimeFormat, "20250501T000000Z")
	win1, _ := time.Parse(icsTimeFormat, "20250601T000000Z")
	ivs := expandBusy(busyEv("20250501T090000Z", "FREQ=DAILY;UNTIL=20250503T090000Z", 30), win0, win1)
	if len(ivs) != 3 {
		t.Fatalf("ivs = %v", ivs)
	}
}

func TestExpandBusyUnsupportedRuleFallsBack(t *testing.T) {
	win0, _ := time.Parse(icsTimeFormat, "20250501T000000Z")
	win1, _ := time.Parse(icsTimeFormat, "20250601T000000Z")
	ivs := expandBusy(busyEv("20250501T090000Z", "FREQ=HOURLY", 30), win0, win1)
	if len(ivs) != 1 {
		t.Fatalf("ivs = %v", ivs)
	}
}

func TestMergeBusy(t *testing.T) {
	at := func(s string) time.Time { t, _ := time.Parse(icsTimeFormat, s); return t }
	ivs := mergeBusy([][2]time.Time{
		{at("20250501T140000Z"), at("20250501T150000Z")},
		{at("20250501T143000Z"), at("20250501T160000Z")},
		{at("20250502T090000Z"), at("20250502T100000Z")},
	})
	if len(ivs) != 2 {
		t.Fatalf("ivs = %v", ivs)
	}
	if ivs[0][1] != at("20250501T160000Z") || ivs[1][0] != at("20250502T090000Z") {
		t.Fatalf("ivs = %v", ivs)
	}
	if got := mergeBusy(nil); got != nil {
		t.Fatalf("nil = %v", got)
	}
}

func TestFreeBusyReport(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	dav := &DAV{Store: store, Users: us, Clock: func() time.Time { return freeze }}

	const recurICS = "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:r1\nDTSTART:20250501T140000Z\nDTEND:20250501T150000Z\nRRULE:FREQ=DAILY;COUNT=3\nEND:VEVENT\nEND:VCALENDAR\n"
	const transpICS = "BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:t1\nDTSTART:20250501T140000Z\nDTEND:20250501T150000Z\nTRANSP:TRANSPARENT\nEND:VEVENT\nEND:VCALENDAR\n"
	for name, ics := range map[string]string{"/personal/recur.ics": recurICS, "/personal/transp.ics": transpICS} {
		if _, _, err := dav.Write(ctx, "alice", name, strings.NewReader(ics), nil); err != nil {
			t.Fatal(err)
		}
	}

	body, ct, ok, err := dav.RawReport(ctx, "alice", "/personal", webdav.ReportRequest{
		Name: "free-busy-query",
		Body: []byte(`<c:free-busy-query xmlns:c="urn:ietf:params:xml:ns:caldav"><c:time-range start="20250501T000000Z" end="20250504T000000Z"/></c:free-busy-query>`),
	})
	if err != nil || !ok {
		t.Fatalf("rawreport ok=%v err=%v", ok, err)
	}
	if ct != "text/calendar; charset=utf-8" {
		t.Fatalf("ct = %q", ct)
	}
	text := string(body)
	for _, want := range []string{
		"BEGIN:VFREEBUSY\r\n",
		"DTSTAMP:20250501T120000Z\r\n",
		"DTSTART:20250501T000000Z\r\n",
		"FREEBUSY;FBTYPE=BUSY:20250501T140000Z/20250501T150000Z\r\n",
		"FREEBUSY;FBTYPE=BUSY:20250502T140000Z/20250502T150000Z\r\n",
		"FREEBUSY;FBTYPE=BUSY:20250503T140000Z/20250503T150000Z\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Count(text, "FREEBUSY;") != 3 {
		t.Fatalf("transparent event leaked or count wrong:\n%s", text)
	}

	if _, _, ok, _ := dav.RawReport(ctx, "alice", "/personal", webdav.ReportRequest{Name: "calendar-query"}); ok {
		t.Fatal("calendar-query should fall through")
	}
	if _, _, _, err := dav.RawReport(ctx, "alice", "/personal", webdav.ReportRequest{Name: "free-busy-query", Body: []byte(`<c:free-busy-query/>`)}); err == nil {
		t.Fatal("missing time-range should error")
	}
}
