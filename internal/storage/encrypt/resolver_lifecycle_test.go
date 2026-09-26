package encrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestSQLResolverOnUserCreated(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	_, _, res := sqlResolverFS(t, db, testKey(t))

	if err := res.OnUserCreated(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	var keyID int64
	if err := db.QueryRow(ctx, `SELECT sealed_uk, key_id FROM user_keys WHERE user_id = ?`, aliceID).Scan(&sealed, &keyID); err != nil {
		t.Fatal("UK not minted at user creation:", err)
	}
	if keyID != 0 {
		t.Errorf("key_id = %d, want 0 (single-key ring)", keyID)
	}
	uk, err := wrapOpen(res.keys[0], sealed, ukAD(aliceID))
	if err != nil {
		t.Fatalf("minted UK does not open under the current ring key with the pinned AD: %v", err)
	}
	if len(uk) != userKeySize {
		t.Errorf("UK length = %d", len(uk))
	}

	// Idempotent: a second call keeps the same row, bytes untouched.
	if err := res.OnUserCreated(ctx, "alice"); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	var n int64
	var sealedAgain []byte
	if err := db.QueryRow(ctx, `
SELECT COUNT(*), (SELECT sealed_uk FROM user_keys WHERE user_id = ?) FROM user_keys WHERE user_id = ?`, aliceID, aliceID).Scan(&n, &sealedAgain); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("user_keys rows = %d, want 1", n)
	}
	if !bytes.Equal(sealed, sealedAgain) {
		t.Error("idempotent re-run rewrote the UK row")
	}

	// Unknown uid errors — the resolver never invents users.
	if err := res.OnUserCreated(ctx, "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("OnUserCreated for an unknown user err = %v", err)
	}
}

func TestSQLResolverOnUserDeleted(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	// alice owns a sealed file shared with bob: key rows exist for both.
	uuid := writeV3(t, fs, "alice/a.txt", []byte("shared"))
	if err := res.WrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	count := func(q string, userID int64) int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(ctx, q, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	uks := `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`
	wraps := `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`

	if err := res.OnUserDeleted(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := count(uks, aliceID); got != 0 {
		t.Errorf("alice UK rows after delete = %d, want 0", got)
	}
	if got := count(wraps, aliceID); got != 0 {
		t.Errorf("alice wrap rows after delete = %d, want 0", got)
	}
	// Other users' rows are untouched.
	if got := count(uks, bobID); got != 1 {
		t.Errorf("bob UK rows after alice's delete = %d, want 1", got)
	}
	if got := count(wraps, bobID); got != 1 {
		t.Errorf("bob wrap rows after alice's delete = %d, want 1", got)
	}

	// Unknown uid is a no-op, and so is a replay once the users row is
	// really gone (the hook firing after the row delete must be safe).
	if err := res.OnUserDeleted(ctx, "ghost"); err != nil {
		t.Fatalf("unknown uid: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = ?`, aliceID); err != nil {
		t.Fatal(err)
	}
	if err := res.OnUserDeleted(ctx, "alice"); err != nil {
		t.Fatalf("replay after the users row is gone: %v", err)
	}
}

func TestSQLResolverResealUserKeys(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	keyA, keyB := testKey(t), testKey(t)

	// Zero rows: nothing to re-seal.
	resAB, err := NewSQLResolver(db, [][]byte{keyA, keyB})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := resAB.ResealUserKeys(ctx); err != nil || n != 0 {
		t.Fatalf("empty re-seal = %d %v, want 0 nil", n, err)
	}

	// Mint both UKs under ring position 0 (single-key ring holding key A).
	resA, err := NewSQLResolver(db, [][]byte{keyA})
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"alice", "bob"} {
		if err := resA.OnUserCreated(ctx, uid); err != nil {
			t.Fatal(err)
		}
	}
	unseal := func(userID int64, key []byte) []byte {
		t.Helper()
		var sealed []byte
		if err := db.QueryRow(ctx, `SELECT sealed_uk FROM user_keys WHERE user_id = ?`, userID).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		uk, err := wrapOpen(key, sealed, ukAD(userID))
		if err != nil {
			t.Fatalf("UK for user %d does not open: %v", userID, err)
		}
		return uk
	}
	want := map[int64][]byte{aliceID: unseal(aliceID, keyA), bobID: unseal(bobID, keyA)}

	// The ring grew to [A, B] (rotation): both rows re-seal to key id 1,
	// carrying the same UK bytes.
	n, err := resAB.ResealUserKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("re-sealed = %d, want 2", n)
	}
	for userID, wantUK := range want {
		var sealed []byte
		var keyID int64
		if err := db.QueryRow(ctx, `SELECT sealed_uk, key_id FROM user_keys WHERE user_id = ?`, userID).Scan(&sealed, &keyID); err != nil {
			t.Fatal(err)
		}
		if keyID != 1 {
			t.Errorf("user %d key_id = %d, want 1 (current ring position)", userID, keyID)
		}
		got, err := wrapOpen(keyB, sealed, ukAD(userID))
		if err != nil {
			t.Fatalf("re-sealed UK for user %d does not open under the new key: %v", userID, err)
		}
		if !bytes.Equal(got, wantUK) {
			t.Errorf("user %d UK bytes changed across the re-seal", userID)
		}
	}

	// Idempotent: every row is at the current position already.
	if n, err := resAB.ResealUserKeys(ctx); err != nil || n != 0 {
		t.Fatalf("idempotent re-seal = %d %v, want 0 nil", n, err)
	}

	// A row sealed under a position the keyring does not hold aborts the
	// pass with ErrUnresolvableKey naming the user (loadUK behavior).
	if _, err := db.Exec(ctx, `UPDATE user_keys SET key_id = 99 WHERE user_id = ?`, aliceID); err != nil {
		t.Fatal(err)
	}
	_, err = resAB.ResealUserKeys(ctx)
	if !errors.Is(err, ErrUnresolvableKey) {
		t.Fatalf("out-of-ring key id err = %v, want ErrUnresolvableKey", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("user %d", aliceID)) {
		t.Errorf("err must name the user: %v", err)
	}
}

func TestSQLResolverPruneAndMint(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	// alice has a UK + an owner wrap from her v3 write; bob has nothing.
	uuid := writeV3(t, fs, "alice/a.txt", []byte("data"))

	// The mint backfills bob only.
	n, err := res.MintMissingUserKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("minted = %d, want 1 (bob only)", n)
	}
	var bobKeyID int64
	if err := db.QueryRow(ctx, `SELECT key_id FROM user_keys WHERE user_id = ?`, bobID).Scan(&bobKeyID); err != nil {
		t.Fatal("bob's UK was not minted:", err)
	}
	if n, err := res.MintMissingUserKeys(ctx); err != nil || n != 0 {
		t.Fatalf("idempotent mint = %d %v, want 0 nil", n, err)
	}

	// bob gains a wrap as a share recipient, then is deleted raw (no
	// hook): both of his rows go stale.
	if err := res.WrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = ?`, bobID); err != nil {
		t.Fatal(err)
	}
	pruned, err := res.PruneStaleKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pruned.UserKeys != 1 || pruned.FileKeys != 1 || pruned.TokenKeys != 0 {
		t.Errorf("pruned = %+v; want {1 1 0}", pruned)
	}
	// alice's rows survive the prune.
	for _, q := range []string{
		`SELECT COUNT(*) FROM user_keys WHERE user_id = ?`,
		`SELECT COUNT(*) FROM file_keys WHERE user_id = ?`,
	} {
		var cnt int64
		if err := db.QueryRow(ctx, q, aliceID).Scan(&cnt); err != nil {
			t.Fatal(err)
		}
		if cnt != 1 {
			t.Errorf("alice rows after prune (%s) = %d, want 1", q, cnt)
		}
	}
	// Nothing stale remains.
	pruned, err = res.PruneStaleKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != (PruneStats{}) {
		t.Errorf("second prune = %+v; want zero", pruned)
	}
}

func TestSQLResolverInventory(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	carolID := seedResolverUser(t, db, "carol")
	keyA, keyB := testKey(t), testKey(t)

	// alice's UK sits at the retired ring position 0; bob's (minted by his
	// v3 write over the full ring) at the current position 1.
	resA, err := NewSQLResolver(db, [][]byte{keyA})
	if err != nil {
		t.Fatal(err)
	}
	if err := resA.OnUserCreated(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	fs, _, res := sqlResolverFS(t, db, keyA, keyB)
	uuidB := writeV3(t, fs, "bob/b.txt", []byte("bob file"))
	// carol's UK is stale: minted, then her users row deleted raw.
	if err := res.OnUserCreated(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE id = ?`, carolID); err != nil {
		t.Fatal(err)
	}

	// Two live v3 files in the filecache: bob's (its owner wrap exists) and
	// alice's broken one — its key UUID names no wrap row for alice, so the
	// file can never resolve.
	insertFile := func(userID int64, name string, uuid []byte) {
		t.Helper()
		if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, key_uuid)
VALUES (?, ?, ?, 0, 8, 0, 'x', 'application/octet-stream', 31, ?)`, userID, name, "/"+name, uuid); err != nil {
			t.Fatal(err)
		}
	}
	insertFile(bobID, "b.txt", uuidB[:])
	ghost := make([]byte, 16)
	if _, err := rand.Read(ghost); err != nil {
		t.Fatal(err)
	}
	insertFile(aliceID, "gone.txt", ghost)

	inv, err := res.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := KeyInventory{
		UsersTotal:       2, // alice, bob (carol is gone)
		UsersWithUK:      3, // + carol's stale row
		UKsRetiredKeyID:  1, // alice at key id 0
		StaleUKs:         1, // carol
		WrapRows:         1, // bob's owner wrap
		DistinctKeyUUIDs: 1,
		StaleWraps:       0,
		V3Files:          2,
		BrokenV3Files:    1, // alice's ghost-UUID file
	}
	if inv != want {
		t.Fatalf("inventory = %+v, want %+v", inv, want)
	}
}

// TestSQLResolverConcurrentOnUserCreated pins the loadOrCreateUK insert-race
// re-select through the lifecycle entry point: N goroutines minting the same
// new user's key must all succeed and leave exactly one row.
func TestSQLResolverConcurrentOnUserCreated(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	_, _, res := sqlResolverFS(t, db, testKey(t))

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := res.OnUserCreated(ctx, "alice"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent OnUserCreated: %v", err)
	}
	var n int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("user_keys rows = %d, want 1 (the insert race re-selects)", n)
	}
}
