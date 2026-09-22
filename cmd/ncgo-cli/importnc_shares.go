package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// ncShareType* are the oc_share share_type values; ncgo's files.ShareType*
// constants deliberately carry the same numbers (0/1/3), so no mapping is
// needed for the supported types.
const (
	ncShareTypeUser  = 0
	ncShareTypeGroup = 1
	ncShareTypeLink  = 3
)

// ncHomeStoragePrefix marks filecache rows living on a user's home storage;
// shares on any other storage (external, object store) are skipped.
const ncHomeStoragePrefix = "home::"

func newImportNCShares(f *importNCFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "shares",
		Short: "Import internal and public-link shares",
		Long: "Import shares from the source <prefix>share table, resolving each\n" +
			"share's target path through <prefix>filecache and <prefix>storages.\n\n" +
			"Supported share types: 0 (user), 1 (group), and 3 (public link).\n" +
			"Remote/federated and circle shares (types 4, 6, 7) are skipped with a\n" +
			"warning — they must be re-created over OCM after migration. Only files\n" +
			"on home storages (home::<uid>) are supported; shares on external\n" +
			"storage are skipped with a warning.\n\n" +
			"Public-link tokens are imported verbatim so existing client links keep\n" +
			"working; a token that collides with an unrelated existing share is\n" +
			"skipped with a warning. Link passwords: standard argon2id PHC hashes\n" +
			"($argon2id$...) are imported verbatim and keep working (PHP's\n" +
			"password_hash output is the format ncgo's verifier parses); any other\n" +
			"hash format (bcrypt etc.) causes the share to be SKIPPED with a\n" +
			"warning — importing it unprotected would silently drop password\n" +
			"protection. User and group shares carry no token in Nextcloud, so a\n" +
			"fresh random token is generated for each (ncgo requires one).\n\n" +
			"Permission bitmasks are identical in Nextcloud and ncgo\n" +
			"(1=read, 2=update, 4=create, 8=delete, 16=share) and are imported\n" +
			"verbatim. stime (unix seconds) becomes stime_ms; expiration\n" +
			"('YYYY-MM-DD HH:MM:SS', UTC) becomes expire_ms. Nextcloud's per-share\n" +
			"note has no ncgo counterpart and is dropped (one summary warning).\n\n" +
			"Preconditions: the owner and (for user shares) the recipient must exist\n" +
			"in the target (run 'import-nextcloud users' first), the recipient group\n" +
			"must exist for group shares, and the shared file must exist in the\n" +
			"target filecache for the owner (run 'import-nextcloud files' first);\n" +
			"otherwise the share is skipped with a warning. The import is idempotent:\n" +
			"an existing equivalent share (same owner+path+type+recipient, or same\n" +
			"link token) is skipped, so an interrupted run can simply be repeated.",
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
			err = importNCShares(ctx, src, users.NewSQLStore(dst), files.NewSQLStore(dst),
				sharing.NewSQLShareStore(dst), f.tablePrefix, f.dryRun, r)
			if err != nil {
				return err
			}
			return r.print(cmd.OutOrStdout(), f.dryRun)
		},
	}
}

// ncShareRow is one row of the oc_share/filecache/storages join, with NULLs
// collapsed to "".
type ncShareRow struct {
	id          int64
	shareType   int
	shareWith   string
	uidOwner    string
	fileSource  int64
	permissions int
	stime       int64
	expiration  string
	token       string
	password    string
	label       string
	note        string
	fcPath      string // "" when hasFC is false
	hasFC       bool
	storageID   string
}

// importNCShares imports oc_share rows into the ncgo shares table. Best-effort:
// per-share failures warn and continue; existing equivalent shares are
// skipped, so runs are idempotent and resumable.
func importNCShares(ctx context.Context, src database.DB, us *users.SQLStore, fs *files.SQLStore, ss *sharing.SQLShareStore, prefix string, dryRun bool, r *importReport) error {
	rows, err := src.Query(ctx, `
SELECT s.id, s.share_type, s.share_with, s.uid_owner, s.file_source, s.permissions, s.stime,
       s.expiration, s.token, s.password, s.label, s.note, fc.path, st.id
FROM `+prefix+`share s
LEFT JOIN `+prefix+`filecache fc ON fc.fileid = s.file_source
LEFT JOIN `+prefix+`storages st ON st.numeric_id = fc.storage
ORDER BY s.id`)
	if err != nil {
		return fmt.Errorf("import shares: read source shares: %w", err)
	}
	defer rows.Close()
	notesDropped := 0
	for rows.Next() {
		var row ncShareRow
		var shareWith, expiration, token, password, label, note, fcPath, storageID *string
		if err := rows.Scan(&row.id, &row.shareType, &shareWith, &row.uidOwner, &row.fileSource,
			&row.permissions, &row.stime, &expiration, &token, &password, &label, &note,
			&fcPath, &storageID); err != nil {
			return fmt.Errorf("import shares: scan share: %w", err)
		}
		row.shareWith = derefStr(shareWith)
		row.expiration = derefStr(expiration)
		row.token = derefStr(token)
		row.password = derefStr(password)
		row.label = derefStr(label)
		row.note = derefStr(note)
		row.fcPath = derefStr(fcPath)
		row.hasFC = fcPath != nil
		row.storageID = derefStr(storageID)
		importNCShare(ctx, &row, us, fs, ss, dryRun, r, &notesDropped)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("import shares: read source shares: %w", err)
	}
	if notesDropped > 0 {
		r.warn("%d share note(s) not imported (ncgo has no note field)", notesDropped)
	}
	return nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// importNCShare imports one share row, counting it under its type's entity.
func importNCShare(ctx context.Context, row *ncShareRow, us *users.SQLStore, fs *files.SQLStore, ss *sharing.SQLShareStore, dryRun bool, r *importReport, notesDropped *int) {
	var c *importCounts
	switch row.shareType {
	case ncShareTypeUser:
		c = r.entity("user shares")
	case ncShareTypeGroup:
		c = r.entity("group shares")
	case ncShareTypeLink:
		c = r.entity("link shares")
	default:
		rc := r.entity("remote shares")
		r.warn("share %d: type %d (remote/federated/circle) not supported, skipped", row.id, row.shareType)
		rc.skipped++
		return
	}
	owner, err := us.GetByUID(ctx, row.uidOwner)
	switch {
	case err == nil:
	case errors.Is(err, users.ErrNotFound):
		r.warn("share %d: owner %s not in target (run 'import-nextcloud users' first), skipped", row.id, row.uidOwner)
		c.skipped++
		return
	default:
		r.warn("share %d: lookup owner %s: %v", row.id, row.uidOwner, err)
		c.failed++
		return
	}
	if !row.hasFC {
		r.warn("share %d: no filecache row for fileid %d, skipped", row.id, row.fileSource)
		c.skipped++
		return
	}
	if !strings.HasPrefix(row.storageID, ncHomeStoragePrefix) {
		r.warn("share %d: fileid %d on non-home storage %q (external storage unsupported), skipped", row.id, row.fileSource, row.storageID)
		c.skipped++
		return
	}
	path, ok := ncHomePath(row.fcPath)
	if !ok {
		r.warn("share %d: filecache path %q is outside the user's files root, skipped", row.id, row.fcPath)
		c.skipped++
		return
	}
	if row.note != "" {
		*notesDropped++
	}
	if !importNCRecipientOK(ctx, row, us, c, r) {
		return
	}
	target, err := fs.GetByPath(ctx, owner.ID, path)
	switch {
	case err == nil:
	case errors.Is(err, files.ErrNotFound):
		r.warn("share %d: %s not in target filecache for %s (run 'import-nextcloud files' first), skipped", row.id, path, row.uidOwner)
		c.skipped++
		return
	default:
		r.warn("share %d: stat target %s: %v", row.id, path, err)
		c.failed++
		return
	}
	expireMs, err := ncExpirationMs(row.expiration)
	if err != nil {
		// Importing without the expiration would extend the share's validity
		// beyond what the owner set — a silent security weakening — so skip.
		r.warn("share %d: unparseable expiration %q, skipped", row.id, row.expiration)
		c.skipped++
		return
	}
	if row.shareType == ncShareTypeLink {
		if row.token == "" {
			r.warn("share %d: link share without token, skipped", row.id)
			c.skipped++
			return
		}
		if row.password != "" && !isArgon2idPHC(row.password) {
			r.warn("share %d: link password not imported (unsupported hash format), share skipped", row.id)
			c.skipped++
			return
		}
	}
	itemType := "file"
	if target.IsDir {
		itemType = "folder"
	}
	sh := &files.Share{
		OwnerUserID: owner.ID,
		ShareType:   row.shareType,
		Path:        path,
		ItemType:    itemType,
		Token:       row.token,
		Permissions: row.permissions,
		Label:       row.label,
		ExpireMs:    expireMs,
		StimeMs:     row.stime * 1000,
		ShareWith:   row.shareWith,
		Accepted:    1,
	}
	if row.shareType == ncShareTypeLink {
		sh.PasswordHash = row.password
	}
	importNCShareInsert(ctx, row, sh, ss, dryRun, c, r)
}

// importNCRecipientOK validates the recipient precondition (target user for
// type 0, target group for type 1), returning false once the share has been
// counted and must not proceed.
func importNCRecipientOK(ctx context.Context, row *ncShareRow, us *users.SQLStore, c *importCounts, r *importReport) bool {
	if row.shareType == ncShareTypeLink {
		return true
	}
	if row.shareWith == "" {
		r.warn("share %d: empty recipient, skipped", row.id)
		c.skipped++
		return false
	}
	if row.shareType == ncShareTypeUser {
		_, err := us.GetByUID(ctx, row.shareWith)
		switch {
		case err == nil:
			return true
		case errors.Is(err, users.ErrNotFound):
			r.warn("share %d: recipient user %s not in target, skipped", row.id, row.shareWith)
			c.skipped++
		default:
			r.warn("share %d: lookup recipient %s: %v", row.id, row.shareWith, err)
			c.failed++
		}
		return false
	}
	_, err := us.GetGroupByGID(ctx, row.shareWith)
	switch {
	case err == nil:
		return true
	case errors.Is(err, users.ErrNotFound):
		r.warn("share %d: recipient group %s not in target, skipped", row.id, row.shareWith)
		c.skipped++
	default:
		r.warn("share %d: lookup recipient group %s: %v", row.id, row.shareWith, err)
		c.failed++
	}
	return false
}

// newImportToken generates the token for imported user/group shares; a
// package-level seam so tests can force a collision against the retry path.
var newImportToken = randomImportToken

// importNCShareInsert performs the idempotency check and the insert. Links
// match on their verbatim token; user/group shares match on
// owner+path+type+recipient and get a fresh random token (Nextcloud stores no
// token for them, but ncgo requires one, generated exactly like the sharing
// service does at create time).
func importNCShareInsert(ctx context.Context, row *ncShareRow, sh *files.Share, ss *sharing.SQLShareStore, dryRun bool, c *importCounts, r *importReport) {
	if sh.ShareType == ncShareTypeLink {
		existing, err := ss.GetByToken(ctx, sh.Token)
		switch {
		case err == nil:
			if equivalentShare(existing, sh) {
				c.skipped++
			} else {
				r.warn("share %d: token %s already used by an unrelated share, skipped", row.id, sh.Token)
				c.skipped++
			}
			return
		case errors.Is(err, files.ErrNotFound):
		default:
			r.warn("share %d: lookup token: %v", row.id, err)
			c.failed++
			return
		}
	} else {
		existing, err := ss.ListByOwner(ctx, sh.OwnerUserID, sh.Path)
		if err != nil {
			r.warn("share %d: list owner shares: %v", row.id, err)
			c.failed++
			return
		}
		for i := range existing {
			if equivalentShare(&existing[i], sh) {
				c.skipped++
				return
			}
		}
	}
	if dryRun {
		c.created++
		return
	}
	if sh.ShareType != ncShareTypeLink {
		tok, err := newImportToken()
		if err != nil {
			r.warn("share %d: generate token: %v", row.id, err)
			c.failed++
			return
		}
		sh.Token = tok
	}
	if err := ss.Insert(ctx, sh); err != nil {
		if errors.Is(err, files.ErrExists) && sh.ShareType != ncShareTypeLink {
			// Generated token collided with an existing share's token;
			// retry once with a fresh token.
			tok, terr := newImportToken()
			if terr == nil {
				sh.Token = tok
				err = ss.Insert(ctx, sh)
			}
		}
		if err != nil {
			r.warn("share %d: insert failed: %v", row.id, err)
			c.failed++
			return
		}
	}
	c.created++
}

// equivalentShare reports whether existing is the same share the import wants
// to create (same owner, path, type, and recipient), making a re-run a no-op.
func equivalentShare(existing, want *files.Share) bool {
	return existing.OwnerUserID == want.OwnerUserID &&
		existing.Path == want.Path &&
		existing.ShareType == want.ShareType &&
		existing.ShareWith == want.ShareWith
}

// importTokenAlphabet mirrors internal/sharing's token alphabet; the service's
// generator is unexported, so the small generator is duplicated here.
const importTokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomImportToken() (string, error) {
	var out [15]byte
	for i := range out {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		out[i] = importTokenAlphabet[int(b[0])%len(importTokenAlphabet)]
	}
	return string(out[:]), nil
}

// ncHomePath maps a home-storage filecache path to the owner-relative ncgo
// path: "files" → "/", "files/a/b" → "/a/b".
func ncHomePath(fcPath string) (string, bool) {
	if fcPath == "files" {
		return "/", true
	}
	rel, ok := strings.CutPrefix(fcPath, "files/")
	if !ok || rel == "" {
		return "", false
	}
	return "/" + rel, true
}

// ncExpirationMs parses Nextcloud's 'YYYY-MM-DD HH:MM:SS' (UTC) expiration;
// empty means no expiry (0).
func ncExpirationMs(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}
