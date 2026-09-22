package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/calendar"
	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/contacts"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

const (
	ncFixtureVEVENT = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-1\r\n" +
		"DTSTART:20260110T100000Z\r\nDTEND:20260110T110000Z\r\nSUMMARY:Meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	ncFixtureVTODO = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VTODO\r\nUID:todo-1\r\n" +
		"DUE:20260201T000000Z\r\nSUMMARY:Task\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	ncFixtureVEVENT2 = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-2\r\n" +
		"DTSTART:20260305T090000Z\r\nDTEND:20260305T093000Z\r\nSUMMARY:Review\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	ncFixtureVCARD  = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card-1\r\nFN:Jane Doe\r\nEND:VCARD\r\n"
	ncFixtureVCARD2 = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card-2\r\nFN:John Roe\r\nEND:VCARD\r\n"
)

// importDAVSourceEnv creates a temp sqlite database holding the PHP Nextcloud
// DAV tables (<prefix>calendars, <prefix>calendarobjects, <prefix>addressbooks,
// <prefix>cards, <prefix>calendarshares) with fixture rows covering every
// imported and skipped category.
func importDAVSourceEnv(t *testing.T, prefix string) string {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "source.db")
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE ` + prefix + `calendars (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, calendarorder INTEGER, calendarcolor TEXT,
			timezone TEXT, components TEXT, transparent INTEGER, synctoken INTEGER)`,
		`CREATE TABLE ` + prefix + `calendarobjects (id INTEGER PRIMARY KEY, calendarid INTEGER, uri TEXT,
			calendardata BLOB, lastmodified INTEGER, etag TEXT, size INTEGER, componenttype TEXT,
			firstoccurence INTEGER, lastoccurence INTEGER, uid TEXT, classification INTEGER)`,
		`CREATE TABLE ` + prefix + `addressbooks (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, synctoken INTEGER)`,
		`CREATE TABLE ` + prefix + `cards (id INTEGER PRIMARY KEY, addressbookid INTEGER, uri TEXT,
			carddata BLOB, lastmodified INTEGER, etag TEXT, size INTEGER, uid TEXT)`,
		`CREATE TABLE ` + prefix + `calendarshares (id INTEGER PRIMARY KEY, calendarid INTEGER,
			principaluri TEXT, access INTEGER)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	str := func(s string) *string { return &s }
	type calRow struct {
		principal, uri           string
		display, desc, color, tz *string
		order, transparent       int
		components               *string
	}
	cals := []calRow{
		// 1: alice personal, fully populated (created).
		{
			"principals/users/alice", "personal", str("Alice Personal"), str("Main calendar"), str("#FF0000"),
			str("BEGIN:VTIMEZONE\nTZID:Europe/Berlin\nEND:VTIMEZONE"), 5, 0, str("VEVENT,VTODO"),
		},
		// 2: alice work, NULLs + transparent flag (created; flag dropped).
		{"principals/users/alice", "work", str("Work"), nil, nil, nil, 0, 1, str("VEVENT")},
		// 3: owner not in target (skipped with its objects).
		{"principals/users/ghost", "personal", str("Ghost"), nil, nil, nil, 0, 0, str("VEVENT")},
		// 4: non-user principal (skipped).
		{"principals/groups/team", "team", str("Team"), nil, nil, nil, 0, 0, str("VEVENT")},
	}
	for i, c := range cals {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`calendars
			(id, principaluri, uri, displayname, description, calendarorder, calendarcolor, timezone, components, transparent, synctoken)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 7)`,
			i+1, c.principal, c.uri, c.display, c.desc, c.order, c.color, c.tz, c.components, c.transparent); err != nil {
			t.Fatal(err)
		}
	}
	type objRow struct {
		calID int64
		uri   string
		data  string
	}
	objs := []objRow{
		{1, "event1.ics", ncFixtureVEVENT},
		{1, "todo1.ics", ncFixtureVTODO},
		{1, "empty.ics", ""}, // empty calendardata (skipped)
		{2, "event2.ics", ncFixtureVEVENT2},
		{3, "ghost.ics", ncFixtureVEVENT2}, // skipped with its calendar
	}
	for i, o := range objs {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`calendarobjects
			(id, calendarid, uri, calendardata, lastmodified, etag, size, componenttype, firstoccurence, lastoccurence, uid, classification)
			VALUES (?, ?, ?, ?, 1600000000, 'nc-etag', 0, 'VEVENT', 0, 0, 'nc-uid', 0)`,
			i+1, o.calID, o.uri, o.data); err != nil {
			t.Fatal(err)
		}
	}
	type bookRow struct {
		principal, uri string
		display, desc  *string
	}
	books := []bookRow{
		{"principals/users/alice", "contacts", str("Alice Contacts"), str("Main book")},
		{"principals/users/ghost", "contacts", str("Ghost Book"), nil},
	}
	for i, b := range books {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`addressbooks
			(id, principaluri, uri, displayname, description, synctoken) VALUES (?, ?, ?, ?, ?, 3)`,
			i+1, b.principal, b.uri, b.display, b.desc); err != nil {
			t.Fatal(err)
		}
	}
	type cardRow struct {
		bookID int64
		uri    string
		data   string
	}
	cards := []cardRow{
		{1, "card1.vcf", ncFixtureVCARD},
		{1, "card2.vcf", ncFixtureVCARD2},
		{1, "empty.vcf", ""}, // empty carddata (skipped)
		{2, "ghost.vcf", ncFixtureVCARD},
	}
	for i, c := range cards {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`cards
			(id, addressbookid, uri, carddata, lastmodified, etag, size, uid)
			VALUES (?, ?, ?, ?, 1600000000, 'nc-etag', 0, 'nc-uid')`,
			i+1, c.bookID, c.uri, c.data); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := db.Exec(ctx, `INSERT INTO `+prefix+`calendarshares (id, calendarid, principaluri, access)
			VALUES (?, 1, 'principals/users/bob', 1)`, i); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func importDAVArgs(cfgPath, dsn string, extra ...string) []string {
	args := make([]string, 0, 8+len(extra))
	args = append(args,
		"--config", cfgPath, "import-nextcloud", "dav",
		"--source-driver", "sqlite", "--source-dsn", dsn,
	)
	return append(args, extra...)
}

func openTargetDB(t *testing.T, cfgPath string) database.DB {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestImportNCDAV(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSourceEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 2 created, 2 skipped (existing), 0 failed",
		"calendar objects: 3 created, 2 skipped (existing), 0 failed",
		"addressbooks: 1 created, 1 skipped (existing), 0 failed",
		"cards: 2 created, 2 skipped (existing), 0 failed",
		"calendar shares: 0 created, 2 skipped (existing), 0 failed",
		"warning: calendar 3: owner ghost not in target (run 'import-nextcloud users' first), skipped with its objects",
		`warning: calendar 4: principal "principals/groups/team" is not a user principal, skipped with its objects`,
		"warning: calendar object 3 (empty.ics): empty calendardata, skipped",
		"warning: addressbook 2: owner ghost not in target (run 'import-nextcloud users' first), skipped with its cards",
		"warning: card 3 (empty.vcf): empty carddata, skipped",
		"warning: 4 calendar components list(s) not imported (ncgo has no components field)",
		"warning: 1 transparent calendar flag(s) not imported (ncgo has no transparent field)",
		"warning: 2 calendar share(s) in oc_calendarshares not imported (invite-state mapping out of scope; re-share calendars after migration)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cs := calendar.NewSQLStore(db)
	bs := contacts.NewSQLStore(db)

	cals, err := cs.ListCalendars(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 2 || cals[0].URI != "personal" || cals[1].URI != "work" {
		t.Fatalf("calendars = %+v", cals)
	}
	personal := cals[0]
	if personal.DisplayName != "Alice Personal" || personal.Description != "Main calendar" ||
		personal.Color != "#FF0000" || personal.Order != 5 ||
		personal.Timezone != "BEGIN:VTIMEZONE\nTZID:Europe/Berlin\nEND:VTIMEZONE" || !personal.Enabled {
		t.Errorf("personal calendar mapping = %+v", personal)
	}
	// Fresh sync state: ctag starts at 1 and is bumped once per object by the
	// production PutObject path (2 objects here).
	if personal.CTag != 3 {
		t.Errorf("personal ctag = %d, want 3 (1 + 2 object bumps)", personal.CTag)
	}
	work := cals[1]
	if work.DisplayName != "Work" || work.Color != calendar.DefaultCalendarColor || work.CTag != 2 {
		t.Errorf("work calendar = %+v", work)
	}

	// Objects carry the verbatim ICS bytes with ncgo-computed index fields.
	evt, err := cs.GetObject(ctx, alice.ID, "personal", "event1.ics")
	if err != nil {
		t.Fatal(err)
	}
	if string(evt.Data) != ncFixtureVEVENT {
		t.Error("event1.ics data must be byte-identical")
	}
	if evt.UID != "evt-1" || evt.Component != calendar.ComponentVEVENT || evt.Size != int64(len(ncFixtureVEVENT)) {
		t.Errorf("event1 = %+v", evt)
	}
	wantStart := time.Date(2026, 1, 10, 10, 0, 0, 0, time.UTC)
	if !evt.FirstOccur.Equal(wantStart) {
		t.Errorf("event1 first occurrence = %v, want %v", evt.FirstOccur, wantStart)
	}
	// etag is ncgo's SHA-1-of-data (40 hex chars), not the source etag; the
	// source lastmodified (1600000000) is not preserved — updated_at is the
	// import time, so clients resync.
	if len(evt.ETag) != 40 || evt.ETag == "nc-etag" {
		t.Errorf("event1 etag = %q, want a regenerated SHA-1 hex etag", evt.ETag)
	}
	if evt.UpdatedAt.Unix() == 1600000000 {
		t.Error("source lastmodified must not be preserved (regenerated at import time)")
	}
	todo, err := cs.GetObject(ctx, alice.ID, "personal", "todo1.ics")
	if err != nil {
		t.Fatal(err)
	}
	if todo.Component != calendar.ComponentVTODO || string(todo.Data) != ncFixtureVTODO {
		t.Errorf("todo1 = %+v", todo)
	}
	// The object is visible through the production range-query read path.
	inRange, err := cs.ObjectsInRange(ctx, alice.ID, "personal",
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(inRange) != 1 || inRange[0].URI != "event1.ics" {
		t.Errorf("ObjectsInRange = %+v", inRange)
	}
	all, err := cs.ListObjects(ctx, alice.ID, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("personal objects = %d, want 2 (empty-data row skipped)", len(all))
	}

	books, err := bs.ListBooks(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 || books[0].URI != "contacts" ||
		books[0].DisplayName != "Alice Contacts" || books[0].Description != "Main book" {
		t.Fatalf("addressbooks = %+v", books)
	}
	if books[0].CTag != 3 {
		t.Errorf("addressbook ctag = %d, want 3", books[0].CTag)
	}
	card, err := bs.GetObject(ctx, alice.ID, "contacts", "card1.vcf")
	if err != nil {
		t.Fatal(err)
	}
	if string(card.Data) != ncFixtureVCARD || card.UID != "card-1" || card.FN != "Jane Doe" {
		t.Errorf("card1 = %+v", card)
	}
	cards, err := bs.ListObjects(ctx, alice.ID, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 {
		t.Errorf("cards = %d, want 2 (empty-data row skipped)", len(cards))
	}

	// Second run: fully idempotent, everything skipped, counts flat.
	out, err = runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 0 created, 4 skipped (existing), 0 failed",
		"calendar objects: 0 created, 5 skipped (existing), 0 failed",
		"addressbooks: 0 created, 2 skipped (existing), 0 failed",
		"cards: 0 created, 4 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "calendar_objects"); n != 3 {
		t.Errorf("calendar_objects after re-run = %d, want 3", n)
	}
	if n := countRows(t, cfgPath, "addressbook_objects"); n != 2 {
		t.Errorf("addressbook_objects after re-run = %d, want 2", n)
	}
}

func TestImportNCDAVDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSourceEnv(t, "nc_")
	createTargetUser(t, cfgPath, "alice")

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn, "--table-prefix", "nc_", "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 2 created, 2 skipped (existing), 0 failed",
		"calendar objects: 3 created, 2 skipped (existing), 0 failed",
		"addressbooks: 1 created, 1 skipped (existing), 0 failed",
		"cards: 2 created, 2 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	for _, table := range []string{"calendars", "calendar_objects", "addressbooks", "addressbook_objects"} {
		if n := countRows(t, cfgPath, table); n != 0 {
			t.Errorf("dry-run must not write %s, got %d rows", table, n)
		}
	}
}

func TestImportNCDAVCalendarCollision(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSourceEnv(t, "oc_")
	createTargetUser(t, cfgPath, "alice")

	// An unrelated calendar already owns the "work" uri for alice.
	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cs := calendar.NewSQLStore(db)
	pre := &calendar.Calendar{UserID: alice.ID, URI: "work", DisplayName: "Existing Work", Enabled: true}
	if err := cs.CreateCalendar(ctx, pre); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`warning: calendar 2: uri "work" already exists for alice with different properties, skipped with its objects`,
		"calendars: 1 created, 3 skipped (existing), 0 failed",
		"calendar objects: 2 created, 3 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The pre-existing calendar is untouched: same displayname, no objects.
	work, err := cs.GetCalendarByURI(ctx, alice.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if work.DisplayName != "Existing Work" {
		t.Errorf("pre-existing calendar must not be overwritten: %+v", work)
	}
	objs, err := cs.ListObjects(ctx, alice.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Errorf("collision calendar must not receive foreign objects: %+v", objs)
	}
}

func TestNCPrincipalUID(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"principals/users/alice", "alice", true},
		{"principals/groups/team", "", false},
		{"principals/users/", "", false},
		{"principals/users/a/b", "", false},
		{"alice", "", false},
	} {
		got, ok := ncPrincipalUID(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ncPrincipalUID(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
