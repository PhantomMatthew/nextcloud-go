package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func newImportNCTokens(f *importNCFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "tokens",
		Short: "Import app passwords (permanent authtokens)",
		Long: "Import app passwords from the source <prefix>authtoken table: rows\n" +
			"with type = 1 (permanent tokens) only. Browser sessions (type 0) are\n" +
			"NOT imported — ncgo browser sessions are its own token family, so\n" +
			"users log in again. Wipe tokens (type 2) are not imported either.\n\n" +
			"ncgo hashes tokens exactly like Nextcloud (SHA-512 of token+instance\n" +
			"secret), so an imported hash verifies a client presenting the original\n" +
			"token IF AND ONLY IF ncgo's instance.secret equals the source\n" +
			"instance's config.php 'secret'. Copy the secret into ncgo's\n" +
			"configuration before first use and keep it forever: rotating the\n" +
			"secret invalidates every imported token at once. Operators who keep a\n" +
			"fresh secret (recommended for production) should skip this import and\n" +
			"have users re-issue app passwords after migrating.\n\n" +
			"The source uid maps to the target uid exactly like 'import-nextcloud\n" +
			"users' does (uid, falling back to uid_lower); tokens whose user is not\n" +
			"mappable or absent from the target are skipped with a warning.\n" +
			"login_name falls back to the mapped uid when NULL, name to an empty\n" +
			"string; last_activity (unix seconds) becomes created_at. The\n" +
			"password/password_hash/keypair columns are deliberately dropped — ncgo\n" +
			"never decrypts stored passwords.\n\n" +
			"The import is idempotent: a token hash already present in the target\n" +
			"is skipped, so an interrupted run can simply be repeated.",
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
			err = importNCTokens(ctx, src, users.NewSQLStore(dst), auth.NewSQLStore(dst), f.tablePrefix, f.dryRun, r)
			if err != nil {
				return err
			}
			return r.print(cmd.OutOrStdout(), f.dryRun)
		},
	}
}

// importNCTokens imports permanent authtokens (type 1, app passwords) from
// <prefix>authtoken into the ncgo app_passwords table. ncgo's token hashing
// is byte-identical to Nextcloud's (hex sha512 of token+instance.secret), so
// the copied hash verifies the original token when — and only when — ncgo's
// instance.secret equals the source instance's secret. Existing hashes are
// skipped, so runs are idempotent and resumable. A missing authtoken table
// is a hard error, like the users importer's required tables.
func importNCTokens(ctx context.Context, src database.DB, us *users.SQLStore, as *auth.SQLStore, prefix string, dryRun bool, r *importReport) error {
	mapped, err := importNCUIDMap(ctx, src, prefix)
	if err != nil {
		return err
	}
	c := r.entity("app passwords")
	rows, err := src.Query(ctx, `
SELECT id, uid, login_name, name, token, last_activity FROM `+prefix+`authtoken
WHERE type = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("import tokens: read source authtokens: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var srcID int64
		var uid, loginName, name, token *string
		var lastActivity *int64
		if err := rows.Scan(&srcID, &uid, &loginName, &name, &token, &lastActivity); err != nil {
			return fmt.Errorf("import tokens: scan authtoken: %w", err)
		}
		if token == nil || *token == "" {
			r.warn("token %d: empty token hash, skipped", srcID)
			c.skipped++
			continue
		}
		target, ok := mapped[derefStr(uid)]
		if !ok {
			r.warn("token %d: user %s not imported, skipped", srcID, derefStr(uid))
			c.skipped++
			continue
		}
		// auth.SQLStore.Insert resolves the user via INSERT ... SELECT FROM
		// users, which silently inserts 0 rows (with a nil error) when the
		// uid is unknown — verify the target user exists before inserting.
		if _, err := us.GetByUID(ctx, target); err != nil {
			if errors.Is(err, users.ErrNotFound) {
				r.warn("token %d: user %s not in target (run 'import-nextcloud users' first), skipped", srcID, target)
				c.skipped++
				continue
			}
			return fmt.Errorf("import tokens: lookup user %s: %w", target, err)
		}
		if _, err := as.GetByHash(ctx, *token); err == nil {
			c.skipped++
			continue
		} else if !errors.Is(err, auth.ErrTokenNotFound) {
			return fmt.Errorf("import tokens: lookup hash of token %d: %w", srcID, err)
		}
		if dryRun {
			c.created++
			continue
		}
		login := target
		if loginName != nil && *loginName != "" {
			login = *loginName
		}
		last := int64(0) // last_activity NULL → 0
		if lastActivity != nil {
			last = *lastActivity
		}
		if err := as.Insert(ctx, &auth.Token{
			ID:        fmt.Sprintf("nc-%d", srcID),
			Hash:      *token,
			UID:       target,
			LoginName: login,
			Name:      derefStr(name),
			Type:      auth.TokenTypePermanent,
			CreatedAt: time.Unix(last, 0).UTC(),
		}); err != nil {
			r.warn("token %d: insert failed: %v", srcID, err)
			c.failed++
			continue
		}
		c.created++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import tokens: read source authtokens: %w", err)
	}
	return nil
}

// importNCUIDMap builds the source-uid → target-uid mapping from
// <prefix>users exactly like the users importer does — uid when it satisfies
// ncgo's uid rules, else uid_lower when that does, else the row is
// unmappable — without importing anything.
func importNCUIDMap(ctx context.Context, src database.DB, prefix string) (map[string]string, error) {
	rows, err := src.Query(ctx, `SELECT uid, uid_lower FROM `+prefix+`users ORDER BY uid`)
	if err != nil {
		return nil, fmt.Errorf("import tokens: read source users: %w", err)
	}
	defer rows.Close()
	mapped := map[string]string{}
	for rows.Next() {
		var uid, uidLower string
		if err := rows.Scan(&uid, &uidLower); err != nil {
			return nil, fmt.Errorf("import tokens: scan user: %w", err)
		}
		target := uid
		if !validNCImportUID(target) {
			target = uidLower
			if !validNCImportUID(target) {
				continue
			}
		}
		mapped[uid] = target
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("import tokens: read source users: %w", err)
	}
	return mapped, nil
}
