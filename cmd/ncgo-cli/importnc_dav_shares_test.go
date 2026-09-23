package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/calendar"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// importDAVSharesSourceEnv builds a source with the real-Nextcloud
// oc_dav_shares schema (principaluri/type/access/resourceid) covering every
// import and skip category of the calendar-share mapping, including an
// addressbook share row.
func importDAVSharesSourceEnv(t *testing.T) string {
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
		`CREATE TABLE oc_calendars (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, calendarorder INTEGER, calendarcolor TEXT,
			timezone TEXT, components TEXT, transparent INTEGER, synctoken INTEGER)`,
		`CREATE TABLE oc_calendarobjects (id INTEGER PRIMARY KEY, calendarid INTEGER, uri TEXT,
			calendardata BLOB, lastmodified INTEGER, etag TEXT, size INTEGER, componenttype TEXT,
			firstoccurence INTEGER, lastoccurence INTEGER, uid TEXT, classification INTEGER)`,
		`CREATE TABLE oc_addressbooks (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, synctoken INTEGER)`,
		`CREATE TABLE oc_cards (id INTEGER PRIMARY KEY, addressbookid INTEGER, uri TEXT,
			carddata BLOB, lastmodified INTEGER, etag TEXT, size INTEGER, uid TEXT)`,
		`CREATE TABLE oc_dav_shares (id INTEGER PRIMARY KEY, principaluri TEXT,
			type TEXT, access INTEGER, resourceid INTEGER, publicuri TEXT)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		id        int
		principal string
		uri       string
	}{
		{1, "principals/users/alice", "personal"},
		{2, "principals/users/alice", "work"},
		{3, "principals/users/ghost", "personal"},
		{4, "principals/users/alice", "conflict"}, // collision-skipped below → shares on it unmatched
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_calendars
			(id, principaluri, uri, displayname, calendarorder, transparent, synctoken)
			VALUES (?, ?, ?, ?, 0, 0, 1)`, c.id, c.principal, c.uri, c.uri); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_addressbooks
		(id, principaluri, uri, displayname, synctoken) VALUES (7, 'principals/users/alice', 'contacts', 'Contacts', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		id        int
		principal string
		typ       string
		access    any
		resource  int
	}{
		{1, "principals/users/bob", "calendar", 3, 1},    // read on personal; target holds read-write → updated
		{2, "principals/users/bob", "calendar", 2, 2},    // read-write on work → created
		{3, "principals/groups/team", "calendar", 3, 1},  // group principal → skipped
		{4, "principals/users/carol", "calendar", 3, 1},  // sharee not in target → skipped
		{5, "principals/users/bob", "addressbook", 3, 7}, // addressbook share → counted, not imported
		{6, "principals/users/bob", "calendar", 3, 42},   // unknown source calendar → skipped
		{7, "principals/users/bob", "calendar", 3, 3},    // calendar not imported → skipped
		{8, "principals/circles/c1", "calendar", 3, 1},   // circle principal → skipped
		{9, "principals/users/bob", "calendar", nil, 1},  // NULL access → skipped
		{10, "principals/users/bob", "calendar", 3, 4},   // collision-skipped calendar → skipped
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_dav_shares (id, principaluri, type, access, resourceid)
			VALUES (?, ?, ?, ?, ?)`, s.id, s.principal, s.typ, s.access, s.resource); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func TestImportNCDAVShares(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSharesSourceEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")

	// Pre-existing ncgo share with different access: the import must update
	// it to the source value (UpsertCalendarShare semantics).
	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	cs := calendar.NewSQLStore(db)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := us.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateCalendar(ctx, &calendar.Calendar{UserID: alice.ID, URI: "personal", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// An unrelated calendar already owns the "conflict" uri, so the source
	// calendar with that uri is skipped wholesale and its share unmatched.
	if err := cs.CreateCalendar(ctx, &calendar.Calendar{UserID: alice.ID, URI: "conflict", DisplayName: "Existing", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	personal, err := cs.GetCalendarByURI(ctx, alice.ID, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.UpsertCalendarShare(ctx, personal.ID, bob.ID, calendar.ShareAccessReadWrite); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendar shares: 1 created, 1 updated, 7 skipped (existing), 0 failed",
		"addressbook shares: 0 created, 1 skipped (existing), 0 failed",
		`warning: calendar share oc_dav_shares:3: principal "principals/groups/team" is not a user principal, skipped`,
		"warning: calendar share oc_dav_shares:4: sharee carol not in target, skipped",
		"warning: calendar share oc_dav_shares:6: source calendar 42 not found, skipped",
		"warning: calendar share oc_dav_shares:7: calendar ghost/personal not imported, share skipped",
		`warning: calendar share oc_dav_shares:8: principal "principals/circles/c1" is not a user principal, skipped`,
		"warning: calendar share oc_dav_shares:9: access 0 not mappable (2=read-write, 3=read), skipped",
		"warning: calendar share oc_dav_shares:10: calendar alice/conflict not imported, share skipped",
		"warning: 1 addressbook share(s) not imported (ncgo has no addressbook sharing; re-share address books after migration)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// personal: updated from read-write down to read (source wins); work:
	// created as read-write.
	shared, err := cs.ListSharedCalendars(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 2 || shared[0].URI != "personal" || shared[1].URI != "work" {
		t.Fatalf("bob shared calendars = %+v", shared)
	}
	if shared[0].Access != calendar.ShareAccessRead {
		t.Errorf("personal access = %q, want read after source update", shared[0].Access)
	}
	if shared[1].Access != calendar.ShareAccessReadWrite {
		t.Errorf("work access = %q, want read-write", shared[1].Access)
	}

	// Second run: fully idempotent — the updated row now matches, so no
	// updates are reported.
	out, err = runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendar shares: 0 created, 9 skipped (existing), 0 failed",
		"addressbook shares: 0 created, 1 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "calendar_shares"); n != 2 {
		t.Errorf("calendar_shares after re-run = %d, want 2", n)
	}
}

func TestImportNCDAVSharesDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSharesSourceEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")

	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	cs := calendar.NewSQLStore(db)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := us.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateCalendar(ctx, &calendar.Calendar{UserID: alice.ID, URI: "personal", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateCalendar(ctx, &calendar.Calendar{UserID: alice.ID, URI: "conflict", DisplayName: "Existing", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	personal, err := cs.GetCalendarByURI(ctx, alice.ID, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.UpsertCalendarShare(ctx, personal.ID, bob.ID, calendar.ShareAccessReadWrite); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn, "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendar shares: 1 created, 1 updated, 7 skipped (existing), 0 failed",
		"addressbook shares: 0 created, 1 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
	// The pre-existing share keeps its access; nothing new is written.
	if n := countRows(t, cfgPath, "calendar_shares"); n != 1 {
		t.Errorf("dry-run must not write calendar_shares, got %d", n)
	}
	sc, err := cs.GetSharedCalendar(ctx, bob.ID, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Access != calendar.ShareAccessReadWrite {
		t.Errorf("dry-run must not update access, got %q", sc.Access)
	}
}
