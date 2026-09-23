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
		Short: "Import calendars, objects, calendar shares, address books, and contacts",
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
			"Calendar sharing (<prefix>dav_shares on all supported Nextcloud versions,\n" +
			"legacy <prefix>calendarshares also recognized) IS imported for the\n" +
			"unambiguous subset: user principals whose sharee and target calendar\n" +
			"exist in the target, access read (3) or read-write (2) — Nextcloud keeps\n" +
			"no invite state for these rows, and ncgo shares take effect immediately\n" +
			"too, so the mapping is faithful. Group/circle principals, unknown\n" +
			"sharees, calendars that were not imported, and unmappable access values\n" +
			"are skipped with per-row warnings. Address book shares are counted and\n" +
			"reported but NOT imported (ncgo has no address book sharing).\n\n" +
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
	resolved, err := importNCCalendars(ctx, src, us, cs, prefix, dryRun, r)
	if err != nil {
		return err
	}
	if err := importNCAddressbooks(ctx, src, us, bs, prefix, dryRun, r); err != nil {
		return err
	}
	return importNCCalendarShares(ctx, src, us, cs, prefix, dryRun, r, resolved)
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

// ncResolvedCalendar records how one source calendar landed in the target:
// it was created (or dry-run-planned, targetID 0) or an equivalent calendar
// already owned the uri. Shares resolve against this set, so a share never
// attaches to a foreign calendar that merely shares the uri (collision skip).
type ncResolvedCalendar struct {
	ownerUID string
	uri      string
	targetID int64
}

func importNCCalendars(ctx context.Context, src database.DB, us *users.SQLStore, cs *calendar.SQLStore, prefix string, dryRun bool, r *importReport) (map[int64]ncResolvedCalendar, error) {
	cc := r.entity("calendars")
	resolved := map[int64]ncResolvedCalendar{}
	rows, err := src.Query(ctx, `
SELECT id, principaluri, uri, displayname, description, calendarorder, calendarcolor, timezone, components, transparent
FROM `+prefix+`calendars ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("import dav: read source calendars: %w", err)
	}
	defer rows.Close()
	componentsDropped, transparentDropped := 0, 0
	for rows.Next() {
		var row ncCalendarRow
		var display, description, color, timezone, components *string
		if err := rows.Scan(&row.id, &row.principal, &row.uri, &display, &description,
			&row.order, &color, &timezone, &components, &row.transparent); err != nil {
			return nil, fmt.Errorf("import dav: scan calendar: %w", err)
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
		importNCCalendar(ctx, src, &row, us, cs, prefix, dryRun, r, cc, resolved)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("import dav: read source calendars: %w", err)
	}
	if componentsDropped > 0 {
		r.warn("%d calendar components list(s) not imported (ncgo has no components field)", componentsDropped)
	}
	if transparentDropped > 0 {
		r.warn("%d transparent calendar flag(s) not imported (ncgo has no transparent field)", transparentDropped)
	}
	return resolved, nil
}

// importNCCalendar imports one calendar and — unless the calendar is skipped
// wholesale — its objects. Calendars that land in the target (created,
// dry-run-planned, or matched to an equivalent existing one) are recorded in
// resolved for the share pass.
func importNCCalendar(ctx context.Context, src database.DB, row *ncCalendarRow, us *users.SQLStore, cs *calendar.SQLStore, prefix string, dryRun bool, r *importReport, cc *importCounts, resolved map[int64]ncResolvedCalendar) {
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
		resolved[row.id] = ncResolvedCalendar{ownerUID: uid, uri: row.uri, targetID: existing.ID}
	case errors.Is(err, calendar.ErrNotFound):
		if dryRun {
			cc.created++
			resolved[row.id] = ncResolvedCalendar{ownerUID: uid, uri: row.uri}
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
		resolved[row.id] = ncResolvedCalendar{ownerUID: uid, uri: row.uri, targetID: want.ID}
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

// ncCalShareRow is one calendar-share row from either source share table:
// <prefix>dav_shares (Nextcloud 9+) or the legacy <prefix>calendarshares
// layout (same mapping, calendarid column read as resource, type implied
// "calendar").
type ncCalShareRow struct {
	table     string
	id        int64
	principal string
	typ       string
	access    int64
	resource  int64 // oc_calendars.id
}

// ncAccessRead / ncAccessReadWrite are Nextcloud's dav_shares access values
// (apps/dav/lib/DAV/Sharing/Backend.php): 1 = owner (never a shareable row
// meaning), 2 = read-write, 3 = read.
const (
	ncAccessOwner     = 1
	ncAccessReadWrite = 2
	ncAccessRead      = 3
)

// readNCShareRows reads both share tables when present (a source migrated
// across many Nextcloud versions can carry both) and returns the combined
// row set. Absent tables are skipped silently; with no rows at all the
// caller reports nothing.
func readNCShareRows(ctx context.Context, src database.DB, prefix string) []ncCalShareRow {
	var out []ncCalShareRow
	if rows, err := src.Query(ctx, `
SELECT id, principaluri, type, access, resourceid FROM `+prefix+`dav_shares ORDER BY id`); err == nil {
		out = append(out, scanNCShareRows(rows, prefix+"dav_shares", false)...)
	}
	if rows, err := src.Query(ctx, `
SELECT id, principaluri, access, calendarid FROM `+prefix+`calendarshares ORDER BY id`); err == nil {
		out = append(out, scanNCShareRows(rows, prefix+"calendarshares", true)...)
	}
	return out
}

func scanNCShareRows(rows database.Rows, table string, legacy bool) []ncCalShareRow {
	defer rows.Close()
	var out []ncCalShareRow
	for rows.Next() {
		var row ncCalShareRow
		var typ *string
		var access *int64
		row.table = table
		var err error
		if legacy {
			err = rows.Scan(&row.id, &row.principal, &access, &row.resource)
		} else {
			err = rows.Scan(&row.id, &row.principal, &typ, &access, &row.resource)
		}
		if err != nil {
			continue
		}
		row.typ = "calendar"
		if typ != nil && *typ != "" {
			row.typ = *typ
		}
		if access != nil {
			row.access = *access
		}
		out = append(out, row)
	}
	return out
}

// importNCCalendarShares maps source calendar shares onto ncgo
// calendar_shares. Only the unambiguous subset is imported: user principals
// (groups/circles skipped), sharee present in the target, and a target
// calendar that this run (or a previous one) actually imported — resolved
// holds exactly those, so a share never attaches to a foreign calendar that
// merely owns the same uri. Nextcloud's dav_shares carries no invite state —
// every row is an effective share (Backend.php hardcodes status accepted),
// and ncgo shares take effect immediately as well, so the mapping is
// faithful. Idempotent: an existing share with the same access is skipped, a
// differing access is updated (UpsertCalendarShare semantics).
func importNCCalendarShares(ctx context.Context, src database.DB, us *users.SQLStore, cs *calendar.SQLStore, prefix string, dryRun bool, r *importReport, resolved map[int64]ncResolvedCalendar) error {
	rows := readNCShareRows(ctx, src, prefix)
	if len(rows) == 0 {
		return nil
	}
	sc := r.entity("calendar shares")
	cals, err := readNCCalendarRefs(ctx, src, prefix)
	if err != nil {
		return err
	}
	// Per-sharee existing shares, lazily loaded and kept in sync with what
	// this run writes (dry-run included) so duplicate source rows and re-runs
	// count exactly.
	existing := map[int64]map[int64]string{}
	var addressbookShares int
	for _, row := range rows {
		if row.typ != "calendar" {
			if row.typ == "addressbook" {
				addressbookShares++
				r.entity("addressbook shares").skipped++
			} else {
				r.warn("calendar share %s:%d: unknown type %q, skipped", row.table, row.id, row.typ)
				sc.skipped++
			}
			continue
		}
		importNCCalendarShare(ctx, row, cals, resolved, us, cs, existing, dryRun, r, sc)
	}
	if addressbookShares > 0 {
		r.warn("%d addressbook share(s) not imported (ncgo has no addressbook sharing; re-share address books after migration)", addressbookShares)
	}
	return nil
}

// importNCCalendarShare maps one source share row; every skip category warns.
func importNCCalendarShare(ctx context.Context, row ncCalShareRow, cals map[int64]ncCalendarRef, resolved map[int64]ncResolvedCalendar, us *users.SQLStore, cs *calendar.SQLStore, existing map[int64]map[int64]string, dryRun bool, r *importReport, sc *importCounts) {
	shareeUID, ok := ncPrincipalUID(row.principal)
	if !ok {
		r.warn("calendar share %s:%d: principal %q is not a user principal, skipped", row.table, row.id, row.principal)
		sc.skipped++
		return
	}
	sharee, err := us.GetByUID(ctx, shareeUID)
	switch {
	case err == nil:
	case errors.Is(err, users.ErrNotFound):
		r.warn("calendar share %s:%d: sharee %s not in target, skipped", row.table, row.id, shareeUID)
		sc.skipped++
		return
	default:
		r.warn("calendar share %s:%d: lookup sharee %s: %v", row.table, row.id, shareeUID, err)
		sc.failed++
		return
	}
	if _, ok := cals[row.resource]; !ok {
		r.warn("calendar share %s:%d: source calendar %d not found, skipped", row.table, row.id, row.resource)
		sc.skipped++
		return
	}
	target, ok := resolved[row.resource]
	if !ok {
		r.warn("calendar share %s:%d: calendar %s not imported, share skipped", row.table, row.id, ncCalendarLabel(cals, row.resource))
		sc.skipped++
		return
	}
	if target.ownerUID == shareeUID {
		r.warn("calendar share %s:%d: %s is the calendar owner, self-share skipped", row.table, row.id, shareeUID)
		sc.skipped++
		return
	}
	var access string
	switch row.access {
	case ncAccessRead:
		access = calendar.ShareAccessRead
	case ncAccessReadWrite:
		access = calendar.ShareAccessReadWrite
	default:
		r.warn("calendar share %s:%d: access %d not mappable (2=read-write, 3=read), skipped", row.table, row.id, row.access)
		sc.skipped++
		return
	}
	if target.targetID == 0 {
		// Dry-run with the calendar only planned: no shares can exist yet.
		sc.created++
		return
	}
	byCal, loaded := existing[sharee.ID]
	if !loaded {
		byCal = map[int64]string{}
		shared, err := cs.ListSharedCalendars(ctx, sharee.ID)
		if err != nil {
			r.warn("calendar share %s:%d: list shares of %s: %v", row.table, row.id, shareeUID, err)
			sc.failed++
			return
		}
		for _, s := range shared {
			byCal[s.ID] = s.Access
		}
		existing[sharee.ID] = byCal
	}
	cur, found := byCal[target.targetID]
	switch {
	case found && cur == access:
		sc.skipped++
		return
	case found && !dryRun:
		if err := cs.UpsertCalendarShare(ctx, target.targetID, sharee.ID, access); err != nil {
			r.warn("calendar share %s:%d: update failed: %v", row.table, row.id, err)
			sc.failed++
			return
		}
		sc.updated++
	case found:
		sc.updated++
	case !dryRun:
		if err := cs.UpsertCalendarShare(ctx, target.targetID, sharee.ID, access); err != nil {
			r.warn("calendar share %s:%d: create failed: %v", row.table, row.id, err)
			sc.failed++
			return
		}
		sc.created++
	default:
		sc.created++
	}
	byCal[target.targetID] = access
}

// ncCalendarLabel renders source calendar id as owner/uri for warnings.
func ncCalendarLabel(cals map[int64]ncCalendarRef, id int64) string {
	ref := cals[id]
	uid := ref.principal
	if u, ok := ncPrincipalUID(ref.principal); ok {
		uid = u
	}
	return uid + "/" + ref.uri
}

// ncCalendarRef is the owner+uri of one source calendar, used to resolve
// share resourceids against the calendars this run (or a previous one)
// imported.
type ncCalendarRef struct {
	principal string
	uri       string
}

func readNCCalendarRefs(ctx context.Context, src database.DB, prefix string) (map[int64]ncCalendarRef, error) {
	rows, err := src.Query(ctx, `SELECT id, principaluri, uri FROM `+prefix+`calendars ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("import dav: read source calendars for shares: %w", err)
	}
	defer rows.Close()
	out := map[int64]ncCalendarRef{}
	for rows.Next() {
		var id int64
		var ref ncCalendarRef
		if err := rows.Scan(&id, &ref.principal, &ref.uri); err != nil {
			return nil, fmt.Errorf("import dav: scan calendar for shares: %w", err)
		}
		out[id] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("import dav: read source calendars for shares: %w", err)
	}
	return out, nil
}
