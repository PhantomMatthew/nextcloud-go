package encrypt

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// App-token key wraps (ADR-0100 §3/§7, built in ADR-0102): each app-password
// token of an enrolled user carries its own wrap of the user's X25519 private
// key, sealed at token issuance (the grant request holds both the raw token
// and the unlocked key) and opened per request by the token verifier. The
// token is the KEK source: it is ≥256-bit random, so HKDF-SHA256 needs no
// stretching (argon2id per request would be a self-DoS vector). Revoking the
// token deletes the wrap — cryptographic revocation. All methods are concrete
// on SQLResolver — the narrow KeyResolver interface is untouched.
const (
	tokenWrapAD = "NCGOAK1" // app-token wrap: "NCGOAK1" || be64(user_id)

	tokenSaltSize = 16
)

// tokenAD is the HKDF info and AEAD associated data binding a token wrap to
// its owner: "NCGOAK1" || bigendian-uint64(user_id).
func tokenAD(userID int64) []byte {
	ad := make([]byte, 0, len(tokenWrapAD)+8)
	ad = append(ad, tokenWrapAD...)
	//nolint:gosec // G115: user IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(userID))
}

// tokenKEK derives the 32-byte token KEK: HKDF-SHA256(tokenRaw, salt,
// tokenAD(userID)).
func tokenKEK(tokenRaw string, salt []byte, userID int64) []byte {
	return hkdfSHA256Salt([]byte(tokenRaw), salt, tokenAD(userID))
}

// WrapKeyForToken seals priv (the user's unlocked 32-byte X25519 private key)
// under the app-password token's KEK with a fresh 16-byte salt and stores the
// 60-byte blob (nonce(12) || AES-256-GCM(KEK, priv, tokenAD)) on a new
// app_token_keys row — wrap-at-issuance (ADR-0102). The raw token itself is
// never persisted. The token's owning user is resolved from the
// app_passwords row; an unknown app password id is an error (issuance wires
// the id of the row it just inserted). A unique violation is a no-op, so
// grant retries are safe.
func (r *SQLResolver) WrapKeyForToken(ctx context.Context, appPasswordID, tokenRaw string, priv []byte) error {
	if len(priv) != x25519KeySize {
		return fmt.Errorf("encrypt: private key must be %d bytes, got %d", x25519KeySize, len(priv))
	}
	var userID int64
	err := r.db.QueryRow(ctx, `SELECT user_id FROM app_passwords WHERE id = ?`, appPasswordID).Scan(&userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return fmt.Errorf("encrypt: wrap key for token: app password %q not found", appPasswordID)
		}
		return fmt.Errorf("encrypt: wrap key for token %q: %w", appPasswordID, err)
	}
	salt := make([]byte, tokenSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("encrypt: generate token salt: %w", err)
	}
	sealed, err := wrapSeal(tokenKEK(tokenRaw, salt, userID), priv, tokenAD(userID))
	if err != nil {
		return err
	}
	if _, err := r.db.Exec(ctx, `
INSERT INTO app_token_keys (app_password_id, sealed_uk, salt, created_ms) VALUES (?, ?, ?, ?)`,
		appPasswordID, sealed, salt, time.Now().UTC().UnixMilli()); err != nil {
		if database.IsUniqueViolation(r.db.Dialect(), err) {
			return nil
		}
		return fmt.Errorf("encrypt: insert token key wrap %q: %w", appPasswordID, err)
	}
	return nil
}

// UnlockForToken opens the app-password token's wrap of the user's private
// key (ADR-0102) and returns it; the verifier attaches it to the request
// principal. The wrap row is found through the token's STORED hash (the
// caller knows which of the primary/legacy hashes matched, or tries both). No
// row is nil, nil — a pre-enrollment or imported token carries no wrap and
// the request simply gets no key. An open failure wraps ErrIntegrity
// (fail-closed, mirroring the session-copy semantics): a corrupt wrap must
// not silently degrade to per-file ErrKeyLocked 403s.
func (r *SQLResolver) UnlockForToken(ctx context.Context, tokenHash, tokenRaw string) ([]byte, error) {
	var sealed, salt []byte
	var userID int64
	err := r.db.QueryRow(ctx, `
SELECT k.sealed_uk, k.salt, p.user_id FROM app_token_keys k
JOIN app_passwords p ON p.id = k.app_password_id
WHERE p.token_hash = ?`, tokenHash).Scan(&sealed, &salt, &userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("encrypt: unlock for token: %w", err)
	}
	priv, err := wrapOpen(tokenKEK(tokenRaw, salt, userID), sealed, tokenAD(userID))
	if err != nil {
		return nil, fmt.Errorf("encrypt: token key wrap authentication failed: %w", errors.Join(err, ErrIntegrity))
	}
	return priv, nil
}

// OnTokenDeleted deletes the revoked app-password token's key wrap —
// cryptographic revocation (ADR-0102): without the row, the token opens
// nothing even if a client still presents it. A missing row is a no-op.
// Satisfies auth.TokenKeysHook structurally (wired best-effort: the token row
// is gone first, and PruneStaleKeys' orphan purge reaps any wrap a failed
// hook leaves behind).
func (r *SQLResolver) OnTokenDeleted(ctx context.Context, appPasswordID string) error {
	if _, err := r.db.Exec(ctx, `DELETE FROM app_token_keys WHERE app_password_id = ?`, appPasswordID); err != nil {
		return fmt.Errorf("encrypt: delete token key wrap %q: %w", appPasswordID, err)
	}
	return nil
}

// deleteTokenWrapsForUser deletes every app_token_keys row belonging to the
// user's app passwords — the shared purge of the unenroll, DestroyEnrollment,
// and OnUserDeleted paths: once the keypair (and with it the private key) is
// gone, the token wraps are dead weight and key hygiene.
func (r *SQLResolver) deleteTokenWrapsForUser(ctx context.Context, userID int64) error {
	if _, err := r.db.Exec(ctx, `
DELETE FROM app_token_keys WHERE app_password_id IN (SELECT id FROM app_passwords WHERE user_id = ?)`, userID); err != nil {
		return fmt.Errorf("encrypt: delete token key wraps for user %d: %w", userID, err)
	}
	return nil
}
