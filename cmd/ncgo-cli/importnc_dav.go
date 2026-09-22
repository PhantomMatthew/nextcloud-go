package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/calendar"
	"github.com/PhantomMatthew/nextcloud-go/internal/contacts"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// ncUserPrincipalPrefix is the principaluri form oc_calendars/oc_addressbooks
// rows carry for user-owned collections; any other principal form (groups,
// system principals) is skipped.
const ncUserPrincipalPrefix = "principals/users/"

func newImportNCDAV(f *importNCFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "dav",
		Short: "Import calendars, calendar objects, address books, and contacts",
		Long: "Import calendars and address books (with their objects) from the\n" +
			"source <prefix>calendars, <prefix>calendarobjects, <prefix>addressbooks,\n" +
			"and <prefix>cards tables.\n\n" +
			"Precondition: each collection's principaluri must be a user principal\n" +
			"(principals/users/<uid>) whose uid already exists in the target — run\n" +
			"'import-nextcloud users' first. Other principal forms and unknown owners\n" +
			"are skipped with a warning, together with all of their objects.\n\n" +
			"Objects are written through the same production store path a client PUT\n" +
			"uses, so uid, component type, first/last occurrence, size, and etag are\n" +
			"computed exactly as ncgo computes them (etag is the SHA-1 of the data,\n" +
			"so identical content keeps an identical etag; Nextcloud's own etags and\n" +
			"synctokens are NOT preserved and each object's lastmodified becomes the\n" +
			"import time — DAV clients must do one full resync after migration).\n" +
			"Calendar color, order, timezone, displayname, and description are\n" +
			"preserved; Nextcloud's per-calendar components list and transparent flag\n" +
			"have no ncgo counterpart and are dropped (one summary warning each).\n" +
			"Empty or unparseable calendardata/carddata is skipped with a warning.\n\n" +
			"Calendar sharing (<prefix>calendarshares, or <prefix>dav_shares on newer\n" +
			"Nextcloud) is NOT imported in v1 — invite-state mapping is out of scope;\n" +
			"affected rows are counted and reported once, and calendars must be\n" +
			"re-shared after migration.\n\n" +
			"The import is idempotent: an existing calendar/addressbook with the same\n" +
			"owner+uri is skipped (an existing one with different properties is a\n" +
			"collision and is skipped wholesale, objects included, with a warning),\n" +
			"and existing objects are skipped by uri, so an interrupted run can simply\n" +
			"be repeated. With --dry-run everything is scanned and counted but nothing\n" +
			"is written.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			dst, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = dst.Close() }()
			src, err := openSourceDB(ctx, f)
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			r := newImportReport()
			err = importNCDAV(ctx, src, users.NewSQLStore(dst), calendar.NewSQLStore(dst),
				contacts.NewSQLStore(dst), f.tablePrefix, f.dryRun, r)
			if err != nil {
				return err
			}
			return r.print(cmd.OutOrStdout(), f.dryRun)
		},
	}
}

// importNCDAV imports calendars + objects and address books + cards from the
// PHP Nextcloud database. Best-effort: per-row failures warn and continue.
func importNCDAV(ctx context.Context, src database.DB, us *users.SQLStore, cs *calendar.SQLStore, bs *contacts.SQLStore, prefix string, dryRun bool, r *importReport) error {
	if err := importNCCalendars(ctx, src, us, cs, prefix, dryRun, r); err != nil {
		return err
	}
	if err := importNCAddressbooks(ctx, src, us, bs, prefix, dryRun, r); err != nil {
		return err
	}
	return warnNCCalendarShares(ctx, src, prefix, r)
}

// ncPrincipalUID extracts the uid from a principals/users/<uid> principaluri.
func ncPrincipalUID(principal string) (string, bool) {
	uid, ok := strings.CutPrefix(principal, ncUserPrincipalPrefix)
	if !ok || uid == "" || strings.Contains(uid, "/") {
		return "", false
	}
	return uid, true
}

// ncCalendarRow is one row of oc_calendars with NULLs collapsed to "".
type ncCalendarRow struct {
	id          int64
	principal   string
	uri         string
	displayName string
	description string
	order       int
	color       string
	timezone    string
	components  string
	transparent int
}

func importNCCalendars(ctx context.Context, src database.DB, us *users.SQLStore, cs *calendar.SQLStore, prefix string, dryRun bool, r *importReport) error {
	cc := r.entity("calendars")
	rows, err := src.Query(ctx, `
SELECT id, principaluri, uri, displayname, description, calendarorder, calendarcolor, timezone, components, transparent
FROM `+prefix+`calendars ORDER BY id`)
	if err != nil {
		return fmt.Errorf("import dav: read source calendars: %w", err)
	}
	defer rows.Close()
	componentsDropped, transparentDropped := 0, 0
	for rows.Next() {
		var row ncCalendarRow
		var display, description, color, timezone, components *string
		if err := rows.Scan(&row.id, &row.principal, &row.uri, &display, &description,
			&row.order, &color, &timezone, &components, &row.transparent); err != nil {
			return fmt.Errorf("import dav: scan calendar: %w", err)
		}
		row.displayName = derefStr(display)
		row.description = derefStr(description)
		row.color = derefStr(color)
		row.timezone = derefStr(timezone)
		row.components = derefStr(components)
		if row.components != "" {
			componentsDropped++
		}
		if row.transparent != 0 {
			transparentDropped++
		}
		importNCCalendar(ctx, src, &row, us, cs, prefix, dryRun, r, cc)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import dav: read source calendars: %w", err)
	}
	if componentsDropped > 0 {
		r.warn("%d calendar components list(s) not imported (ncgo has no components field)", componentsDropped)
	}
	if transparentDropped > 0 {
		r.warn("%d transparent calendar flag(s) not imported (ncgo has no transparent field)", transparentDropped)
	}
	return nil
}

// importNCCalendar imports one calendar and — unless the calendar is skipped
// wholesale — its objects.
func importNCCalendar(ctx context.Context, src database.DB, row *ncCalendarRow, us *users.SQLStore, cs *calendar.SQLStore, prefix string, dryRun bool, r *importReport, cc *importCounts) {
	uid, ok := ncPrincipalUID(row.principal)
	if !ok {
		r.warn("calendar %d: principal %q is not a user principal, skipped with its objects", row.id, row.principal)
		cc.skipped++
		skipNCObjects(ctx, src, prefix+`calendarobjects`, "calendarid", row.id, r.entity("calendar objects"), r)
		return
	}
	owner, err := us.GetByUID(ctx, uid)
	switch {
	case err == nil:
	case errors.Is(err, users.ErrNotFound):
		r.warn("calendar %d: owner %s not in target (run 'import-nextcloud users' first), skipped with its objects", row.id, uid)
		cc.skipped++
		skipNCObjects(ctx, src, prefix+`calendarobjects`, "calendarid", row.id, r.entity("calendar objects"), r)
		return
	default:
		r.warn("calendar %d: lookup owner %s: %v", row.id, uid, err)
		cc.failed++
		return
	}
	want := &calendar.Calendar{
		UserID:      owner.ID,
		URI:         row.uri,
		DisplayName: row.displayName,
		Description: row.description,
		Color:       row.color,
		Order:       row.order,
		Timezone:    row.timezone,
		Enabled:     true,
	}
	existing, err := cs.GetCalendarByURI(ctx, owner.ID, row.uri)
	switch {
	case err == nil:
		cc.skipped++
		if !equivalentNCCalendar(existing, want) {
			// A different calendar already owns this uri; never merge
			// foreign objects into it.
			r.warn("calendar %d: uri %q already exists for %s with different properties, skipped with its objects", row.id, row.uri, uid)
			skipNCObjects(ctx, src, prefix+`calendarobjects`, "calendarid", row.id, r.entity("calendar objects"), r)
			return
		}
		// Idempotent re-run (or resume after a partial failure): import
		// whichever objects are still missing.
	case errors.Is(err, calendar.ErrNotFound):
		if dryRun {
			cc.created++
			countNCObjects(ctx, src, prefix+`calendarobjects`, "calendarid", "calendardata", row.id, r.entity("calendar objects"), r)
			return
		}
		if cerr := cs.CreateCalendar(ctx, want); cerr != nil {
			if errors.Is(cerr, calendar.ErrExists) {
				cc.skipped++
				return
			}
			r.warn("calendar %d: create failed: %v", row.id, cerr)
			cc.failed++
			return
		}
		cc.created++
	default:
		r.warn("calendar %d: lookup uri %q: %v", row.id, row.uri, err)
		cc.failed++
		return
	}
	importNCCalendarObjects(ctx, src, row.id, cs, owner.ID, row.uri, prefix, dryRun, r)
}

// equivalentNCCalendar reports whether the existing target calendar carries
// the same mapped properties as the source row, distinguishing an idempotent
// re-run from a uri collision with an unrelated calendar. DisplayName and
// Color are compared after the store's defaults are applied (empty source
// values default to the uri and DefaultCalendarColor on create).
func equivalentNCCalendar(existing, want *calendar.Calendar) bool {
	display, color := want.DisplayName, want.Color
	if display == "" {
		display = want.URI
	}
	if color == "" {
		color = calendar.DefaultCalendarColor
	}
	return existing.DisplayName == display &&
		existing.Description == want.Description &&
		existing.Color == color &&
		existing.Order == want.Order &&
		existing.Timezone == want.Timezone
}

// importNCCalendarObjects imports the oc_calendarobjects rows of one source
// calendar through the production PutObject path.
func importNCCalendarObjects(ctx context.Context, src database.DB, srcCalID int64, cs *calendar.SQLStore, userID int64, calURI, prefix string, dryRun bool, r *importReport) {
	oc := r.entity("calendar objects")
	rows, err := src.Query(ctx, `
SELECT id, uri, calendardata FROM `+prefix+`calendarobjects WHERE calendarid = ? ORDER BY id`, srcCalID)
	if err != nil {
		r.warn("calendar objects of source calendar %d: read failed: %v", srcCalID, err)
		oc.failed++
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var uri string
		var data []byte
		if err := rows.Scan(&id, &uri, &data); err != nil {
			r.warn("calendar objects of source calendar %d: scan failed: %v", srcCalID, err)
			oc.failed++
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			r.warn("calendar object %d (%s): empty calendardata, skipped", id, uri)
			oc.skipped++
			continue
		}
		if _, err := cs.GetObject(ctx, userID, calURI, uri); err == nil {
			oc.skipped++
			continue
		} else if !errors.Is(err, calendar.ErrNotFound) {
			r.warn("calendar object %d (%s): lookup failed: %v", id, uri, err)
			oc.failed++
			continue
		}
		if dryRun {
			oc.created++
			continue
		}
		_, err := cs.PutObject(ctx, userID, calURI, &calendar.Object{URI: uri, Data: data})
		switch {
		case err == nil:
			oc.created++
		case errors.Is(err, calendar.ErrInvalid), errors.Is(err, calendar.ErrUnsupported):
			r.warn("calendar object %d (%s): not importable (%v), skipped", id, uri, err)
			oc.skipped++
		case errors.Is(err, calendar.ErrConflict):
			r.warn("calendar object %d (%s): UID already used by another object, skipped", id, uri)
			oc.skipped++
		default:
			r.warn("calendar object %d (%s): import failed: %v", id, uri, err)
			oc.failed++
		}
	}
	if err := rows.Err(); err != nil {
		r.warn("calendar objects of source calendar %d: read failed: %v", srcCalID, err)
		oc.failed++
	}
}

// ncAddressbookRow is one row of oc_addressbooks with NULLs collapsed to "".
type ncAddressbookRow struct {
	id          int64
	principal   string
	uri         string
	displayName string
	description string
}

func importNCAddressbooks(ctx context.Context, src database.DB, us *users.SQLStore, bs *contacts.SQLStore, prefix string, dryRun bool, r *importReport) error {
	ac := r.entity("addressbooks")
	rows, err := src.Query(ctx, `
SELECT id, principaluri, uri, displayname, description
FROM `+prefix+`addressbooks ORDER BY id`)
	if err != nil {
		return fmt.Errorf("import dav: read source addressbooks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row ncAddressbookRow
		var display, description *string
		if err := rows.Scan(&row.id, &row.principal, &row.uri, &display, &description); err != nil {
			return fmt.Errorf("import dav: scan addressbook: %w", err)
		}
		row.displayName = derefStr(display)
		row.description = derefStr(description)
		importNCAddressbook(ctx, src, &row, us, bs, prefix, dryRun, r, ac)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import dav: read source addressbooks: %w", err)
	}
	return nil
}

func importNCAddressbook(ctx context.Context, src database.DB, row *ncAddressbookRow, us *users.SQLStore, bs *contacts.SQLStore, prefix string, dryRun bool, r *importReport, ac *importCounts) {
	uid, ok := ncPrincipalUID(row.principal)
	if !ok {
		r.warn("addressbook %d: principal %q is not a user principal, skipped with its cards", row.id, row.principal)
		ac.skipped++
		skipNCObjects(ctx, src, prefix+`cards`, "addressbookid", row.id, r.entity("cards"), r)
		return
	}
	owner, err := us.GetByUID(ctx, uid)
	switch {
	case err == nil:
	case errors.Is(err, users.ErrNotFound):
		r.warn("addressbook %d: owner %s not in target (run 'import-nextcloud users' first), skipped with its cards", row.id, uid)
		ac.skipped++
		skipNCObjects(ctx, src, prefix+`cards`, "addressbookid", row.id, r.entity("cards"), r)
		return
	default:
		r.warn("addressbook %d: lookup owner %s: %v", row.id, uid, err)
		ac.failed++
		return
	}
	want := &contacts.Addressbook{
		UserID:      owner.ID,
		URI:         row.uri,
		DisplayName: row.displayName,
		Description: row.description,
		Enabled:     true,
	}
	existing, err := bs.GetBookByURI(ctx, owner.ID, row.uri)
	switch {
	case err == nil:
		ac.skipped++
		display := want.DisplayName
		if display == "" {
			display = want.URI
		}
		if existing.DisplayName != display || existing.Description != want.Description {
			r.warn("addressbook %d: uri %q already exists for %s with different properties, skipped with its cards", row.id, row.uri, uid)
			skipNCObjects(ctx, src, prefix+`cards`, "addressbookid", row.id, r.entity("cards"), r)
			return
		}
	case errors.Is(err, contacts.ErrNotFound):
		if dryRun {
			ac.created++
			countNCObjects(ctx, src, prefix+`cards`, "addressbookid", "carddata", row.id, r.entity("cards"), r)
			return
		}
		if cerr := bs.CreateBook(ctx, want); cerr != nil {
			if errors.Is(cerr, contacts.ErrExists) {
				ac.skipped++
				return
			}
			r.warn("addressbook %d: create failed: %v", row.id, cerr)
			ac.failed++
			return
		}
		ac.created++
	default:
		r.warn("addressbook %d: lookup uri %q: %v", row.id, row.uri, err)
		ac.failed++
		return
	}
	importNCCards(ctx, src, row.id, bs, owner.ID, row.uri, prefix, dryRun, r)
}

// importNCCards imports the oc_cards rows of one source address book through
// the production PutObject path.
func importNCCards(ctx context.Context, src database.DB, srcBookID int64, bs *contacts.SQLStore, userID int64, bookURI, prefix string, dryRun bool, r *importReport) {
	kc := r.entity("cards")
	rows, err := src.Query(ctx, `
SELECT id, uri, carddata FROM `+prefix+`cards WHERE addressbookid = ? ORDER BY id`, srcBookID)
	if err != nil {
		r.warn("cards of source addressbook %d: read failed: %v", srcBookID, err)
		kc.failed++
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var uri string
		var data []byte
		if err := rows.Scan(&id, &uri, &data); err != nil {
			r.warn("cards of source addressbook %d: scan failed: %v", srcBookID, err)
			kc.failed++
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			r.warn("card %d (%s): empty carddata, skipped", id, uri)
			kc.skipped++
			continue
		}
		if _, err := bs.GetObject(ctx, userID, bookURI, uri); err == nil {
			kc.skipped++
			continue
		} else if !errors.Is(err, contacts.ErrNotFound) {
			r.warn("card %d (%s): lookup failed: %v", id, uri, err)
			kc.failed++
			continue
		}
		if dryRun {
			kc.created++
			continue
		}
		_, err := bs.PutObject(ctx, userID, bookURI, &contacts.Object{URI: uri, Data: data})
		switch {
		case err == nil:
			kc.created++
		case errors.Is(err, contacts.ErrInvalid):
			r.warn("card %d (%s): not importable (%v), skipped", id, uri, err)
			kc.skipped++
		case errors.Is(err, contacts.ErrConflict):
			r.warn("card %d (%s): UID already used by another card, skipped", id, uri)
			kc.skipped++
		default:
			r.warn("card %d (%s): import failed: %v", id, uri, err)
			kc.failed++
		}
	}
	if err := rows.Err(); err != nil {
		r.warn("cards of source addressbook %d: read failed: %v", srcBookID, err)
		kc.failed++
	}
}

// skipNCObjects counts all objects of a wholesale-skipped collection as
// skipped so the report reflects every source row.
func skipNCObjects(ctx context.Context, src database.DB, table, column string, parentID int64, c *importCounts, r *importReport) {
	n := countNCSourceRows(ctx, src, table, column, parentID, r)
	c.skipped += n
}

// countNCObjects counts the objects a dry-run would create (all non-empty
// rows of the collection).
func countNCObjects(ctx context.Context, src database.DB, table, column, dataColumn string, parentID int64, c *importCounts, r *importReport) {
	rows, err := src.Query(ctx, `SELECT `+dataColumn+` FROM `+table+` WHERE `+column+` = ?`, parentID)
	if err != nil {
		r.warn("dry-run: count objects of %s %d: %v", table, parentID, err)
		c.failed++
		return
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			r.warn("dry-run: scan object of %s %d: %v", table, parentID, err)
			c.failed++
			continue
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			c.skipped++
			continue
		}
		c.created++
	}
	if err := rows.Err(); err != nil {
		r.warn("dry-run: count objects of %s %d: %v", table, parentID, err)
		c.failed++
	}
}

func countNCSourceRows(ctx context.Context, src database.DB, table, column string, parentID int64, r *importReport) int {
	var n int
	if err := src.QueryRow(ctx, `SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, parentID).Scan(&n); err != nil {
		r.warn("count rows of %s %d: %v", table, parentID, err)
		return 0
	}
	return n
}

// warnNCCalendarShares counts source calendar-share rows without importing
// them (invite-state mapping is out of scope in v1) and reports the deferral
// once. Newer Nextcloud versions store calendar shares in dav_shares instead
// of calendarshares; whichever table exists is counted.
func warnNCCalendarShares(ctx context.Context, src database.DB, prefix string, r *importReport) error {
	for _, table := range []string{prefix + "calendarshares", prefix + "dav_shares"} {
		var n int
		if err := src.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			continue
		}
		if n > 0 {
			r.entity("calendar shares").skipped += n
			r.warn("%d calendar share(s) in %s not imported (invite-state mapping out of scope; re-share calendars after migration)", n, table)
		}
		return nil
	}
	return nil
}
