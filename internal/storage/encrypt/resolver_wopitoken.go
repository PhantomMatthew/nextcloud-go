package encrypt

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// WOPI-token key wraps (ADR-0107, mirroring ADR-0102's app-token wraps): each
// WOPI access token minted by an enrolled user carries its own wrap of the
// user's X25519 private key, sealed at mint time (the session-authed mint
// holds both the raw token and the unlocked key) and opened per anonymous
// Collabora callback by the WOPI files handler. The token is the KEK source:
// it is ≥256-bit random and stored PLAINTEXT (ADR-0106's bearer precedent),
// so the wrap rows key off the token itself — no hash dance. Token expiry
// (the GC sweep) deletes the wrap — cryptographic revocation. All methods
// are concrete on SQLResolver — the narrow KeyResolver interface is
// untouched.
const wopiTokenWrapAD = "NCGOWK1" // wopi-token wrap: "NCGOWK1" || be64(user_id) || be64(file_id)

// wopiTokenAD is the HKDF info and AEAD associated data binding a WOPI token
// wrap to its owner AND its file: "NCGOWK1" || bigendian-uint64(user_id) ||
// bigendian-uint64(file_id). The file id joins the app-token AD shape
// ("NCGOAK1" || be64(user_id), resolver_token.go) because a WOPI token is
// file-scoped; a distinct domain string keeps the two wrap families
// incompatible.
func wopiTokenAD(userID, fileID int64) []byte {
	ad := make([]byte, 0, len(wopiTokenWrapAD)+16)
	ad = append(ad, wopiTokenWrapAD...)
	//nolint:gosec // G115: user and filecache IDs are positive serial keys
	ad = binary.BigEndian.AppendUint64(ad, uint64(userID))
	//nolint:gosec // G115: user and filecache IDs are positive serial keys
	return binary.BigEndian.AppendUint64(ad, uint64(fileID))
}

// wopiTokenKEK derives the 32-byte WOPI token KEK: HKDF-SHA256(tokenRaw,
// salt, wopiTokenAD(userID, fileID)).
func wopiTokenKEK(tokenRaw string, salt []byte, userID, fileID int64) []byte {
	return hkdfSHA256Salt([]byte(tokenRaw), salt, wopiTokenAD(userID, fileID))
}

// WrapKeyForWOPIToken seals priv (the user's unlocked 32-byte X25519 private
// key) under the WOPI token's KEK with a fresh 16-byte salt and stores the
// 60-byte blob (nonce(12) || AES-256-GCM(KEK, priv, wopiTokenAD)) on a new
// wopi_token_keys row — wrap-at-mint (ADR-0107). The token's owning user is
// resolved from the wopi_tokens row; an unknown token is an error (mint wires
// the token it just inserted). A unique violation is a no-op, so mint retries
// are safe. The raw token is a bearer credential: error strings name the
// failure, never the token.
func (r *SQLResolver) WrapKeyForWOPIToken(ctx context.Context, tokenRaw string, fileID int64, priv []byte) error {
	if len(priv) != x25519KeySize {
		return fmt.Errorf("encrypt: private key must be %d bytes, got %d", x25519KeySize, len(priv))
	}
	var userID int64
	err := r.db.QueryRow(ctx, `
SELECT u.id FROM wopi_tokens t JOIN users u ON u.uid = t.uid WHERE t.token = ?`, tokenRaw).Scan(&userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return errors.New("encrypt: wrap key for wopi token: token not found")
		}
		return fmt.Errorf("encrypt: wrap key for wopi token: %w", err)
	}
	salt := make([]byte, tokenSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("encrypt: generate wopi token salt: %w", err)
	}
	sealed, err := wrapSeal(wopiTokenKEK(tokenRaw, salt, userID, fileID), priv, wopiTokenAD(userID, fileID))
	if err != nil {
		return err
	}
	if _, err := r.db.Exec(ctx, `
INSERT INTO wopi_token_keys (token, sealed_uk, salt, created_ms) VALUES (?, ?, ?, ?)`,
		tokenRaw, sealed, salt, time.Now().UTC().UnixMilli()); err != nil {
		if database.IsUniqueViolation(r.db.Dialect(), err) {
			return nil
		}
		return fmt.Errorf("encrypt: insert wopi token key wrap: %w", err)
	}
	return nil
}

// UnlockForWOPIToken opens the WOPI token's wrap of the user's private key
// (ADR-0107) and returns it; the files handler attaches it to the anonymous
// callback's request principal. The wrap row keys off the plaintext token
// directly (WOPI tokens are stored plaintext, ADR-0106). No row is nil, nil
// — a pre-0026 or keylessly-minted token carries no wrap and the callback
// simply gets no key (the documented 403 boundary). An open failure wraps
// ErrIntegrity (fail-closed, mirroring the app-token semantics): a corrupt
// wrap must not silently degrade to per-file ErrKeyLocked 403s.
func (r *SQLResolver) UnlockForWOPIToken(ctx context.Context, tokenRaw string, fileID int64) ([]byte, error) {
	var sealed, salt []byte
	var userID int64
	err := r.db.QueryRow(ctx, `
SELECT k.sealed_uk, k.salt, u.id FROM wopi_token_keys k
JOIN wopi_tokens t ON t.token = k.token
JOIN users u ON u.uid = t.uid
WHERE k.token = ?`, tokenRaw).Scan(&sealed, &salt, &userID)
	if err != nil {
		if errors.Is(err, database.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("encrypt: unlock for wopi token: %w", err)
	}
	priv, err := wrapOpen(wopiTokenKEK(tokenRaw, salt, userID, fileID), sealed, wopiTokenAD(userID, fileID))
	if err != nil {
		return nil, fmt.Errorf("encrypt: wopi token key wrap authentication failed: %w", errors.Join(err, ErrIntegrity))
	}
	return priv, nil
}

// DeleteWOPITokenKeys deletes the given tokens' key wraps in one batch —
// cryptographic revocation (ADR-0107), fired by the WOPI GC sweep for the
// exact tokens it reaps: without the rows, an expired token opens nothing
// even while a copy of the wrap table survives. An empty slice is a no-op.
func (r *SQLResolver) DeleteWOPITokenKeys(ctx context.Context, tokens []string) (int64, error) {
	if len(tokens) == 0 {
		return 0, nil
	}
	marks := strings.Repeat("?,", len(tokens))
	marks = marks[:len(marks)-1]
	args := make([]any, 0, len(tokens))
	for _, tok := range tokens {
		args = append(args, tok)
	}
	res, err := r.db.Exec(ctx, `DELETE FROM wopi_token_keys WHERE token IN (`+marks+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("encrypt: delete wopi token key wraps: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("encrypt: delete wopi token key wraps: %w", err)
	}
	return n, nil
}

// PruneWOPITokenKeys deletes wrap rows whose token no longer exists — the
// orphan safety net (mirroring PruneStaleKeys' app_token_keys purge,
// ADR-0102 §5) for wraps a failed GC delete left behind.
func (r *SQLResolver) PruneWOPITokenKeys(ctx context.Context) (int64, error) {
	res, err := r.db.Exec(ctx, `
DELETE FROM wopi_token_keys WHERE token NOT IN (SELECT token FROM wopi_tokens)`)
	if err != nil {
		return 0, fmt.Errorf("encrypt: prune stale wopi token key wraps: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("encrypt: prune stale wopi token key wraps: %w", err)
	}
	return n, nil
}
