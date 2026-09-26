package encrypt

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// Per-user key hierarchy (ADR-0096 phase 1, as built in ADR-0097):
//
//   - User key (UK): 32 random bytes per user, minted lazily at the user's
//     first sealed write. Sealed for storage as
//     nonce(12) || AES-256-GCM(ring[keyID], UK, "NCGOUK1" || be64(user_id))
//     where keyID is the ring position of the sealing (current) key at
//     creation time; the row keeps that position forever, so master-key
//     rotation needs no UK re-seal for correctness while the ring stays
//     append-only.
//   - File key (FK): 32 random bytes per sealed file with a 16-byte random
//     key UUID (the v3 header names it). Wrapped as
//     nonce(12) || AES-256-GCM(UK, FK, "NCGOFK1" || keyUUID(16) || be64(user_id)).
//     A wrap replayed onto a different file UUID or user fails
//     authentication.
const (
	userKeySize = 32
	fileKeySize = 32

	ukWrapAD = "NCGOUK1"
	fkWrapAD = "NCGOFK1"
)

// SQLResolver is a KeyResolver backed by the server database: user_keys
// holds one sealed UK per user, file_keys one wrapped FK per (key UUID,
// user), and files.key_uuid indexes a filecache row to its key UUID. The
// resolver holds a copy of the keyring; ring positions are the key IDs UK
// rows reference.
type SQLResolver struct {
	db   database.DB
	keys [][]byte
	// PasswordWrapped enables the ADR-0100 enrollment state machine at
	// password login (UnlockForLogin); false keeps every user master-wrapped.
	// Set by wiring after construction; the zero value is disabled.
	PasswordWrapped bool
	// KDF is the argon2id parameter set new pw_sealed_uk rows are sealed
	// with (existing rows carry their own parameters in pw_kdf). Set by
	// wiring from auth.argon2id.
	KDF KeyDerivationParams
}

// NewSQLResolver returns a KeyResolver persisting the per-user key
// hierarchy in db. keys is the full keyring in ring order (previous keys
// first, current key last — positions are the key IDs written into new UK
// rows) and is copied. Every key must be exactly MasterKeySize bytes.
func NewSQLResolver(db database.DB, keys [][]byte) (*SQLResolver, error) {
	if db == nil {
		return nil, fmt.Errorf("encrypt: resolver: nil database")
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("encrypt: resolver: empty keyring")
	}
	ring := make([][]byte, len(keys))
	for i, k := range keys {
		if len(k) != MasterKeySize {
			return nil, fmt.Errorf("encrypt: resolver: ring key %d must be %d bytes, got %d", i, MasterKeySize, len(k))
		}
		ring[i] = append([]byte(nil), k...)
	}
	return &SQLResolver{db: db, keys: ring}, nil
}

// Allocate implements KeyResolver: it mints a fresh file key, wraps it for
// the owner derived from the storage key, and persists the file_keys row,
// lazily creating the owner's UK on first use. The owner uid is parsed from
// the storage key (not looked up in the filecache — the row does not exist
// yet at Create time); an unknown uid is an error, never an invented user.
func (r *SQLResolver) Allocate(ctx context.Context, storageKey string) ([16]byte, []byte, error) {
	var zero [16]byte
	uid := ownerUID(storageKey)
	if uid == "" {
		return zero, nil, fmt.Errorf("encrypt: allocate: storage key %q names no owner", storageKey)
	}
	userID, err := r.userID(ctx, uid)
	if err != nil {
		return zero, nil, err
	}
	fk := make([]byte, fileKeySize)
	if _, err := rand.Read(fk); err != nil {
		return zero, nil, fmt.Errorf("encrypt: generate file key: %w", err)
	}
	var keyUUID [keyUUIDSize]byte
	if _, err := rand.Read(keyUUID[:]); err != nil {
		return zero, nil, fmt.Errorf("encrypt: generate key uuid: %w", err)
	}
	// An enrolled owner is wrapped with their public key (ADR-0100): a box
	// needs no session and no key material of theirs. An unenrolled owner
	// gets the symmetric UK wrap, lazily minted on first use.
	if pub, enrolled, err := r.publicKeyFor(ctx, userID); err != nil {
		return zero, nil, err
	} else if enrolled {
		blob, err := boxWrap(pub, fk, keyUUID, userID)
		if err != nil {
			return zero, nil, err
		}
		if _, err := r.db.Exec(ctx, `
INSERT INTO file_keys (key_uuid, user_id, wrapped_fk, created_ms, scheme) VALUES (?, ?, ?, ?, 1)`,
			keyUUID[:], userID, blob, time.Now().UTC().UnixMilli()); err != nil {
			return zero, nil, fmt.Errorf("encrypt: insert file key %s: %w", hex.EncodeToString(keyUUID[:]), err)
		}
		return keyUUID, fk, nil
	}
	uk, err := r.loadOrCreateUK(ctx, userID)
	if err != nil {
		return zero, nil, err
	}
	wrapped, err := wrapSeal(uk, fk, fkAD(keyUUID, userID))
	if err != nil {
		return zero, nil, err
	}
	if _, err := r.db.Exec(ctx, `
INSERT INTO file_keys (key_uuid, user_id, wrapped_fk, created_ms) VALUES (?, ?, ?, ?)`,
		keyUUID[:], userID, wrapped, time.Now().UTC().UnixMilli()); err != nil {
		return zero, nil, fmt.Errorf("encrypt: insert file key %s: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	return keyUUID, fk, nil
}

// Resolve implements KeyResolver: it unwraps the file key named by keyUUID.
// The owner is looked up in the filecache first (files.key_uuid); when that
// row is gone (trash, versions, upload parts) the file_keys wrap rows name
// the owner set — phase 1 writes only owner rows, so the lowest user ID is
// the owner.
//
// An owner with a user_keys row (unenrolled) resolves through the master→
// UK→FK chain exactly as in phases 1–3; every such failure wraps
// ErrUnresolvableKey, never ErrIntegrity: a resolve failure is a key or
// configuration problem, not data corruption. An enrolled owner (no
// user_keys row — the master key opens nothing of theirs, ADR-0100)
// resolves through the READING user's own wrap row, with the reader's
// identity and key taken from the request context: an enrolled reader
// box-opens their scheme=1 row with the session key, an unenrolled reader
// opens their scheme=0 row via the master path (mixed populations
// interoperate per-recipient). No principal, an unknown reader, no unlocked
// key, or no row for the reader is ErrKeyLocked — never ErrUnresolvableKey
// or ErrIntegrity, so tooling and WebDAV can tell "locked" apart from
// "broken".
func (r *SQLResolver) Resolve(ctx context.Context, keyUUID [keyUUIDSize]byte) ([]byte, error) {
	userID, err := r.ownerOf(ctx, keyUUID)
	if err != nil {
		return nil, err
	}
	hasUK, err := r.hasUserKey(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !hasUK {
		return r.resolveIdentity(ctx, keyUUID)
	}
	uk, found, err := r.loadUK(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("encrypt: key uuid %s: user %d has no user key row: %w",
			hex.EncodeToString(keyUUID[:]), userID, ErrUnresolvableKey)
	}
	var wrapped []byte
	err = r.db.QueryRow(ctx, `
SELECT wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = ?`, keyUUID[:], userID).Scan(&wrapped)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, fmt.Errorf("encrypt: key uuid %s: no wrap row for user %d: %w",
				hex.EncodeToString(keyUUID[:]), userID, ErrUnresolvableKey)
		}
		return nil, fmt.Errorf("encrypt: load file key %s: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	fk, err := wrapOpen(uk, wrapped, fkAD(keyUUID, userID))
	if err != nil {
		return nil, fmt.Errorf("encrypt: unwrap file key %s for user %d: %w",
			hex.EncodeToString(keyUUID[:]), userID, ErrUnresolvableKey)
	}
	return fk, nil
}

// resolveIdentity unwraps an enrolled owner's file key through the reading
// user's wrap row (ADR-0100 §5). The reader comes from the request context:
// an unenrolled reader's row opens symmetrically via the master→UK chain; an
// enrolled reader's box row opens with the session-attached unlocked private
// key. Every "cannot read" outcome is ErrKeyLocked; a box that fails
// authentication is ErrIntegrity (tampered or wrong key material), and a
// broken symmetric chain is ErrUnresolvableKey — the lock cases are never
// conflated with corruption or configuration errors.
func (r *SQLResolver) resolveIdentity(ctx context.Context, keyUUID [keyUUIDSize]byte) ([]byte, error) {
	p, ok := auth.UserFromContext(ctx)
	if !ok || p == nil || p.UID == "" {
		return nil, fmt.Errorf("encrypt: key uuid %s: no authenticated principal: %w",
			hex.EncodeToString(keyUUID[:]), ErrKeyLocked)
	}
	readerID, found, err := r.lookupUserID(ctx, p.UID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("encrypt: key uuid %s: reading user %q unknown: %w",
			hex.EncodeToString(keyUUID[:]), p.UID, ErrKeyLocked)
	}
	var wrapped []byte
	err = r.db.QueryRow(ctx, `
SELECT wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = ?`, keyUUID[:], readerID).Scan(&wrapped)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, fmt.Errorf("encrypt: key uuid %s: no wrap row for reader %q: %w",
				hex.EncodeToString(keyUUID[:]), p.UID, ErrKeyLocked)
		}
		return nil, fmt.Errorf("encrypt: load file key %s: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	if hasUK, err := r.hasUserKey(ctx, readerID); err != nil {
		return nil, err
	} else if hasUK {
		uk, found, err := r.loadUK(ctx, readerID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("encrypt: key uuid %s: reader %q has no user key row: %w",
				hex.EncodeToString(keyUUID[:]), p.UID, ErrUnresolvableKey)
		}
		fk, err := wrapOpen(uk, wrapped, fkAD(keyUUID, readerID))
		if err != nil {
			return nil, fmt.Errorf("encrypt: unwrap file key %s for reader %q: %w",
				hex.EncodeToString(keyUUID[:]), p.UID, ErrUnresolvableKey)
		}
		return fk, nil
	}
	if len(p.UnlockedKey) == 0 {
		return nil, fmt.Errorf("encrypt: key uuid %s: reader %q has no unlocked session key: %w",
			hex.EncodeToString(keyUUID[:]), p.UID, ErrKeyLocked)
	}
	fk, err := boxOpen(p.UnlockedKey, wrapped, keyUUID, readerID)
	if err != nil {
		return nil, fmt.Errorf("encrypt: open file key box %s for reader %q: %w",
			hex.EncodeToString(keyUUID[:]), p.UID, errors.Join(err, ErrIntegrity))
	}
	return fk, nil
}

// WrapKeyFor wraps the file key named by keyUUID for the user uid (a share
// recipient, ADR-0098), per the recipient's scheme: an enrolled recipient is
// boxed with their public key (never lazy-minted — the keypair exists by
// construction), an unenrolled one symmetrically with their lazily-created
// UK. Re-wrapping the same (key, user) pair is a no-op, so grant hooks can
// retry freely. An unknown uid is an error — the resolver never invents
// users; an unknown keyUUID surfaces as ErrUnresolvableKey (there is no FK
// to wrap).
//
// The FK comes from Resolve in the caller's ctx — the FK-availability corner
// of ADR-0101: for an enrolled OWNER's key, Resolve succeeds only when the
// ctx carries an authorized reader (owner or recipient with an unlocked
// session). Hooks firing in a third-party ctx (an admin's group-member-add)
// therefore fail best-effort for enrolled-owner files (existing hook
// semantics: logged, never fatal) and heal on the next authorized write via
// WrapForWrite, which threads the FK (WrapKeyForFK).
func (r *SQLResolver) WrapKeyFor(ctx context.Context, keyUUID [keyUUIDSize]byte, uid string) error {
	userID, err := r.userID(ctx, uid)
	if err != nil {
		return err
	}
	fk, err := r.Resolve(ctx, keyUUID)
	if err != nil {
		return err
	}
	return r.wrapFKForUser(ctx, keyUUID, fk, userID)
}

// WrapKeyForFK is WrapKeyFor with the plaintext file key supplied by the
// caller — the write path holds it at Allocate time, so an overwrite into an
// enrolled owner's tree wraps recipients without a Resolve the writer's ctx
// cannot satisfy (ADR-0101). The caller is in the storage trust domain; the
// FK is never persisted beyond the wrap rows it produces.
func (r *SQLResolver) WrapKeyForFK(ctx context.Context, keyUUID [keyUUIDSize]byte, uid string, fk []byte) error {
	userID, err := r.userID(ctx, uid)
	if err != nil {
		return err
	}
	return r.wrapFKForUser(ctx, keyUUID, fk, userID)
}

// UnwrapKeyFor deletes the user's wrap row for keyUUID (share revocation,
// ADR-0098); a missing row is a no-op. Two guards keep the delete safe: the
// user owning the files.key_uuid row for keyUUID is never unwrapped — a
// revoke must never orphan a file from its owner (the owner can be a member
// of a group the file is shared to) — and an unknown uid is a no-op (no live
// user means no addressable row; stale rows of deleted users are purged by
// OnUserDeleted at user deletion and by PruneStaleKeys, ADR-0099).
func (r *SQLResolver) UnwrapKeyFor(ctx context.Context, keyUUID [keyUUIDSize]byte, uid string) error {
	userID, found, err := r.lookupUserID(ctx, uid)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	owner, err := r.fileOwner(ctx, keyUUID)
	if err != nil {
		return err
	}
	if owner == userID {
		return nil
	}
	if _, err := r.db.Exec(ctx, `
DELETE FROM file_keys WHERE key_uuid = ? AND user_id = ?`, keyUUID[:], userID); err != nil {
		return fmt.Errorf("encrypt: unwrap file key %s for user %d: %w",
			hex.EncodeToString(keyUUID[:]), userID, err)
	}
	return nil
}

// ReWrapSharees carries a file's share-recipient wraps across an overwrite
// (ADR-0098): the overwrite minted newUUID with its owner row (Allocate), so
// every non-owner recipient of oldUUID gets a wrap of the new file key,
// after which all old-UUID rows are deleted — wrap rows belong to the live
// file, and continuity for sharees means following the current key. A
// recipient wrap failure aborts before the old rows are deleted so a retry
// continues where it stopped; a re-run after success finds no old rows and
// re-inserts nothing (idempotent).
func (r *SQLResolver) ReWrapSharees(ctx context.Context, oldUUID, newUUID [keyUUIDSize]byte) error {
	if oldUUID == newUUID {
		return nil
	}
	fk, err := r.Resolve(ctx, newUUID)
	if err != nil {
		return err
	}
	return r.reWrapShareesWithFK(ctx, oldUUID, newUUID, fk)
}

// ReWrapShareesFK is ReWrapSharees with the new file key supplied by the
// caller (ADR-0101): the overwrite path holds it at Allocate time, so
// carrying recipient wraps across an overwrite of an enrolled owner's file
// needs no Resolve in the writer's ctx (which would fail ErrKeyLocked for an
// enrolled writer who holds no row on the fresh UUID yet).
func (r *SQLResolver) ReWrapShareesFK(ctx context.Context, oldUUID, newUUID [keyUUIDSize]byte, fk []byte) error {
	if oldUUID == newUUID {
		return nil
	}
	return r.reWrapShareesWithFK(ctx, oldUUID, newUUID, fk)
}

// reWrapShareesWithFK carries oldUUID's non-owner recipient wraps to newUUID
// using fk (newUUID's plaintext file key) and deletes the old rows.
func (r *SQLResolver) reWrapShareesWithFK(ctx context.Context, oldUUID, newUUID [keyUUIDSize]byte, fk []byte) error {
	newOwner, err := r.ownerOf(ctx, newUUID)
	if err != nil {
		return err
	}
	rows, err := r.db.Query(ctx, `
SELECT user_id FROM file_keys WHERE key_uuid = ? ORDER BY user_id`, oldUUID[:])
	if err != nil {
		return fmt.Errorf("encrypt: list wraps for key uuid %s: %w", hex.EncodeToString(oldUUID[:]), err)
	}
	defer rows.Close()
	var recipients []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return fmt.Errorf("encrypt: list wraps for key uuid %s: %w", hex.EncodeToString(oldUUID[:]), err)
		}
		if userID != newOwner {
			recipients = append(recipients, userID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("encrypt: list wraps for key uuid %s: %w", hex.EncodeToString(oldUUID[:]), err)
	}
	for _, userID := range recipients {
		if err := r.wrapFKForUser(ctx, newUUID, fk, userID); err != nil {
			return err
		}
	}
	if _, err := r.db.Exec(ctx, `
DELETE FROM file_keys WHERE key_uuid = ?`, oldUUID[:]); err != nil {
		return fmt.Errorf("encrypt: delete old wraps for key uuid %s: %w", hex.EncodeToString(oldUUID[:]), err)
	}
	return nil
}

// wrapFKForUser inserts a wrap of fk for the user, per their enrollment
// scheme (ADR-0100): an enrolled recipient gets a scheme=1 box under their
// public key (their private key is never server-held — a lazy mint is
// impossible and unnecessary, the keypair exists by construction); an
// unenrolled recipient gets a scheme=0 symmetric wrap under their UK (lazily
// created) with the pinned AD. A duplicate (key uuid, user) row is a no-op.
func (r *SQLResolver) wrapFKForUser(ctx context.Context, keyUUID [keyUUIDSize]byte, fk []byte, userID int64) error {
	if pub, enrolled, err := r.publicKeyFor(ctx, userID); err != nil {
		return err
	} else if enrolled {
		blob, err := boxWrap(pub, fk, keyUUID, userID)
		if err != nil {
			return err
		}
		if _, err := r.db.Exec(ctx, `
INSERT INTO file_keys (key_uuid, user_id, wrapped_fk, created_ms, scheme) VALUES (?, ?, ?, ?, 1)`,
			keyUUID[:], userID, blob, time.Now().UTC().UnixMilli()); err != nil {
			if database.IsUniqueViolation(r.db.Dialect(), err) {
				return nil
			}
			return fmt.Errorf("encrypt: wrap file key %s for user %d: %w",
				hex.EncodeToString(keyUUID[:]), userID, err)
		}
		return nil
	}
	uk, err := r.loadOrCreateUK(ctx, userID)
	if err != nil {
		return err
	}
	wrapped, err := wrapSeal(uk, fk, fkAD(keyUUID, userID))
	if err != nil {
		return err
	}
	if _, err := r.db.Exec(ctx, `
INSERT INTO file_keys (key_uuid, user_id, wrapped_fk, created_ms) VALUES (?, ?, ?, ?)`,
		keyUUID[:], userID, wrapped, time.Now().UTC().UnixMilli()); err != nil {
		if database.IsUniqueViolation(r.db.Dialect(), err) {
			return nil
		}
		return fmt.Errorf("encrypt: wrap file key %s for user %d: %w",
			hex.EncodeToString(keyUUID[:]), userID, err)
	}
	return nil
}

// fileOwner returns the user_id of the filecache row carrying keyUUID, or 0
// when none references it (trash/versions fallback territory — no live file
// owns the key).
func (r *SQLResolver) fileOwner(ctx context.Context, keyUUID [keyUUIDSize]byte) (int64, error) {
	var userID int64
	err := r.db.QueryRow(ctx, `SELECT user_id FROM files WHERE key_uuid = ? LIMIT 1`, keyUUID[:]).Scan(&userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("encrypt: resolve key uuid %s file owner: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	return userID, nil
}

// ownerUID derives the owning user's uid from a storage key. DAV file keys
// are uid + "/" + path (dav.go storageKey); the uploads, versions, and
// trash namespaces nest the uid one level down ("versions/<uid>/<id>"), so
// there the second segment is the owner. Anything else — the appdata_
// system trees in particular — has no owner, and Allocate rejects it:
// phase 1 does not seal system trees under per-user keys.
func ownerUID(storageKey string) string {
	head, rest, _ := strings.Cut(storageKey, "/")
	switch head {
	case "uploads", "versions", "trash":
		uid, _, _ := strings.Cut(rest, "/")
		return uid
	default:
		return head
	}
}

// userID resolves a uid to its users.id; an unknown uid is an error (the
// resolver never invents users).
func (r *SQLResolver) userID(ctx context.Context, uid string) (int64, error) {
	id, found, err := r.lookupUserID(ctx, uid)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("encrypt: user %q not found", uid)
	}
	return id, nil
}

// lookupUserID resolves a uid to its users.id, reporting found = false for
// an unknown uid.
func (r *SQLResolver) lookupUserID(ctx context.Context, uid string) (int64, bool, error) {
	var id int64
	err := r.db.QueryRow(ctx, `SELECT id FROM users WHERE uid = ?`, uid).Scan(&id)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("encrypt: look up user %q: %w", uid, err)
	}
	return id, true, nil
}

// ownerOf names the user whose wrap row resolves keyUUID: the filecache
// owner first, then the lowest-ID wrap row (trash/edge fallback). A UUID
// neither names is unresolvable.
func (r *SQLResolver) ownerOf(ctx context.Context, keyUUID [keyUUIDSize]byte) (int64, error) {
	var userID int64
	err := r.db.QueryRow(ctx, `SELECT user_id FROM files WHERE key_uuid = ? LIMIT 1`, keyUUID[:]).Scan(&userID)
	if err == nil {
		return userID, nil
	}
	if !errors.Is(err, database.ErrNoRows) {
		return 0, fmt.Errorf("encrypt: resolve key uuid %s: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	err = r.db.QueryRow(ctx, `
SELECT user_id FROM file_keys WHERE key_uuid = ? ORDER BY user_id LIMIT 1`, keyUUID[:]).Scan(&userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return 0, fmt.Errorf("encrypt: key uuid %s names no known file key: %w",
				hex.EncodeToString(keyUUID[:]), ErrUnresolvableKey)
		}
		return 0, fmt.Errorf("encrypt: resolve key uuid %s: %w", hex.EncodeToString(keyUUID[:]), err)
	}
	return userID, nil
}

// loadOrCreateUK returns the user's UK, minting and sealing it under the
// current ring key on first use. A concurrent first write that wins the
// insert race surfaces as a unique violation; the loser re-reads the
// winner's row, so both return the same UK.
func (r *SQLResolver) loadOrCreateUK(ctx context.Context, userID int64) ([]byte, error) {
	uk, found, err := r.loadUK(ctx, userID)
	if err != nil || found {
		return uk, err
	}
	uk = make([]byte, userKeySize)
	if _, err := rand.Read(uk); err != nil {
		return nil, fmt.Errorf("encrypt: generate user key: %w", err)
	}
	keyID := len(r.keys) - 1
	sealed, err := wrapSeal(r.keys[keyID], uk, ukAD(userID))
	if err != nil {
		return nil, err
	}
	if _, err := r.db.Exec(ctx, `
INSERT INTO user_keys (user_id, sealed_uk, key_id, created_ms) VALUES (?, ?, ?, ?)`,
		userID, sealed, keyID, time.Now().UTC().UnixMilli()); err != nil {
		if database.IsUniqueViolation(r.db.Dialect(), err) {
			uk, found, err = r.loadUK(ctx, userID)
			if err == nil && !found {
				err = fmt.Errorf("encrypt: user key row for user %d vanished after insert conflict", userID)
			}
			return uk, err
		}
		return nil, fmt.Errorf("encrypt: insert user key for user %d: %w", userID, err)
	}
	return uk, nil
}

// loadUK reads and unseals the user's UK; found is false when no row
// exists. A row sealed under a ring position the keyring no longer holds,
// or one that fails authentication, is ErrUnresolvableKey.
func (r *SQLResolver) loadUK(ctx context.Context, userID int64) (uk []byte, found bool, err error) {
	var sealed []byte
	var keyID int64
	err = r.db.QueryRow(ctx, `
SELECT sealed_uk, key_id FROM user_keys WHERE user_id = ?`, userID).Scan(&sealed, &keyID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("encrypt: load user key for user %d: %w", userID, err)
	}
	if keyID < 0 || keyID >= int64(len(r.keys)) {
		return nil, false, fmt.Errorf("encrypt: user key for user %d sealed under key id %d, keyring holds %d keys: %w",
			userID, keyID, len(r.keys), ErrUnresolvableKey)
	}
	uk, err = wrapOpen(r.keys[keyID], sealed, ukAD(userID))
	if err != nil {
		return nil, false, fmt.Errorf("encrypt: unseal user key for user %d: %w", userID, ErrUnresolvableKey)
	}
	return uk, true, nil
}

// ukAD is the associated data binding a sealed UK to its owner:
// "NCGOUK1" || bigendian-uint64(user_id).
func ukAD(userID int64) []byte {
	ad := make([]byte, 0, len(ukWrapAD)+8)
	ad = append(ad, ukWrapAD...)
	//nolint:gosec // G115: user IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(userID))
}

// fkAD is the associated data binding a wrapped FK to its file key UUID and
// owner: "NCGOFK1" || keyUUID(16 raw bytes) || bigendian-uint64(user_id).
func fkAD(keyUUID [keyUUIDSize]byte, userID int64) []byte {
	ad := make([]byte, 0, len(fkWrapAD)+keyUUIDSize+8)
	ad = append(ad, fkWrapAD...)
	ad = append(ad, keyUUID[:]...)
	//nolint:gosec // G115: user IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(userID))
}

// wrapSeal AES-256-GCM-seals plaintext under key with the given associated
// data, returning nonce(12) || ciphertext||tag.
func wrapSeal(key, plaintext, ad []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	n := make([]byte, nonceSize)
	if _, err := rand.Read(n); err != nil {
		return nil, fmt.Errorf("encrypt: generate wrap nonce: %w", err)
	}
	return aead.Seal(n, n, plaintext, ad), nil
}

// wrapOpen reverses wrapSeal.
func wrapOpen(key, sealed, ad []byte) ([]byte, error) {
	if len(sealed) < nonceSize+tagSize {
		return nil, fmt.Errorf("encrypt: sealed key material truncated (%d bytes)", len(sealed))
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], ad)
	if err != nil {
		return nil, fmt.Errorf("encrypt: key wrap authentication failed: %w", err)
	}
	return plain, nil
}
