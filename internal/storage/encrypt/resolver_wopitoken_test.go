package encrypt

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// insertWOPIToken seeds a wopi_tokens row directly (minting is the wopi
// layer's; these tests need the row, not the flow).
func insertWOPIToken(t *testing.T, db database.DB, uid, token string, fileID int64) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
INSERT INTO wopi_tokens (token, uid, file_id, can_write, expires_at)
VALUES (?, ?, ?, 1, 0)`, token, uid, fileID); err != nil {
		t.Fatal(err)
	}
}

const (
	wopiTokOne = "wopi-token-one-0123456789abcdef"
	wopiTokTwo = "wopi-token-two-0123456789abcdef"
)

// enrollForWOPITest enrolls uid and returns the unlocked private key plus a
// wopi_tokens row (no wrap yet).
func enrollForWOPITest(t *testing.T, db database.DB, res *SQLResolver, uid, token string, fileID int64) (priv []byte) {
	t.Helper()
	ctx := context.Background()
	seedResolverUser(t, db, uid)
	priv, err := res.UnlockForLogin(ctx, uid, uid+"-pw")
	if err != nil {
		t.Fatal(err)
	}
	insertWOPIToken(t, db, uid, token, fileID)
	return priv
}

// TestWrapKeyForWOPITokenRoundTrip pins the ADR-0107 construction: a wrap
// seals the private key under the token KEK (60-byte blob, 16-byte salt,
// AD binding user AND file id), opens with the same raw token and file id,
// and fails closed (ErrIntegrity) on the wrong token, the wrong file id, or
// a tampered blob.
func TestWrapKeyForWOPITokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	priv := enrollForWOPITest(t, db, res, "alice", wopiTokOne, 7)

	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, priv); err != nil {
		t.Fatal(err)
	}
	var sealed, salt []byte
	if err := db.QueryRow(ctx, `
SELECT sealed_uk, salt FROM wopi_token_keys WHERE token = ?`, wopiTokOne).Scan(&sealed, &salt); err != nil {
		t.Fatal(err)
	}
	if len(sealed) != pwSealedUKSize {
		t.Errorf("sealed_uk = %d bytes, want %d", len(sealed), pwSealedUKSize)
	}
	if len(salt) != tokenSaltSize {
		t.Errorf("salt = %d bytes, want %d", len(salt), tokenSaltSize)
	}

	got, err := res.UnlockForWOPIToken(ctx, wopiTokOne, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Error("UnlockForWOPIToken returned a different key than wrapped")
	}

	// An unknown token is nil, nil — unlike app tokens there is no hash
	// indirection: the token IS the row key, so a wrong token never reaches
	// the open step (the re-keyed-row case below does).
	if got, err := res.UnlockForWOPIToken(ctx, wopiTokTwo, 7); got != nil || err != nil {
		t.Errorf("unknown token = %v, %v; want nil, nil", got, err)
	}
	// File-id binding: the same token presented for a DIFFERENT file id must
	// not open the wrap (the AD binds the file id).
	if _, err := res.UnlockForWOPIToken(ctx, wopiTokOne, 8); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong file id err = %v, want ErrIntegrity", err)
	}
	// A tampered blob fails the same way. Flip a bit (not a fixed 0xff
	// overwrite): sealed[0] is random, so a fixed byte is a 1/256 no-op.
	tampered := bytes.Clone(sealed)
	tampered[0] ^= 0xff
	if _, err := db.Exec(ctx, `UPDATE wopi_token_keys SET sealed_uk = ? WHERE token = ?`,
		tampered, wopiTokOne); err != nil {
		t.Fatal(err)
	}
	if _, err := res.UnlockForWOPIToken(ctx, wopiTokOne, 7); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered wrap err = %v, want ErrIntegrity", err)
	}
}

// TestUnlockForWOPITokenRekeyedRow pins the wrong-KEK failure through the
// public path: a wrap row whose token key no longer matches the KEK it was
// sealed under (a wrap row re-keyed onto another live token) fails closed
// with ErrIntegrity — never a silent nil.
func TestUnlockForWOPITokenRekeyedRow(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	priv := enrollForWOPITest(t, db, res, "alice", wopiTokOne, 7)
	insertWOPIToken(t, db, "alice", wopiTokTwo, 7)
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, priv); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE wopi_token_keys SET token = ? WHERE token = ?`, wopiTokTwo, wopiTokOne); err != nil {
		t.Fatal(err)
	}
	if _, err := res.UnlockForWOPIToken(ctx, wopiTokTwo, 7); !errors.Is(err, ErrIntegrity) {
		t.Errorf("re-keyed wrap err = %v, want ErrIntegrity", err)
	}
}

// TestWrapKeyForWOPITokenEdgeCases pins the retry-safe and validation
// behavior: re-wrapping the same token is a no-op (the FIRST wrap survives),
// an unknown token is an error, and the private key must be exactly 32
// bytes.
func TestWrapKeyForWOPITokenEdgeCases(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	priv := enrollForWOPITest(t, db, res, "alice", wopiTokOne, 7)

	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, priv); err != nil {
		t.Fatal(err)
	}
	// Unique re-wrap (mint retry): no-op, still exactly one row, the FIRST
	// wrap survives — even when the retry carries different key material.
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, testKey(t)); err != nil {
		t.Fatalf("re-wrap = %v, want no-op nil", err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM wopi_token_keys`); n != 1 {
		t.Fatalf("wrap rows after re-wrap = %d, want 1", n)
	}
	got, err := res.UnlockForWOPIToken(ctx, wopiTokOne, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Error("re-wrap replaced the first wrap's key")
	}

	if err := res.WrapKeyForWOPIToken(ctx, "no-such-token", 7, priv); err == nil {
		t.Error("unknown token must be an error")
	}
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, []byte("short")); err == nil {
		t.Error("a non-32-byte private key must be an error")
	}
}

// TestUnlockForWOPITokenNoRow pins the no-wrap behavior: a token without a
// wrap row (pre-0026 or keylessly minted) unlocks to nil, nil — the callback
// attaches no key and does not fail.
func TestUnlockForWOPITokenNoRow(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	got, err := res.UnlockForWOPIToken(ctx, "no-such-token", 7)
	if err != nil || got != nil {
		t.Errorf("UnlockForWOPIToken without a row = %v, %v; want nil, nil", got, err)
	}
	// A token ROW without a wrap row (keyless mint) behaves the same.
	seedResolverUser(t, db, "alice")
	insertWOPIToken(t, db, "alice", wopiTokOne, 7)
	got, err = res.UnlockForWOPIToken(ctx, wopiTokOne, 7)
	if err != nil || got != nil {
		t.Errorf("UnlockForWOPIToken with a bare token row = %v, %v; want nil, nil", got, err)
	}
}

// TestDeleteWOPITokenKeys pins the GC's bulk revocation: the named tokens'
// wraps go, other tokens' wraps stay, an empty slice is a no-op, and a
// re-delete reports zero rows.
func TestDeleteWOPITokenKeys(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	enrollForWOPITest(t, db, res, "alice", wopiTokOne, 7)
	privB := enrollForWOPITest(t, db, res, "bob", wopiTokTwo, 9)
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, testKey(t)); err != nil {
		t.Fatal(err)
	}
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokTwo, 9, privB); err != nil {
		t.Fatal(err)
	}

	if n, err := res.DeleteWOPITokenKeys(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty delete = %d, %v; want 0, nil", n, err)
	}
	n, err := res.DeleteWOPITokenKeys(ctx, []string{wopiTokOne, "never-existed"})
	if err != nil || n != 1 {
		t.Fatalf("batch delete = %d, %v; want 1, nil", n, err)
	}
	if got, err := res.UnlockForWOPIToken(ctx, wopiTokOne, 7); got != nil || err != nil {
		t.Errorf("deleted token unlock = %v, %v; want nil, nil", got, err)
	}
	got, err := res.UnlockForWOPIToken(ctx, wopiTokTwo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, privB) {
		t.Error("batch delete took the other token's wrap")
	}
	if n, err := res.DeleteWOPITokenKeys(ctx, []string{wopiTokOne}); err != nil || n != 0 {
		t.Errorf("re-delete = %d, %v; want 0, nil", n, err)
	}
}

// TestPruneWOPITokenKeys pins the orphan purge: wraps whose token row is
// gone are reaped; wraps of live tokens stay.
func TestPruneWOPITokenKeys(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	privA := enrollForWOPITest(t, db, res, "alice", wopiTokOne, 7)
	privB := enrollForWOPITest(t, db, res, "bob", wopiTokTwo, 9)
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokOne, 7, privA); err != nil {
		t.Fatal(err)
	}
	if err := res.WrapKeyForWOPIToken(ctx, wopiTokTwo, 9, privB); err != nil {
		t.Fatal(err)
	}
	// alice's token row vanishes without the GC's wrap delete (a failed
	// key-side sweep): her wrap is an orphan, bob's is not.
	if _, err := db.Exec(ctx, `DELETE FROM wopi_tokens WHERE token = ?`, wopiTokOne); err != nil {
		t.Fatal(err)
	}
	n, err := res.PruneWOPITokenKeys(ctx)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1, nil", n, err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM wopi_token_keys`); n != 1 {
		t.Errorf("wrap rows after prune = %d, want 1", n)
	}
	got, err := res.UnlockForWOPIToken(ctx, wopiTokTwo, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, privB) {
		t.Error("prune took the live token's wrap")
	}
	if n, err := res.PruneWOPITokenKeys(ctx); err != nil || n != 0 {
		t.Errorf("second prune = %d, %v; want 0, nil", n, err)
	}
}
