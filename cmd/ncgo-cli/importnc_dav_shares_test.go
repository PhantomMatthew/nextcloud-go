package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/calendar"
	"github.com/PhantomMatthew/nextcloud-go/internal/contacts"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// importDAVSharesSourceEnv builds a source with the real-Nextcloud
// oc_dav_shares schema (principaluri/type/access/resourceid) covering every
// import and skip category of the calendar-share and addressbook-share
// mappings.
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
	for _, b := range []struct {
		id        int
		principal string
		uri       string
		display   string
	}{
		{7, "principals/users/alice", "contacts", "Contacts"},
		{8, "principals/users/alice", "work", "work"},
		{9, "principals/users/ghost", "ghostbook", "ghostbook"}, // owner missing → book not imported
		{10, "principals/users/alice", "conflict", "conflict"},  // collision-skipped below → shares on it unmatched
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_addressbooks
			(id, principaluri, uri, displayname, synctoken) VALUES (?, ?, ?, ?, 1)`,
			b.id, b.principal, b.uri, b.display); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []struct {
		id        int
		principal string
		typ       string
		access    any
		resource  int
	}{
		{1, "principals/users/bob", "calendar", 3, 1},       // read on personal; target holds read-write → updated
		{2, "principals/users/bob", "calendar", 2, 2},       // read-write on work → created
		{3, "principals/groups/team", "calendar", 3, 1},     // group principal → skipped
		{4, "principals/users/carol", "calendar", 3, 1},     // sharee not in target → skipped
		{5, "principals/users/bob", "addressbook", 3, 7},    // read on contacts; target holds read-write → updated
		{6, "principals/users/bob", "calendar", 3, 42},      // unknown source calendar → skipped
		{7, "principals/users/bob", "calendar", 3, 3},       // calendar not imported → skipped
		{8, "principals/circles/c1", "calendar", 3, 1},      // circle principal → skipped
		{9, "principals/users/bob", "calendar", nil, 1},     // NULL access → skipped
		{10, "principals/users/bob", "calendar", 3, 4},      // collision-skipped calendar → skipped
		{11, "principals/users/bob", "addressbook", 2, 8},   // read-write on work book → created
		{12, "principals/groups/team", "addressbook", 3, 7}, // group principal → skipped
		{13, "principals/circles/c1", "addressbook", 3, 7},  // circle principal → skipped
		{14, "principals/users/carol", "addressbook", 3, 7}, // sharee not in target → skipped
		{15, "principals/users/alice", "addressbook", 3, 7}, // alice owns the book → self-share skipped
		{16, "principals/users/bob", "addressbook", 3, 42},  // unknown source addressbook → skipped
		{17, "principals/users/bob", "addressbook", 3, 9},   // book not imported (owner missing) → skipped
		{18, "principals/users/bob", "addressbook", 3, 10},  // collision-skipped book → skipped
		{19, "principals/users/bob", "addressbook", nil, 7}, // NULL access → skipped
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_dav_shares (id, principaluri, type, access, resourceid)
			VALUES (?, ?, ?, ?, ?)`, s.id, s.principal, s.typ, s.access, s.resource); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

// prepareDAVSharesTarget creates the target state the shares fixture
// expects: the "personal"/"conflict" calendars and "contacts"/"conflict"
// addressbooks pre-exist (the conflict pair with different properties so the
// source rows collide), and a read-write share on personal+contacts for bob
// that the source downgrades to read.
func prepareDAVSharesTarget(t *testing.T, cfgPath string) {
	t.Helper()
	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	cs := calendar.NewSQLStore(db)
	bs := contacts.NewSQLStore(db)
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
	if err := bs.CreateBook(ctx, &contacts.Addressbook{UserID: alice.ID, URI: "contacts", DisplayName: "Contacts", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Same collision for the "conflict" addressbook.
	if err := bs.CreateBook(ctx, &contacts.Addressbook{UserID: alice.ID, URI: "conflict", DisplayName: "Existing", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	book, err := bs.GetBookByURI(ctx, alice.ID, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.UpsertAddressbookShare(ctx, book.ID, bob.ID, contacts.ShareAccessReadWrite); err != nil {
		t.Fatal(err)
	}
}

func TestImportNCDAVShares(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSharesSourceEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")

	// Pre-existing ncgo shares with different access: the import must update
	// them to the source values (upsert semantics).
	prepareDAVSharesTarget(t, cfgPath)

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"addressbooks: 1 created, 3 skipped (existing), 0 failed",
		"calendar shares: 1 created, 1 updated, 7 skipped (existing), 0 failed",
		"addressbook shares: 1 created, 1 updated, 8 skipped (existing), 0 failed",
		`warning: calendar share oc_dav_shares:3: principal "principals/groups/team" is not a user principal, skipped`,
		"warning: calendar share oc_dav_shares:4: sharee carol not in target, skipped",
		"warning: calendar share oc_dav_shares:6: source calendar 42 not found, skipped",
		"warning: calendar share oc_dav_shares:7: calendar ghost/personal not imported, share skipped",
		`warning: calendar share oc_dav_shares:8: principal "principals/circles/c1" is not a user principal, skipped`,
		"warning: calendar share oc_dav_shares:9: access 0 not mappable (2=read-write, 3=read), skipped",
		"warning: calendar share oc_dav_shares:10: calendar alice/conflict not imported, share skipped",
		`warning: addressbook share oc_dav_shares:12: principal "principals/groups/team" is not a user principal, skipped`,
		`warning: addressbook share oc_dav_shares:13: principal "principals/circles/c1" is not a user principal, skipped`,
		"warning: addressbook share oc_dav_shares:14: sharee carol not in target, skipped",
		"warning: addressbook share oc_dav_shares:15: alice is the addressbook owner, self-share skipped",
		"warning: addressbook share oc_dav_shares:16: source addressbook 42 not found, skipped",
		"warning: addressbook share oc_dav_shares:17: addressbook ghost/ghostbook not imported, share skipped",
		"warning: addressbook share oc_dav_shares:18: addressbook alice/conflict not imported, share skipped",
		"warning: addressbook share oc_dav_shares:19: access 0 not mappable (2=read-write, 3=read), skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "not imported (ncgo has no addressbook sharing") {
		t.Errorf("stale addressbook-shares skip warning:\n%s", out)
	}

	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	cs := calendar.NewSQLStore(db)
	bs := contacts.NewSQLStore(db)
	bob, err := us.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
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

	// contacts: updated from read-write down to read; work book: created as
	// read-write.
	sharedBooks, err := bs.ListSharedAddressbooks(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sharedBooks) != 2 || sharedBooks[0].URI != "contacts" || sharedBooks[1].URI != "work" {
		t.Fatalf("bob shared addressbooks = %+v", sharedBooks)
	}
	if sharedBooks[0].OwnerUID != "alice" || sharedBooks[0].Access != contacts.ShareAccessRead {
		t.Errorf("contacts share = %+v, want alice/read after source update", sharedBooks[0])
	}
	if sharedBooks[1].OwnerUID != "alice" || sharedBooks[1].Access != contacts.ShareAccessReadWrite {
		t.Errorf("work share = %+v, want alice/read-write", sharedBooks[1])
	}

	// Second run: fully idempotent — the updated rows now match, so no
	// updates are reported.
	out, err = runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendar shares: 0 created, 9 skipped (existing), 0 failed",
		"addressbook shares: 0 created, 10 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "calendar_shares"); n != 2 {
		t.Errorf("calendar_shares after re-run = %d, want 2", n)
	}
	if n := countRows(t, cfgPath, "addressbook_shares"); n != 2 {
		t.Errorf("addressbook_shares after re-run = %d, want 2", n)
	}
}

func TestImportNCDAVSharesDryRun(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVSharesSourceEnv(t)
	createTargetUser(t, cfgPath, "alice")
	createTargetUser(t, cfgPath, "bob")
	prepareDAVSharesTarget(t, cfgPath)

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn, "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendar shares: 1 created, 1 updated, 7 skipped (existing), 0 failed",
		"addressbook shares: 1 created, 1 updated, 8 skipped (existing), 0 failed",
		"dry-run: no changes written",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	// The pre-existing shares keep their access; nothing new is written.
	if n := countRows(t, cfgPath, "calendar_shares"); n != 1 {
		t.Errorf("dry-run must not write calendar_shares, got %d", n)
	}
	if n := countRows(t, cfgPath, "addressbook_shares"); n != 1 {
		t.Errorf("dry-run must not write addressbook_shares, got %d", n)
	}
	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	cs := calendar.NewSQLStore(db)
	bs := contacts.NewSQLStore(db)
	bob, err := us.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cs.GetSharedCalendar(ctx, bob.ID, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Access != calendar.ShareAccessReadWrite {
		t.Errorf("dry-run must not update access, got %q", sc.Access)
	}
	sb, err := bs.GetSharedAddressbook(ctx, bob.ID, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Access != contacts.ShareAccessReadWrite {
		t.Errorf("dry-run must not update access, got %q", sb.Access)
	}
}
