package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// davBrokenSourceEnv builds a source DB whose calendarobjects table is
// missing and which has no calendar-share table at all, exercising the
// per-object read-failure path, the skipped-objects count failure, and the
// no-share-table silent path. extraDDL creates any additional tables.
func davBrokenSourceEnv(t *testing.T, extraDDL ...string) string {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "source.db")
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	stmts := append([]string{
		`CREATE TABLE oc_calendars (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, calendarorder INTEGER, calendarcolor TEXT,
			timezone TEXT, components TEXT, transparent INTEGER, synctoken INTEGER)`,
	}, extraDDL...)
	for _, stmt := range stmts {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i, p := range []string{"principals/users/alice", "principals/users/ghost"} {
		if _, err := db.Exec(ctx, `INSERT INTO oc_calendars
			(id, principaluri, uri, displayname, calendarorder, transparent, synctoken)
			VALUES (?, ?, 'cal', 'Cal', 0, 0, 1)`, i+1, p); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func TestImportNCDAVBrokenSource(t *testing.T) {
	cfgPath := cliEnv(t)
	createTargetUser(t, cfgPath, "alice")

	// calendarobjects missing; addressbooks/cards present; empty share table.
	dsn := davBrokenSourceEnv(t,
		`CREATE TABLE oc_addressbooks (id INTEGER PRIMARY KEY, principaluri TEXT, uri TEXT,
			displayname TEXT, description TEXT, synctoken INTEGER)`,
		`CREATE TABLE oc_cards (id INTEGER PRIMARY KEY, addressbookid INTEGER, uri TEXT,
			carddata BLOB, lastmodified INTEGER, etag TEXT, size INTEGER, uid TEXT)`,
		`CREATE TABLE oc_calendarshares (id INTEGER PRIMARY KEY, calendarid INTEGER,
			principaluri TEXT, access INTEGER)`)
	out, err := runCLI(t, "", importDAVArgs(cfgPath, dsn)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"calendars: 1 created, 1 skipped (existing), 0 failed",
		"calendar objects: 0 created, 0 skipped (existing), 1 failed",
		"warning: calendar objects of source calendar 1: read failed",
		"warning: calendar 2: owner ghost not in target (run 'import-nextcloud users' first), skipped with its objects",
		"warning: count rows of oc_calendarobjects 2:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "calendar shares:") {
		t.Errorf("empty share table must mean no calendar-shares report line:\n%s", out)
	}

	// Dry-run against the same broken source but a fresh target: the dry-run
	// object counter hits the missing table too.
	cfgPath2 := cliEnv(t)
	createTargetUser(t, cfgPath2, "alice")
	out, err = runCLI(t, "", importDAVArgs(cfgPath2, dsn, "--dry-run")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: dry-run: count objects of oc_calendarobjects 1:") {
		t.Errorf("dry-run output missing count warning:\n%s", out)
	}

	// addressbooks missing: the run fails with a wrapped read error.
	dsn2 := davBrokenSourceEnv(t)
	if _, err := runCLI(t, "", importDAVArgs(cfgPath, dsn2)...); err == nil ||
		!strings.Contains(err.Error(), "import dav: read source addressbooks") {
		t.Errorf("missing addressbooks table must fail the run, got %v", err)
	}

	// calendars missing: the run fails even earlier.
	ctx := context.Background()
	emptyDSN := "file:" + filepath.Join(t.TempDir(), "empty.db")
	db, err := database.Open(ctx, database.Config{Driver: database.DialectSQLite, DSN: emptyDSN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `CREATE TABLE dummy (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(t, "", importDAVArgs(cfgPath, emptyDSN)...); err == nil ||
		!strings.Contains(err.Error(), "import dav: read source calendars") {
		t.Errorf("missing calendars table must fail the run, got %v", err)
	}
}
