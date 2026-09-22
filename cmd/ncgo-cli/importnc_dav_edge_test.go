package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/contacts"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// importDAVEdgeSourceEnv builds a minimal DAV source exercising the
// object-level skip categories (unparseable ICS, unsupported VJOURNAL, UID
// conflict), an addressbook uri collision, and the newer-Nextcloud dav_shares
// calendar-share table.
func importDAVEdgeSourceEnv(t *testing.T) string {
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
		`CREATE TABLE oc_dav_shares (id INTEGER PRIMARY KEY, calendarid INTEGER,
			principaluri TEXT, access INTEGER)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_calendars
		(id, principaluri, uri, calendarorder, transparent, synctoken)
		VALUES (1, 'principals/users/alice', 'main', 0, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	vjournal := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VJOURNAL\r\nUID:j-1\r\n" +
		"DTSTART:20260110T100000Z\r\nEND:VJOURNAL\r\nEND:VCALENDAR\r\n"
	dupUID := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:evt-1\r\n" +
		"DTSTART:20260111T100000Z\r\nDTEND:20260111T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for i, o := range []struct{ uri, data string }{
		{"good.ics", ncFixtureVEVENT},
		{"bad.ics", "this is not icalendar data"},
		{"journal.ics", vjournal},
		{"dup.ics", dupUID}, // same UID as good.ics under a different uri
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_calendarobjects
			(id, calendarid, uri, calendardata, lastmodified, etag, size, componenttype, firstoccurence, lastoccurence, uid, classification)
			VALUES (?, 1, ?, ?, 1600000000, 'e', 0, 'VEVENT', 0, 0, 'u', 0)`, i+1, o.uri, o.data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_addressbooks
		(id, principaluri, uri, displayname, description, synctoken)
		VALUES (1, 'principals/users/alice', 'contacts', 'Source Book', NULL, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_addressbooks
		(id, principaluri, uri, displayname, description, synctoken)
		VALUES (2, 'principals/users/alice', 'work', 'Work Book', NULL, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_cards
		(id, addressbookid, uri, carddata, lastmodified, etag, size, uid)
		VALUES (1, 1, 'card1.vcf', ?, 1600000000, 'e', 0, 'u')`, ncFixtureVCARD); err != nil {
		t.Fatal(err)
	}
	dupCard := "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:card-2\r\nFN:John Duplicate\r\nEND:VCARD\r\n"
	for i, c := range []struct{ uri, data string }{
		{"good.vcf", ncFixtureVCARD2},
		{"bad.vcf", "not a vcard at all"},
		{"dup.vcf", dupCard}, // same UID as good.vcf under a different uri
	} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_cards
			(id, addressbookid, uri, carddata, lastmodified, etag, size, uid)
			VALUES (?, 2, ?, ?, 1600000000, 'e', 0, 'u')`, i+2, c.uri, c.data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO oc_dav_shares (id, calendarid, principaluri, access)
		VALUES (1, 1, 'principals/users/bob', 1)`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestImportNCDAVEdgeCases(t *testing.T) {
	cfgPath := cliEnv(t)
	dsn := importDAVEdgeSourceEnv(t)
	createTargetUser(t, cfgPath, "alice")

	// An unrelated addressbook already owns the "contacts" uri for alice.
	ctx := context.Background()
	db := openTargetDB(t, cfgPath)
	us := openTargetStore(t, cfgPath)
	alice, err := us.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bs := contacts.NewSQLStore(db)
	if err := bs.CreateBook(ctx, &contacts.Addressbook{
		UserID: alice.ID, URI: "contacts", DisplayName: "Existing Book", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 1 created, 0 skipped (existing), 0 failed",
		"calendar objects: 1 created, 3 skipped (existing), 0 failed",
		"addressbooks: 1 created, 1 skipped (existing), 0 failed",
		"cards: 1 created, 3 skipped (existing), 0 failed",
		"calendar shares: 0 created, 1 skipped (existing), 0 failed",
		"warning: calendar object 2 (bad.ics): not importable (calendar: invalid: missing VCALENDAR), skipped",
		"warning: calendar object 3 (journal.ics): not importable (calendar: unsupported), skipped",
		"warning: calendar object 4 (dup.ics): UID already used by another object, skipped",
		`warning: addressbook 1: uri "contacts" already exists for alice with different properties, skipped with its cards`,
		"warning: card 3 (bad.vcf): not importable (contacts: invalid: want exactly one VCARD), skipped",
		"warning: card 4 (dup.vcf): UID already used by another card, skipped",
		"warning: 1 calendar share(s) in oc_dav_shares not imported (invite-state mapping out of scope; re-share calendars after migration)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The collided addressbook is untouched.
	book, err := bs.GetBookByURI(ctx, alice.ID, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if book.DisplayName != "Existing Book" {
		t.Errorf("pre-existing addressbook must not be overwritten: %+v", book)
	}
	cards, err := bs.ListObjects(ctx, alice.ID, "contacts")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Errorf("collision addressbook must not receive foreign cards: %+v", cards)
	}

	// The imported "work" book holds only the good card.
	workCards, err := bs.ListObjects(ctx, alice.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(workCards) != 1 || workCards[0].URI != "good.vcf" || workCards[0].UID != "card-2" {
		t.Errorf("work cards = %+v", workCards)
	}

	// Second run: fully idempotent. The NULL-displayname calendar matches its
	// imported form (store defaulted displayname to the uri), so it is a
	// resume, not a collision; rejected objects re-skip the same way.
	out, err = runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 0 created, 1 skipped (existing), 0 failed",
		"calendar objects: 0 created, 4 skipped (existing), 0 failed",
		"addressbooks: 0 created, 2 skipped (existing), 0 failed",
		"cards: 0 created, 4 skipped (existing), 0 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("second run output missing %q:\n%s", want, out)
		}
	}
	if n := countRows(t, cfgPath, "calendar_objects"); n != 1 {
		t.Errorf("calendar_objects after re-run = %d, want 1", n)
	}
	if n := countRows(t, cfgPath, "addressbook_objects"); n != 1 {
		t.Errorf("addressbook_objects after re-run = %d, want 1", n)
	}
}
