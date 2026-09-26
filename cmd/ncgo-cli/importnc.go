package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// noPasswordSentinel is stored for users whose source password hash cannot be
// imported. It is not a parseable PHC string, so it never verifies.
const noPasswordSentinel = "!"

// importNCFlags holds the persistent flags of import-nextcloud, shared by the
// 4e-series subcommands (users, files, shares, dav).
type importNCFlags struct {
	sourceDriver string
	sourceDSN    string
	tablePrefix  string
	dryRun       bool
}

func newImportNextcloud() *cobra.Command {
	f := &importNCFlags{}
	cmd := &cobra.Command{
		Use:   "import-nextcloud",
		Short: "Import data from a PHP Nextcloud database",
		Long: "Import data from an existing PHP Nextcloud instance into the configured\n" +
			"ncgo database, reading the source oc_* tables directly.\n\n" +
			"The import is phased; each subcommand imports one entity family and is\n" +
			"idempotent and resumable (existing rows are skipped, so an interrupted\n" +
			"run can simply be repeated):\n" +
			"  users   user accounts (with email and quota), groups, and memberships\n" +
			"  files   user files from a Nextcloud data directory\n" +
			"  shares  internal and public-link shares\n" +
			"  dav     calendars, calendar objects, calendar shares, address books,\n" +
			"          and contacts\n" +
			"  tokens  app passwords (permanent authtokens)\n\n" +
			"Sessions are NOT imported: ncgo browser sessions are its own token\n" +
			"family, so users must log in again. App passwords (oc_authtoken rows\n" +
			"with type = 1) are imported by the 'tokens' subcommand — they verify\n" +
			"only when ncgo's instance.secret equals the source instance's\n" +
			"config.php 'secret' (see the tokens command help).",
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			switch database.Dialect(f.sourceDriver) {
			case database.DialectMySQL, database.DialectPostgres, database.DialectSQLite:
			default:
				return fmt.Errorf("ncgo-cli: --source-driver must be mysql, postgres, or sqlite")
			}
			if f.sourceDSN == "" {
				return fmt.Errorf("ncgo-cli: --source-dsn is required")
			}
			if !validTablePrefix(f.tablePrefix) {
				return fmt.Errorf("ncgo-cli: --table-prefix %q must contain only letters, digits, and underscores", f.tablePrefix)
			}
			return nil
		},
	}
	cmd.PersistentFlags().StringVar(&f.sourceDriver, "source-driver", "", "source database driver (mysql|postgres|sqlite)")
	cmd.PersistentFlags().StringVar(&f.sourceDSN, "source-dsn", "", "source database DSN (required)")
	cmd.PersistentFlags().StringVar(&f.tablePrefix, "table-prefix", "oc_", "source table prefix")
	cmd.PersistentFlags().BoolVar(&f.dryRun, "dry-run", false, "scan and count without writing to the target")
	cmd.AddCommand(newImportNCUsers(f), newImportNCFiles(f), newImportNCShares(f), newImportNCDAV(f), newImportNCTokens(f))
	return cmd
}

func validTablePrefix(prefix string) bool {
	if prefix == "" {
		return false
	}
	for _, r := range prefix {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}

// openSourceDB opens the PHP Nextcloud source database; the caller closes it.
// This is a separate connection from the configured ncgo target database.
func openSourceDB(ctx context.Context, f *importNCFlags) (database.DB, error) {
	return database.Open(ctx, database.Config{
		Driver: database.Dialect(f.sourceDriver),
		DSN:    f.sourceDSN,
	})
}

// importReport accumulates per-entity counts and warnings for an import run.
type importReport struct {
	entities []string
	counts   map[string]*importCounts
	warnings []string
	extra    int
}

type importCounts struct {
	created int
	updated int
	skipped int
	failed  int
}

const maxPrintedWarnings = 20

func newImportReport() *importReport {
	return &importReport{counts: map[string]*importCounts{}}
}

// entity returns the counters for name, registering it on first use.
func (r *importReport) entity(name string) *importCounts {
	c, ok := r.counts[name]
	if !ok {
		c = &importCounts{}
		r.counts[name] = c
		r.entities = append(r.entities, name)
	}
	return c
}

// warn records a warning; at most maxPrintedWarnings are kept for printing,
// the rest are counted.
func (r *importReport) warn(format string, args ...any) {
	if len(r.warnings) < maxPrintedWarnings {
		r.warnings = append(r.warnings, fmt.Sprintf(format, args...))
		return
	}
	r.extra++
}

// print writes the summary and warnings.
func (r *importReport) print(w io.Writer, dryRun bool) error {
	for _, name := range r.entities {
		c := r.counts[name]
		updated := ""
		if c.updated > 0 {
			updated = fmt.Sprintf(", %d updated", c.updated)
		}
		if _, err := fmt.Fprintf(w, "%s: %d created%s, %d skipped (existing), %d failed\n",
			name, c.created, updated, c.skipped, c.failed); err != nil {
			return err
		}
	}
	for _, msg := range r.warnings {
		if _, err := fmt.Fprintf(w, "warning: %s\n", msg); err != nil {
			return err
		}
	}
	if r.extra > 0 {
		if _, err := fmt.Fprintf(w, "... and %d more warnings\n", r.extra); err != nil {
			return err
		}
	}
	if dryRun {
		if _, err := fmt.Fprintln(w, "dry-run: no changes written"); err != nil {
			return err
		}
	}
	return nil
}

func newImportNCUsers(f *importNCFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "users",
		Short: "Import users, groups, and group memberships",
		Long: "Import users, groups, and group memberships from the source\n" +
			"<prefix>users, <prefix>groups, and <prefix>group_user tables.\n\n" +
			"Password policy: standard argon2id PHC hashes ($argon2id$v=19$m=...,t=...,p=...)\n" +
			"are imported verbatim — PHP's password_hash output is the same format ncgo's\n" +
			"verifier parses, so those users keep their passwords. Any other hash format\n" +
			"(bcrypt $2y$, argon2i, legacy, or empty) is replaced with a sentinel that\n" +
			"never verifies, a warning is printed, and the user must reset their password.\n\n" +
			"Source uids that do not satisfy ncgo's uid rules (non-empty, no whitespace,\n" +
			"control characters, '/', or '\\\\') fall back to uid_lower; if neither is\n" +
			"valid the user is skipped with a warning.\n\n" +
			"Email and quota are mapped from <prefix>preferences (settings/primary_email,\n" +
			"settings/email, files/quota) and — as an email fallback — the\n" +
			"<prefix>accounts JSON blob. Quota values \"none\", \"default\", and empty map\n" +
			"to no quota (ncgo has no instance default); human-readable sizes like\n" +
			"\"5 GB\" or \"512 MB\" convert to bytes (1024-based, as Nextcloud computes\n" +
			"them); unparseable values import without a quota plus a warning.\n\n" +
			"Existing users, groups, and memberships in the target are skipped unchanged,\n" +
			"so the import is idempotent and resumable. Writes are committed per entity.\n" +
			"Sessions are NOT imported; app passwords are imported by the 'tokens'\n" +
			"subcommand (see its help).",
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
			userStore := users.NewSQLStore(dst)
			// ADR-0099: with per-user keys on, imported users get their UK
			// minted eagerly via the lifecycle hook, exactly as locally
			// created accounts do.
			hook, err := userKeysHook(cfg, dst)
			if err != nil {
				return err
			}
			userStore.UserKeys = hook
			if err := importNCUsers(ctx, src, userStore, f.tablePrefix, f.dryRun, r); err != nil {
				return err
			}
			return r.print(cmd.OutOrStdout(), f.dryRun)
		},
	}
}

// validNCImportUID reports whether uid satisfies ncgo's uid rules: non-empty
// and free of whitespace, control characters, '/', and '\', which break the
// DAV URL and file-path contexts uids appear in.
func validNCImportUID(uid string) bool {
	if uid == "" {
		return false
	}
	for _, r := range uid {
		if r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// importNCUsers imports users, groups, and memberships from src (the PHP
// Nextcloud database) into dst (the ncgo store). Writes are committed per
// entity; existing rows are skipped, so runs are idempotent and resumable.
// With dryRun, the source is fully scanned and counted but nothing is written.
func importNCUsers(ctx context.Context, src database.DB, dst *users.SQLStore, prefix string, dryRun bool, r *importReport) error {
	mapped, plannedUsers, err := importNCUserRows(ctx, src, dst, prefix, dryRun, r)
	if err != nil {
		return err
	}
	plannedGroups, err := importNCGroupRows(ctx, src, dst, prefix, dryRun, r)
	if err != nil {
		return err
	}
	return importNCMemberRows(ctx, src, dst, prefix, dryRun, r, mapped, plannedUsers, plannedGroups)
}

// importNCUserRows imports <prefix>users and returns the source-uid →
// target-uid mapping plus the set of target uids planned (dry-run only).
func importNCUserRows(ctx context.Context, src database.DB, dst *users.SQLStore, prefix string, dryRun bool, r *importReport) (map[string]string, map[string]bool, error) {
	uc := r.entity("users")
	mapped := map[string]string{}
	planned := map[string]bool{}
	extras := readNCUserExtras(ctx, src, prefix, r)
	rows, err := src.Query(ctx, `SELECT uid, uid_lower, displayname, password FROM `+prefix+`users ORDER BY uid`)
	if err != nil {
		return nil, nil, fmt.Errorf("import users: read source users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var uid, uidLower string
		var display, password *string
		if err := rows.Scan(&uid, &uidLower, &display, &password); err != nil {
			return nil, nil, fmt.Errorf("import users: scan user: %w", err)
		}
		target := uid
		if !validNCImportUID(target) {
			target = uidLower
			if !validNCImportUID(target) {
				r.warn("user %s: uid not valid for ncgo, skipped", uid)
				uc.skipped++
				continue
			}
			r.warn("user %s: uid not valid for ncgo, imported as %s", uid, target)
		}
		mapped[uid] = target
		if _, err := dst.GetByUID(ctx, target); err == nil {
			uc.skipped++
			continue
		} else if !errors.Is(err, users.ErrNotFound) {
			return nil, nil, fmt.Errorf("import users: lookup %s: %w", target, err)
		}
		hash := ""
		if password != nil {
			hash = *password
		}
		if !isArgon2idPHC(hash) {
			hash = noPasswordSentinel
			r.warn("user %s: password not imported (unsupported hash), must reset", target)
		}
		name := target
		if display != nil && *display != "" {
			name = *display
		}
		var email string
		var quota *int64
		if ex, ok := extras[uid]; ok {
			email = ex.email
			if q, qok := parseNCQuota(ex.quotaRaw); qok {
				quota = q
			} else {
				r.warn("user %s: quota %q not understood, imported without quota", target, ex.quotaRaw)
			}
			if ex.accountsBroken && ex.email == "" {
				r.warn("user %s: accounts data not parseable, email not imported from it", target)
			}
		}
		if dryRun {
			uc.created++
			planned[target] = true
			continue
		}
		if err := dst.Create(ctx, &users.User{UID: target, DisplayName: name, Email: email, PasswordHash: hash, QuotaBytes: quota, Enabled: true}); err != nil {
			if errors.Is(err, users.ErrExists) {
				uc.skipped++
				continue
			}
			r.warn("user %s: create failed: %v", target, err)
			uc.failed++
			continue
		}
		uc.created++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("import users: read source users: %w", err)
	}
	return mapped, planned, nil
}

// isArgon2idPHC reports whether hash carries the argon2id PHC prefix, the
// format both PHP's password_hash and ncgo's Argon2id verifier parse.
func isArgon2idPHC(hash string) bool {
	return strings.HasPrefix(hash, "$argon2id$")
}

// ncUserExtras carries the email and quota of one source user, gathered from
// <prefix>preferences and <prefix>accounts (oc_users itself has neither
// column in any Nextcloud release).
type ncUserExtras struct {
	email          string // resolved: settings/primary_email > settings/email > oc_accounts
	quotaRaw       string // raw files/quota preference value; "" when unset
	accountsBroken bool   // oc_accounts.data held invalid JSON
}

// readNCUserExtras collects per-source-uid email and quota. oc_preferences
// holds quota (appid files, key quota) and email (appid settings, keys
// primary_email and email); oc_accounts.data (Nextcloud 13+) is a JSON blob
// whose "email" property is the fallback. A missing table (very old sources)
// degrades to one warning instead of failing the run.
func readNCUserExtras(ctx context.Context, src database.DB, prefix string, r *importReport) map[string]*ncUserExtras {
	extras := map[string]*ncUserExtras{}
	extra := func(uid string) *ncUserExtras {
		e, ok := extras[uid]
		if !ok {
			e = &ncUserExtras{}
			extras[uid] = e
		}
		return e
	}
	rows, err := src.Query(ctx, `
SELECT userid, configkey, configvalue FROM `+prefix+`preferences
WHERE (appid = ? AND configkey = ?) OR (appid = ? AND (configkey = ? OR configkey = ?))
ORDER BY userid`,
		"files", "quota", "settings", "email", "primary_email")
	if err != nil {
		r.warn("read %spreferences: %v (user email/quota from preferences not imported)", prefix, err)
	} else {
		primary := map[string]string{}
		for rows.Next() {
			var uid, key string
			var value *string
			if err := rows.Scan(&uid, &key, &value); err != nil {
				r.warn("scan %spreferences: %v", prefix, err)
				continue
			}
			if value == nil {
				continue
			}
			switch key {
			case "quota":
				extra(uid).quotaRaw = *value
			case "email":
				extra(uid).email = *value
			case "primary_email":
				primary[uid] = *value
			}
		}
		if err := rows.Err(); err != nil {
			r.warn("read %spreferences: %v", prefix, err)
		}
		rows.Close()
		for uid, p := range primary {
			if p != "" {
				extra(uid).email = p
			}
		}
	}
	arow, err := src.Query(ctx, `SELECT uid, data FROM `+prefix+`accounts ORDER BY uid`)
	if err != nil {
		// oc_accounts exists since Nextcloud 13; older sources only have
		// the preferences rows above.
		r.warn("read %saccounts: %v (user email from accounts not imported)", prefix, err)
		return extras
	}
	defer arow.Close()
	for arow.Next() {
		var uid, data string
		if err := arow.Scan(&uid, &data); err != nil {
			r.warn("scan %saccounts: %v", prefix, err)
			continue
		}
		email, err := ncAccountEmail(data)
		if err != nil {
			extra(uid).accountsBroken = true
			continue
		}
		if e := extra(uid); e.email == "" {
			e.email = email
		}
	}
	if err := arow.Err(); err != nil {
		r.warn("read %saccounts: %v", prefix, err)
	}
	return extras
}

// ncAccountEmail extracts the email property value from an oc_accounts.data
// JSON blob, shaped {"email": {"value": "...", "scope": "...", ...}, ...}.
func ncAccountEmail(data string) (string, error) {
	var props map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &props); err != nil {
		return "", fmt.Errorf("accounts data: %w", err)
	}
	raw, ok := props["email"]
	if !ok {
		return "", nil
	}
	var prop struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &prop); err != nil {
		return "", fmt.Errorf("accounts email property: %w", err)
	}
	return prop.Value, nil
}

// parseNCQuota converts an oc_preferences files/quota value to bytes.
// "none", "default", and "" all mean no explicit quota (nil); ncgo has no
// instance default quota, so "default" maps to unlimited like "none". Sizes
// follow Nextcloud's OC_Helper::computerFileSize: a number (bytes, float
// allowed) with an optional 1024-based unit suffix (k/m/g/t/p with optional
// trailing b, case-insensitive). ok=false marks an unparseable value; the
// caller warns and imports the user without a quota.
func parseNCQuota(raw string) (quota *int64, ok bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "", "none", "default":
		return nil, true
	}
	mult := int64(1)
	num := strings.TrimSuffix(s, "b")
	if len(num) > 0 {
		switch num[len(num)-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		case 'p':
			mult = 1 << 50
		}
		if mult != 1 {
			num = num[:len(num)-1]
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return nil, false
	}
	b := math.Round(f * float64(mult))
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 || b > math.MaxInt64 {
		return nil, false
	}
	q := int64(b)
	return &q, true
}

// importNCGroupRows imports <prefix>groups and returns the set of gids
// planned (dry-run only).
func importNCGroupRows(ctx context.Context, src database.DB, dst *users.SQLStore, prefix string, dryRun bool, r *importReport) (map[string]bool, error) {
	gc := r.entity("groups")
	planned := map[string]bool{}
	rows, err := src.Query(ctx, `SELECT gid FROM `+prefix+`groups ORDER BY gid`)
	if err != nil {
		return nil, fmt.Errorf("import users: read source groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			return nil, fmt.Errorf("import users: scan group: %w", err)
		}
		if gid == "" {
			r.warn("group with empty gid skipped")
			gc.skipped++
			continue
		}
		if _, err := dst.GetGroupByGID(ctx, gid); err == nil {
			gc.skipped++
			continue
		} else if !errors.Is(err, users.ErrNotFound) {
			return nil, fmt.Errorf("import users: lookup group %s: %w", gid, err)
		}
		if dryRun {
			gc.created++
			planned[gid] = true
			continue
		}
		if err := dst.CreateGroup(ctx, &users.Group{GID: gid}); err != nil {
			if errors.Is(err, users.ErrExists) {
				gc.skipped++
				continue
			}
			r.warn("group %s: create failed: %v", gid, err)
			gc.failed++
			continue
		}
		gc.created++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("import users: read source groups: %w", err)
	}
	return planned, nil
}

// importNCMemberRows imports <prefix>group_user pairs where both the user
// (imported or pre-existing) and the group exist in the target.
func importNCMemberRows(ctx context.Context, src database.DB, dst *users.SQLStore, prefix string, dryRun bool, r *importReport, mapped map[string]string, plannedUsers, plannedGroups map[string]bool) error {
	mc := r.entity("memberships")
	members := map[string]map[string]bool{}
	rows, err := src.Query(ctx, `SELECT gid, uid FROM `+prefix+`group_user ORDER BY gid, uid`)
	if err != nil {
		return fmt.Errorf("import users: read source memberships: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gid, srcUID string
		if err := rows.Scan(&gid, &srcUID); err != nil {
			return fmt.Errorf("import users: scan membership: %w", err)
		}
		target, ok := mapped[srcUID]
		if !ok {
			r.warn("membership %s/%s: user not imported, skipped", gid, srcUID)
			mc.skipped++
			continue
		}
		groupOK := plannedGroups[gid]
		if !groupOK {
			_, err := dst.GetGroupByGID(ctx, gid)
			switch {
			case err == nil:
				groupOK = true
			case errors.Is(err, users.ErrNotFound):
			default:
				return fmt.Errorf("import users: lookup group %s: %w", gid, err)
			}
		}
		if !groupOK {
			r.warn("membership %s/%s: group not imported, skipped", gid, srcUID)
			mc.skipped++
			continue
		}
		userOK := plannedUsers[target]
		if !userOK {
			_, err := dst.GetByUID(ctx, target)
			switch {
			case err == nil:
				userOK = true
			case errors.Is(err, users.ErrNotFound):
			default:
				return fmt.Errorf("import users: lookup user %s: %w", target, err)
			}
		}
		if !userOK {
			r.warn("membership %s/%s: user not in target, skipped", gid, srcUID)
			mc.skipped++
			continue
		}
		set, cached := members[gid]
		if !cached {
			set = map[string]bool{}
			if !plannedGroups[gid] {
				uids, err := dst.GroupMembers(ctx, gid, 0)
				if err != nil {
					return fmt.Errorf("import users: list members of %s: %w", gid, err)
				}
				for _, m := range uids {
					set[m] = true
				}
			}
			members[gid] = set
		}
		if set[target] {
			mc.skipped++
			continue
		}
		if !dryRun {
			if err := dst.AddGroupMember(ctx, gid, target); err != nil {
				r.warn("membership %s/%s: add failed: %v", gid, target, err)
				mc.failed++
				continue
			}
		}
		set[target] = true
		mc.created++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import users: read source memberships: %w", err)
	}
	return nil
}
