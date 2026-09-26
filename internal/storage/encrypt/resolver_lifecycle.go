package encrypt

import (
	"context"
	"fmt"
)

// Per-user key lifecycle and operator tooling (ADR-0096 phase 3, built in
// ADR-0099): eager UK minting at user creation, key-row purges at user
// deletion, UK re-sealing after master-key rotation, an inventory read
// model for `encryption status`, and the repair primitives
// `encryption reconcile` composes. All methods live on SQLResolver only —
// the narrow KeyResolver interface the storage decorator consumes is
// untouched.

// OnUserCreated mints the user's UK eagerly at account creation (ADR-0099),
// so the user's first sealed write never pays the mint inside the write
// path. Idempotent (an existing UK is kept); an unknown uid is an error —
// the hook fires from a successful users.SQLStore.Create, so a missing user
// is a real inconsistency, never something to invent.
func (r *SQLResolver) OnUserCreated(ctx context.Context, uid string) error {
	userID, err := r.userID(ctx, uid)
	if err != nil {
		return err
	}
	_, err = r.loadOrCreateUK(ctx, userID)
	return err
}

// OnUserDeleted purges the deleted user's per-user key rows (ADR-0099): the
// UK row and every wrap row the user holds, theirs alone. Wrap rows of OTHER
// users and files.key_uuid are untouched — sealed files a deleted user still
// owned become unresolvable, the same operator territory as
// users.SQLStore.Delete's "files are not cascaded" warning. An unknown uid
// is a no-op (mirrors UnwrapKeyFor: no live user means no addressable row),
// which also makes hook replays safe.
func (r *SQLResolver) OnUserDeleted(ctx context.Context, uid string) error {
	userID, found, err := r.lookupUserID(ctx, uid)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM file_keys WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("encrypt: delete file keys for user %d: %w", userID, err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM user_keys WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("encrypt: delete user key for user %d: %w", userID, err)
	}
	return nil
}

// ResealUserKeys re-seals every UK row not sealed under the current ring
// position (key id len(keys)-1) — the hygiene step after a master-key
// rotation, so retired keys can eventually leave the ring (ADR-0099). Each
// row is unsealed under its recorded ring position and re-sealed under the
// current key with the same pinned AD. A row whose key_id the ring no longer
// holds, or that fails authentication, aborts the pass with
// ErrUnresolvableKey naming the user — matching loadUK. Idempotent: a re-run
// finds every row at the current position and re-seals nothing.
func (r *SQLResolver) ResealUserKeys(ctx context.Context) (int, error) {
	current := len(r.keys) - 1
	rows, err := r.db.Query(ctx, `
SELECT user_id, sealed_uk, key_id FROM user_keys WHERE key_id <> ? ORDER BY user_id`, current)
	if err != nil {
		return 0, fmt.Errorf("encrypt: list user keys for re-seal: %w", err)
	}
	defer rows.Close()
	type ukRow struct {
		userID int64
		sealed []byte
		keyID  int64
	}
	var stale []ukRow
	for rows.Next() {
		var row ukRow
		if err := rows.Scan(&row.userID, &row.sealed, &row.keyID); err != nil {
			return 0, fmt.Errorf("encrypt: list user keys for re-seal: %w", err)
		}
		stale = append(stale, row)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("encrypt: list user keys for re-seal: %w", err)
	}
	resealed := 0
	for _, row := range stale {
		if row.keyID < 0 || row.keyID >= int64(len(r.keys)) {
			return resealed, fmt.Errorf("encrypt: user key for user %d sealed under key id %d, keyring holds %d keys: %w",
				row.userID, row.keyID, len(r.keys), ErrUnresolvableKey)
		}
		uk, err := wrapOpen(r.keys[row.keyID], row.sealed, ukAD(row.userID))
		if err != nil {
			return resealed, fmt.Errorf("encrypt: unseal user key for user %d: %w", row.userID, ErrUnresolvableKey)
		}
		sealed, err := wrapSeal(r.keys[current], uk, ukAD(row.userID))
		if err != nil {
			return resealed, err
		}
		if _, err := r.db.Exec(ctx, `
UPDATE user_keys SET sealed_uk = ?, key_id = ? WHERE user_id = ?`, sealed, current, row.userID); err != nil {
			return resealed, fmt.Errorf("encrypt: re-seal user key for user %d: %w", row.userID, err)
		}
		resealed++
	}
	return resealed, nil
}

// PruneStaleKeys deletes key rows whose owning user no longer exists — the
// repair half of OnUserDeleted, catching deletions that ran without the
// hook (imports, raw SQL, crashes between the row delete and the hook,
// ADR-0099). It reports the rows affected per table.
func (r *SQLResolver) PruneStaleKeys(ctx context.Context) (userKeys, fileKeys int, err error) {
	res, err := r.db.Exec(ctx, `
DELETE FROM user_keys WHERE user_id NOT IN (SELECT id FROM users)`)
	if err != nil {
		return 0, 0, fmt.Errorf("encrypt: prune stale user keys: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("encrypt: prune stale user keys: %w", err)
	}
	userKeys = int(n)
	res, err = r.db.Exec(ctx, `
DELETE FROM file_keys WHERE user_id NOT IN (SELECT id FROM users)`)
	if err != nil {
		return 0, 0, fmt.Errorf("encrypt: prune stale file keys: %w", err)
	}
	n, err = res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("encrypt: prune stale file keys: %w", err)
	}
	return userKeys, int(n), nil
}

// MintMissingUserKeys mints a UK for every user who has none — the backfill
// for accounts that predate per-user keys or the creation hook (ADR-0099).
// Idempotent; returns the number of keys minted.
func (r *SQLResolver) MintMissingUserKeys(ctx context.Context) (int, error) {
	rows, err := r.db.Query(ctx, `
SELECT id FROM users WHERE NOT EXISTS (SELECT 1 FROM user_keys WHERE user_keys.user_id = users.id) ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("encrypt: list users without a user key: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("encrypt: list users without a user key: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("encrypt: list users without a user key: %w", err)
	}
	for _, id := range ids {
		if _, err := r.loadOrCreateUK(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// KeyInventory is the per-user key hierarchy's health snapshot for
// `ncgo-cli encryption status` (ADR-0099). Counts are plain row counts over
// users/user_keys/file_keys/files; "stale" rows name a user_id no users row
// carries, and a "broken" v3 file is a filecache row whose key UUID has no
// wrap row for its owner — that file cannot resolve and is UNREADABLE.
type KeyInventory struct {
	UsersTotal       int64 // users rows
	UsersWithUK      int64 // user_keys rows
	UKsRetiredKeyID  int64 // user_keys rows sealed under a retired ring position
	StaleUKs         int64 // user_keys rows whose user is gone
	WrapRows         int64 // file_keys rows
	DistinctKeyUUIDs int64 // distinct file key UUIDs wrapped
	StaleWraps       int64 // file_keys rows whose user is gone
	V3Files          int64 // files rows with a key UUID
	BrokenV3Files    int64 // v3 files with no owner wrap row (unreadable)
}

// Inventory computes the KeyInventory against the live database. All queries
// are dialect-agnostic (placeholders, NOT IN, NOT EXISTS); no schema beyond
// migration 0020 is required.
func (r *SQLResolver) Inventory(ctx context.Context) (KeyInventory, error) {
	var inv KeyInventory
	current := len(r.keys) - 1
	queries := []struct {
		q    string
		args []any
		dst  *int64
	}{
		{`SELECT COUNT(*) FROM users`, nil, &inv.UsersTotal},
		{`SELECT COUNT(*) FROM user_keys`, nil, &inv.UsersWithUK},
		{`SELECT COUNT(*) FROM user_keys WHERE key_id < ?`, []any{current}, &inv.UKsRetiredKeyID},
		{`SELECT COUNT(*) FROM user_keys WHERE user_id NOT IN (SELECT id FROM users)`, nil, &inv.StaleUKs},
		{`SELECT COUNT(*) FROM file_keys`, nil, &inv.WrapRows},
		{`SELECT COUNT(DISTINCT key_uuid) FROM file_keys`, nil, &inv.DistinctKeyUUIDs},
		{`SELECT COUNT(*) FROM file_keys WHERE user_id NOT IN (SELECT id FROM users)`, nil, &inv.StaleWraps},
		{`SELECT COUNT(*) FROM files WHERE key_uuid IS NOT NULL`, nil, &inv.V3Files},
		{`SELECT COUNT(*) FROM files f WHERE f.key_uuid IS NOT NULL AND NOT EXISTS (
	SELECT 1 FROM file_keys k WHERE k.key_uuid = f.key_uuid AND k.user_id = f.user_id)`, nil, &inv.BrokenV3Files},
	}
	for _, q := range queries {
		if err := r.db.QueryRow(ctx, q.q, q.args...).Scan(q.dst); err != nil {
			return KeyInventory{}, fmt.Errorf("encrypt: key inventory: %w", err)
		}
	}
	return inv, nil
}
