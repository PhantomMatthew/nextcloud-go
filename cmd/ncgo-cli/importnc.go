package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// 4e-series subcommands (users now; files, shares, dav later).
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
			"  users   users, groups, and group memberships\n" +
			"  files   file data (planned)\n" +
			"  shares  internal and public-link shares (planned)\n" +
			"  dav     calendars and contacts (planned)\n\n" +
			"Sessions and app passwords are NOT imported: Nextcloud authtokens are\n" +
			"cryptographically bound to the source instance secret, so users must log\n" +
			"in again and reissue app passwords after migrating.",
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
	cmd.AddCommand(newImportNCUsers(f))
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
		if _, err := fmt.Fprintf(w, "%s: %d created, %d skipped (existing), %d failed\n",
			name, c.created, c.skipped, c.failed); err != nil {
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
			"Existing users, groups, and memberships in the target are skipped unchanged,\n" +
			"so the import is idempotent and resumable. Writes are committed per entity.\n" +
			"Sessions and app passwords are NOT imported (see the parent command help).",
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
			if err := importNCUsers(ctx, src, users.NewSQLStore(dst), f.tablePrefix, f.dryRun, r); err != nil {
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
		if dryRun {
			uc.created++
			planned[target] = true
			continue
		}
		if err := dst.Create(ctx, &users.User{UID: target, DisplayName: name, PasswordHash: hash, Enabled: true}); err != nil {
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
