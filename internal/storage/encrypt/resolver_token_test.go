package encrypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// insertAppPassword seeds an app_passwords row directly (token issuance is
// the auth layer's; these tests need the row, not the flow).
func insertAppPassword(t *testing.T, db database.DB, userID int64, id, hash string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
INSERT INTO app_passwords (id, user_id, token_hash, login_name, name, type, created_at)
VALUES (?, ?, ?, ?, '', 1, 0)`, id, userID, hash, id); err != nil {
		t.Fatal(err)
	}
}

const (
	tokenRawOne = "raw-token-one-0123456789abcdef"
	tokenRawTwo = "raw-token-two-0123456789abcdef"
)

// enrollForTokenTest enrolls uid and returns the unlocked private key plus a
// token row carrying a wrap of it.
func enrollForTokenTest(t *testing.T, db database.DB, res *SQLResolver, uid, tokenID, tokenHash, raw string) (priv []byte, userID int64) {
	t.Helper()
	ctx := context.Background()
	userID = seedResolverUser(t, db, uid)
	priv, err := res.UnlockForLogin(ctx, uid, uid+"-pw")
	if err != nil {
		t.Fatal(err)
	}
	insertAppPassword(t, db, userID, tokenID, tokenHash)
	if err := res.WrapKeyForToken(ctx, tokenID, raw, priv); err != nil {
		t.Fatal(err)
	}
	return priv, userID
}

// TestWrapKeyForTokenRoundTrip pins the ADR-0102 construction: a wrap seals
// the private key under the token KEK (60-byte blob, 16-byte salt), opens
// with the same raw token through the stored hash, and fails closed
// (ErrIntegrity) on the wrong token.
func TestWrapKeyForTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	res := pwResolver(t, db, true)
	priv, err := res.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	insertAppPassword(t, db, aliceID, "tok-1", "hash-1")

	if err := res.WrapKeyForToken(ctx, "tok-1", tokenRawOne, priv); err != nil {
		t.Fatal(err)
	}
	var sealed, salt []byte
	if err := db.QueryRow(ctx, `
SELECT sealed_uk, salt FROM app_token_keys WHERE app_password_id = 'tok-1'`).Scan(&sealed, &salt); err != nil {
		t.Fatal(err)
	}
	if len(sealed) != pwSealedUKSize {
		t.Errorf("sealed_uk = %d bytes, want %d", len(sealed), pwSealedUKSize)
	}
	if len(salt) != tokenSaltSize {
		t.Errorf("salt = %d bytes, want %d", len(salt), tokenSaltSize)
	}

	got, err := res.UnlockForToken(ctx, "hash-1", tokenRawOne)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Error("UnlockForToken returned a different key than wrapped")
	}

	// Wrong raw token: fail closed (ErrIntegrity), never a silent nil.
	if _, err := res.UnlockForToken(ctx, "hash-1", tokenRawTwo); !errors.Is(err, ErrIntegrity) {
		t.Errorf("wrong token err = %v, want ErrIntegrity", err)
	}
	// A tampered blob fails the same way.
	if _, err := db.Exec(ctx, `UPDATE app_token_keys SET sealed_uk = ? WHERE app_password_id = 'tok-1'`,
		append([]byte{0xff}, sealed[1:]...)); err != nil {
		t.Fatal(err)
	}
	if _, err := res.UnlockForToken(ctx, "hash-1", tokenRawOne); !errors.Is(err, ErrIntegrity) {
		t.Errorf("tampered wrap err = %v, want ErrIntegrity", err)
	}
}

// TestWrapKeyForTokenEdgeCases pins the retry-safe and validation behavior:
// re-wrapping the same token id is a no-op, an unknown id is an error, and
// the private key must be exactly 32 bytes.
func TestWrapKeyForTokenEdgeCases(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	res := pwResolver(t, db, true)
	priv, err := res.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	insertAppPassword(t, db, aliceID, "tok-1", "hash-1")

	if err := res.WrapKeyForToken(ctx, "tok-1", tokenRawOne, priv); err != nil {
		t.Fatal(err)
	}
	// Unique re-wrap (grant retry): no-op, still exactly one row, the FIRST
	// wrap (sealed under the original token) survives.
	if err := res.WrapKeyForToken(ctx, "tok-1", tokenRawTwo, priv); err != nil {
		t.Fatalf("re-wrap = %v, want no-op nil", err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM app_token_keys`); n != 1 {
		t.Fatalf("wrap rows after re-wrap = %d, want 1", n)
	}
	if _, err := res.UnlockForToken(ctx, "hash-1", tokenRawOne); err != nil {
		t.Errorf("original token no longer opens the wrap: %v", err)
	}

	if err := res.WrapKeyForToken(ctx, "no-such-id", tokenRawOne, priv); err == nil {
		t.Error("unknown app password id must be an error")
	}
	if err := res.WrapKeyForToken(ctx, "tok-1", tokenRawOne, []byte("short")); err == nil {
		t.Error("a non-32-byte private key must be an error")
	}
}

// TestUnlockForTokenNoRow pins the no-wrap behavior: a token without a wrap
// row (pre-enrollment or imported) unlocks to nil, nil — the verifier
// attaches no key and does not fail.
func TestUnlockForTokenNoRow(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	got, err := res.UnlockForToken(ctx, "no-such-hash", tokenRawOne)
	if err != nil || got != nil {
		t.Errorf("UnlockForToken without a row = %v, %v; want nil, nil", got, err)
	}
}

// TestUnlockForTokenStoredHashLookup pins the two-call caller pattern: the
// wrap row is keyed by the row's STORED token hash, so a lookup under the
// primary hash misses a legacy-hashed token's wrap and the legacy hash hits
// it (the bearer verifier tries primary first, legacy second).
func TestUnlockForTokenStoredHashLookup(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	priv, _ := enrollForTokenTest(t, db, res, "alice", "tok-1", "legacy-hash-1", tokenRawOne)

	if got, err := res.UnlockForToken(ctx, "primary-hash-1", tokenRawOne); got != nil || err != nil {
		t.Errorf("primary-hash lookup of a legacy row = %v, %v; want nil, nil", got, err)
	}
	got, err := res.UnlockForToken(ctx, "legacy-hash-1", tokenRawOne)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, priv) {
		t.Error("legacy-hash lookup did not open the wrap")
	}
}

// TestOnTokenDeleted pins cryptographic revocation: deleting the token's
// wrap makes the token open nothing; a missing row is a no-op.
func TestOnTokenDeleted(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	enrollForTokenTest(t, db, res, "alice", "tok-1", "hash-1", tokenRawOne)

	if err := res.OnTokenDeleted(ctx, "tok-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := res.UnlockForToken(ctx, "hash-1", tokenRawOne); got != nil || err != nil {
		t.Errorf("post-delete unlock = %v, %v; want nil, nil", got, err)
	}
	if err := res.OnTokenDeleted(ctx, "tok-1"); err != nil {
		t.Errorf("second delete = %v, want no-op nil", err)
	}
	if err := res.OnTokenDeleted(ctx, "never-existed"); err != nil {
		t.Errorf("unknown id delete = %v, want no-op nil", err)
	}
}

// tokenWrapPurgeSetup enrolls alice and bob and gives each a wrapped token.
func tokenWrapPurgeSetup(t *testing.T) (database.DB, *SQLResolver) {
	t.Helper()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	enrollForTokenTest(t, db, res, "alice", "tok-a", "hash-a", tokenRawOne)
	enrollForTokenTest(t, db, res, "bob", "tok-b", "hash-b", tokenRawTwo)
	return db, res
}

// TestTokenWrapPurgePaths pins the four paths that remove a user's token
// wraps (unenroll, DestroyEnrollment, OnUserDeleted, PruneStaleKeys) — each
// removes exactly the target user's wraps and nothing else.
func TestTokenWrapPurgePaths(t *testing.T) {
	ctx := context.Background()
	countWraps := func(t *testing.T, db database.DB) (alice, bob int64) {
		t.Helper()
		return rowCount(t, db, `SELECT COUNT(*) FROM app_token_keys WHERE app_password_id = 'tok-a'`),
			rowCount(t, db, `SELECT COUNT(*) FROM app_token_keys WHERE app_password_id = 'tok-b'`)
	}

	t.Run("unenroll", func(t *testing.T) {
		db, res := tokenWrapPurgeSetup(t)
		res.PasswordWrapped = false
		if _, err := res.UnlockForLogin(ctx, "alice", "alice-pw"); err != nil {
			t.Fatal(err)
		}
		if a, b := countWraps(t, db); a != 0 || b != 1 {
			t.Errorf("wraps after unenroll = alice:%d bob:%d, want 0, 1", a, b)
		}
	})

	t.Run("DestroyEnrollment", func(t *testing.T) {
		db, res := tokenWrapPurgeSetup(t)
		if err := res.DestroyEnrollment(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		if a, b := countWraps(t, db); a != 0 || b != 1 {
			t.Errorf("wraps after DestroyEnrollment = alice:%d bob:%d, want 0, 1", a, b)
		}
	})

	t.Run("OnUserDeleted", func(t *testing.T) {
		db, res := tokenWrapPurgeSetup(t)
		if err := res.OnUserDeleted(ctx, "alice"); err != nil {
			t.Fatal(err)
		}
		if a, b := countWraps(t, db); a != 0 || b != 1 {
			t.Errorf("wraps after OnUserDeleted = alice:%d bob:%d, want 0, 1", a, b)
		}
	})

	t.Run("PruneStaleKeys", func(t *testing.T) {
		db, res := tokenWrapPurgeSetup(t)
		// The token row vanishes without the hook (raw SQL, a cascade on the
		// user delete, a failed best-effort hook): the wrap is an orphan.
		if _, err := db.Exec(ctx, `DELETE FROM app_passwords WHERE id = 'tok-a'`); err != nil {
			t.Fatal(err)
		}
		pruned, err := res.PruneStaleKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if pruned != (PruneStats{UserKeys: 0, FileKeys: 0, TokenKeys: 1}) {
			t.Errorf("pruned = %+v, want {0 0 1}", pruned)
		}
		if a, b := countWraps(t, db); a != 0 || b != 1 {
			t.Errorf("wraps after prune = alice:%d bob:%d, want 0, 1", a, b)
		}
	})
}

// TestTokenWrapInventory pins the ADR-0102 status counts: app_token_keys rows
// are counted, and app passwords of ENROLLED users without a wrap are named
// (unenrolled users' tokens are not — they need no wrap).
func TestTokenWrapInventory(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	// alice: enrolled, one wrapped token and one bare token.
	_, aliceID := enrollForTokenTest(t, db, res, "alice", "tok-a", "hash-a", tokenRawOne)
	insertAppPassword(t, db, aliceID, "tok-a2", "hash-a2")
	// bob: unenrolled, one bare token (must not count as unwrapped-enrolled).
	bobID := seedResolverUser(t, db, "bob")
	insertAppPassword(t, db, bobID, "tok-b", "hash-b")

	inv, err := res.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inv.TokenWraps != 1 {
		t.Errorf("TokenWraps = %d, want 1", inv.TokenWraps)
	}
	if inv.UnwrappedEnrolledTokens != 1 {
		t.Errorf("UnwrappedEnrolledTokens = %d, want 1 (alice's bare token only)", inv.UnwrappedEnrolledTokens)
	}
}

// TestUnlockForTokenConcurrentDelete pins the revoke-mid-request race under
// -race: concurrent UnlockForToken and OnTokenDeleted must never error or
// panic — each unlock legally returns either the key (before the delete) or
// nil (after it).
func TestUnlockForTokenConcurrentDelete(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	res := pwResolver(t, db, true)
	priv, _ := enrollForTokenTest(t, db, res, "alice", "tok-1", "hash-1", tokenRawOne)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers+1)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := res.UnlockForToken(ctx, "hash-1", tokenRawOne)
			if err != nil {
				errs <- fmt.Errorf("unlock during revoke: %w", err)
				return
			}
			if got != nil && !bytes.Equal(got, priv) {
				errs <- fmt.Errorf("unlock during revoke returned a wrong key")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := res.OnTokenDeleted(ctx, "tok-1"); err != nil {
			errs <- fmt.Errorf("delete: %w", err)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM app_token_keys`); n != 0 {
		t.Errorf("wrap rows after the race = %d, want 0", n)
	}
}
