package encrypt

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// Password-wrapped user keys (ADR-0100, built in ADR-0101): the enrollment
// state machine driven from the password-login path, the session key copy
// API, and the enrollment queries the CLI consumes. All methods are concrete
// on SQLResolver — the narrow KeyResolver interface is untouched.

// UnlockForLogin runs the enrollment state machine for a password login (the
// only moment the server holds the password). It returns the user's unlocked
// X25519 private key when the user is (or becomes) enrolled, nil when the
// master-wrapped path serves the user. The four branches, ordered so a crash
// at any point is resumed by re-running the same branch:
//
//   - enrolled && !PasswordWrapped: unenroll — open the private key with the
//     password, ensure the symmetric UK exists (the row may already be back
//     from an interrupted unenroll), re-wrap every scheme=1 row symmetric
//     (scheme=0), delete the user_key_pw row and every app_token_keys wrap of
//     the user's tokens (ADR-0102: the keypair is gone, so the token wraps
//     are dead weight), return nil.
//   - enrolled && PasswordWrapped: open the private key with the password
//     (AEAD failure means the password changed out-of-band — a loud
//     descriptive error, never ErrKeyLocked and never silent re-enrollment,
//     which would orphan every file). A lingering user_keys row means an
//     interrupted enrollment: finish the box conversion, delete the row.
//     Return the private key.
//   - !enrolled && PasswordWrapped: enroll — mint the keypair, seal the
//     private key under the password KEK, insert the user_key_pw row, box
//     every existing scheme=0 wrap with the new public key (scheme=1), and
//     only then delete the user_keys row (the threat-model pivot: the
//     master key no longer opens anything of theirs).
//   - !enrolled && !PasswordWrapped: return nil, nil — phases 1–3 behavior.
func (r *SQLResolver) UnlockForLogin(ctx context.Context, uid, password string) ([]byte, error) {
	userID, found, err := r.lookupUserID(ctx, uid)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("encrypt: unlock for login: user %q not found", uid)
	}
	enr, enrolled, err := r.loadEnrollment(ctx, userID)
	if err != nil {
		return nil, err
	}
	hasUK, err := r.hasUserKey(ctx, userID)
	if err != nil {
		return nil, err
	}
	switch {
	case enrolled && !r.PasswordWrapped:
		priv, err := openPrivWithPassword(enr.sealedUK, password, enr.salt, userID, enr.params)
		if err != nil {
			return nil, fmt.Errorf("encrypt: password unwrap failed for user %q — password changed out-of-band? "+
				"the account is inconsistent; resetting the password destroys the enrollment (%w)", uid, err)
		}
		uk, err := r.loadOrCreateUK(ctx, userID)
		if err != nil {
			return nil, err
		}
		if err := r.convertToSymmetric(ctx, userID, priv, uk); err != nil {
			return nil, err
		}
		if _, err := r.db.Exec(ctx, `DELETE FROM user_key_pw WHERE user_id = ?`, userID); err != nil {
			return nil, fmt.Errorf("encrypt: unenroll user %d: %w", userID, err)
		}
		if err := r.deleteTokenWrapsForUser(ctx, userID); err != nil {
			return nil, err
		}
		return nil, nil
	case enrolled:
		priv, err := openPrivWithPassword(enr.sealedUK, password, enr.salt, userID, enr.params)
		if err != nil {
			return nil, fmt.Errorf("encrypt: password unwrap failed for user %q — password changed out-of-band? "+
				"the account is inconsistent; resetting the password destroys the enrollment (%w)", uid, err)
		}
		if hasUK {
			// Interrupted enrollment: finish the conversion, then drop the
			// master-sealed UK. A row that vanishes between the check and
			// the load means a concurrent login completed the conversion
			// (only this code deletes user_keys, after converting) — done.
			uk, found, err := r.loadUK(ctx, userID)
			if err != nil {
				return nil, err
			}
			if found {
				if err := r.convertToBoxes(ctx, userID, uk, enr.pub); err != nil {
					return nil, err
				}
				if _, err := r.db.Exec(ctx, `DELETE FROM user_keys WHERE user_id = ?`, userID); err != nil {
					return nil, fmt.Errorf("encrypt: resume enrollment for user %d: %w", userID, err)
				}
			}
		}
		return priv, nil
	case r.PasswordWrapped:
		priv, pub, err := newKeypair()
		if err != nil {
			return nil, err
		}
		sealed, salt, err := sealPrivForPassword(priv, password, userID, r.KDF)
		if err != nil {
			return nil, err
		}
		if _, err := r.db.Exec(ctx, `
INSERT INTO user_key_pw (user_id, public_key, pw_sealed_uk, pw_kdf, pw_salt, created_ms) VALUES (?, ?, ?, ?, ?, ?)`,
			userID, pub, sealed, r.KDF.marshalKDF(), salt, time.Now().UTC().UnixMilli()); err != nil {
			if database.IsUniqueViolation(r.db.Dialect(), err) {
				// A concurrent login enrolled first: re-run — the enrolled
				// branch opens the winner's row and resumes its conversion.
				return r.UnlockForLogin(ctx, uid, password)
			}
			return nil, fmt.Errorf("encrypt: enroll user %d: %w", userID, err)
		}
		if hasUK {
			// A row that vanishes between the check and the load was
			// converted and deleted by a concurrent login — done.
			uk, found, err := r.loadUK(ctx, userID)
			if err != nil {
				return nil, err
			}
			if found {
				if err := r.convertToBoxes(ctx, userID, uk, pub); err != nil {
					return nil, err
				}
				if _, err := r.db.Exec(ctx, `DELETE FROM user_keys WHERE user_id = ?`, userID); err != nil {
					return nil, fmt.Errorf("encrypt: enroll user %d: %w", userID, err)
				}
			}
		}
		return priv, nil
	default:
		return nil, nil
	}
}

// Enrolled reports whether uid has a user_key_pw row (ADR-0100 enrollment).
// An unknown uid is unenrolled by definition.
func (r *SQLResolver) Enrolled(ctx context.Context, uid string) (bool, error) {
	userID, found, err := r.lookupUserID(ctx, uid)
	if err != nil || !found {
		return false, err
	}
	_, enrolled, err := r.loadEnrollment(ctx, userID)
	return enrolled, err
}

// DestroyEnrollment deletes uid's user_key_pw row and EVERY file_keys wrap
// row the user holds — the reset-password --force semantic (ADR-0100 §6):
// the password-sealed private key is unrecoverable, so the user's v3 files
// are permanently unreadable and their wrap rows are dead weight. The user's
// app_token_keys wraps go too (ADR-0102: the keypair they seal is gone).
// Other users' rows and files.key_uuid are untouched. An unknown uid is a
// no-op.
func (r *SQLResolver) DestroyEnrollment(ctx context.Context, uid string) error {
	userID, found, err := r.lookupUserID(ctx, uid)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM user_key_pw WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("encrypt: destroy enrollment for user %d: %w", userID, err)
	}
	if _, err := r.db.Exec(ctx, `DELETE FROM file_keys WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("encrypt: destroy wrap rows for user %d: %w", userID, err)
	}
	return r.deleteTokenWrapsForUser(ctx, userID)
}

// SealSessionKey seals an unlocked private key for storage on a session row
// (sessions.sealed_uk) under the current ring key; the blob records the key
// ID byte so a master-key rotation does not strand live sessions.
func (r *SQLResolver) SealSessionKey(priv []byte, sessionID string) ([]byte, error) {
	return r.sealSessionCopy(priv, sessionID)
}

// UnsealSessionKey opens a session's sealed key copy. Failures wrap
// ErrIntegrity: a corrupt copy or a mismatched keyring is a wrong-key
// condition, and callers (session verification) must fail closed rather
// than degrade to per-file ErrKeyLocked 403s.
func (r *SQLResolver) UnsealSessionKey(_ context.Context, sessionID string, sealed []byte) ([]byte, error) {
	return r.openSessionCopy(sessionID, sealed)
}

// enrollment is a parsed user_key_pw row.
type enrollment struct {
	pub      []byte
	sealedUK []byte
	salt     []byte
	params   KeyDerivationParams
}

// loadEnrollment reads and parses the user's user_key_pw row; found is
// false when no row exists. A row whose pw_kdf does not parse is an error —
// it cannot be opened and must fail loudly.
func (r *SQLResolver) loadEnrollment(ctx context.Context, userID int64) (enrollment, bool, error) {
	var enr enrollment
	var kdf string
	err := r.db.QueryRow(ctx, `
SELECT public_key, pw_sealed_uk, pw_kdf, pw_salt FROM user_key_pw WHERE user_id = ?`, userID).
		Scan(&enr.pub, &enr.sealedUK, &kdf, &enr.salt)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return enrollment{}, false, nil
		}
		return enrollment{}, false, fmt.Errorf("encrypt: load enrollment for user %d: %w", userID, err)
	}
	params, err := parseKDF(kdf)
	if err != nil {
		return enrollment{}, false, fmt.Errorf("encrypt: enrollment for user %d: %w", userID, err)
	}
	enr.params = params
	return enr, true, nil
}

// publicKeyFor returns the enrolled user's public key; enrolled is false
// when no user_key_pw row exists.
func (r *SQLResolver) publicKeyFor(ctx context.Context, userID int64) (pub []byte, enrolled bool, err error) {
	err = r.db.QueryRow(ctx, `SELECT public_key FROM user_key_pw WHERE user_id = ?`, userID).Scan(&pub)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("encrypt: load public key for user %d: %w", userID, err)
	}
	return pub, true, nil
}

// hasUserKey reports whether the user has a user_keys (symmetric UK) row.
func (r *SQLResolver) hasUserKey(ctx context.Context, userID int64) (bool, error) {
	var n int64
	if err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, userID).Scan(&n); err != nil {
		return false, fmt.Errorf("encrypt: count user keys for user %d: %w", userID, err)
	}
	return n > 0, nil
}

// convertToBoxes re-wraps every scheme=0 wrap row of the user as a scheme=1
// box under pub, opening each FK with the symmetric UK (in hand during
// enrollment). Rows already at scheme=1 are skipped, so an interrupted
// enrollment simply re-runs. A row that fails to open is a broken key chain
// (ErrUnresolvableKey), never a silent skip.
func (r *SQLResolver) convertToBoxes(ctx context.Context, userID int64, uk, pub []byte) error {
	rows, err := r.wrapRowsByScheme(ctx, userID, 0)
	if err != nil {
		return err
	}
	for _, row := range rows {
		fk, err := wrapOpen(uk, row.wrapped, fkAD(row.keyUUID, userID))
		if err != nil {
			return fmt.Errorf("encrypt: convert wrap %s for user %d: %w",
				hex.EncodeToString(row.keyUUID[:]), userID, ErrUnresolvableKey)
		}
		blob, err := boxWrap(pub, fk, row.keyUUID, userID)
		if err != nil {
			return err
		}
		if _, err := r.db.Exec(ctx, `
UPDATE file_keys SET wrapped_fk = ?, scheme = 1 WHERE key_uuid = ? AND user_id = ?`,
			blob, row.keyUUID[:], userID); err != nil {
			return fmt.Errorf("encrypt: convert wrap %s for user %d: %w",
				hex.EncodeToString(row.keyUUID[:]), userID, err)
		}
	}
	return nil
}

// convertToSymmetric re-wraps every scheme=1 box row of the user back to a
// scheme=0 symmetric wrap under uk, opening each box with the unlocked
// private key (unenrollment). Idempotent: a re-run finds no scheme=1 rows.
// A box that fails to open is tampered or the key is wrong (ErrIntegrity).
func (r *SQLResolver) convertToSymmetric(ctx context.Context, userID int64, priv, uk []byte) error {
	rows, err := r.wrapRowsByScheme(ctx, userID, 1)
	if err != nil {
		return err
	}
	for _, row := range rows {
		fk, err := boxOpen(priv, row.wrapped, row.keyUUID, userID)
		if err != nil {
			return fmt.Errorf("encrypt: convert box %s for user %d: %w",
				hex.EncodeToString(row.keyUUID[:]), userID, errors.Join(err, ErrIntegrity))
		}
		wrapped, err := wrapSeal(uk, fk, fkAD(row.keyUUID, userID))
		if err != nil {
			return err
		}
		if _, err := r.db.Exec(ctx, `
UPDATE file_keys SET wrapped_fk = ?, scheme = 0 WHERE key_uuid = ? AND user_id = ?`,
			wrapped, row.keyUUID[:], userID); err != nil {
			return fmt.Errorf("encrypt: convert box %s for user %d: %w",
				hex.EncodeToString(row.keyUUID[:]), userID, err)
		}
	}
	return nil
}

// userWrapRow is one file_keys row of a user.
type userWrapRow struct {
	keyUUID [keyUUIDSize]byte
	wrapped []byte
}

// wrapRowsByScheme lists the user's file_keys rows at one scheme.
func (r *SQLResolver) wrapRowsByScheme(ctx context.Context, userID int64, scheme int) ([]userWrapRow, error) {
	rows, err := r.db.Query(ctx, `
SELECT key_uuid, wrapped_fk FROM file_keys WHERE user_id = ? AND scheme = ? ORDER BY key_uuid`, userID, scheme)
	if err != nil {
		return nil, fmt.Errorf("encrypt: list wraps for user %d: %w", userID, err)
	}
	defer rows.Close()
	var out []userWrapRow
	for rows.Next() {
		var row userWrapRow
		var uuid []byte
		if err := rows.Scan(&uuid, &row.wrapped); err != nil {
			return nil, fmt.Errorf("encrypt: list wraps for user %d: %w", userID, err)
		}
		if len(uuid) != keyUUIDSize {
			return nil, fmt.Errorf("encrypt: wrap row for user %d carries a %d-byte key uuid", userID, len(uuid))
		}
		copy(row.keyUUID[:], uuid)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("encrypt: list wraps for user %d: %w", userID, err)
	}
	return out, nil
}
