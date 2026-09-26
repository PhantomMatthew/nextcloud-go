package encrypt

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSQLResolverWrapKeyFor(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	uuid := writeV3(t, fs, "alice/docs/a.txt", []byte("shared v3"))

	// Wrap for the recipient: the row exists, opens under bob's lazily
	// created UK with the pinned AD, and carries the same FK as the owner's
	// row.
	if err := res.WrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	var wrapped []byte
	if err := db.QueryRow(ctx, `
SELECT wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], bobID).Scan(&wrapped); err != nil {
		t.Fatal("recipient wrap row missing:", err)
	}
	var sealedUK []byte
	var keyID int64
	if err := db.QueryRow(ctx, `
SELECT sealed_uk, key_id FROM user_keys WHERE user_id = ?`, bobID).Scan(&sealedUK, &keyID); err != nil {
		t.Fatal("recipient UK not lazily created:", err)
	}
	if keyID != 0 {
		t.Errorf("recipient UK key_id = %d, want 0 (single-key ring)", keyID)
	}
	bobUK, err := wrapOpen(res.keys[0], sealedUK, ukAD(bobID))
	if err != nil {
		t.Fatalf("recipient UK does not open under ring key 0 with the pinned AD: %v", err)
	}
	fkBob, err := wrapOpen(bobUK, wrapped, fkAD(uuid, bobID))
	if err != nil {
		t.Fatalf("recipient FK wrap does not open with the pinned AD: %v", err)
	}
	fkOwner, err := res.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fkBob, fkOwner) {
		t.Error("recipient wrap carries a different FK than the owner row")
	}

	// Idempotent: a re-wrap is a no-op (one row, no error).
	if err := res.WrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatalf("re-wrap: %v", err)
	}
	var n int64
	if err := db.QueryRow(ctx, `
SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], bobID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recipient rows after re-wrap = %d, want 1", n)
	}

	// Unknown user errors; unknown key UUID is unresolvable.
	if err := res.WrapKeyFor(ctx, uuid, "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("WrapKeyFor for an unknown user err = %v", err)
	}
	var zero [16]byte
	if err := res.WrapKeyFor(ctx, zero, "bob"); !errors.Is(err, ErrUnresolvableKey) {
		t.Errorf("WrapKeyFor for an unknown uuid err = %v, want ErrUnresolvableKey", err)
	}
}

func TestSQLResolverUnwrapKeyFor(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	uuid := writeV3(t, fs, "alice/a.txt", []byte("owned and shared"))
	// Live filecache row names the owner (production always has it).
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, key_uuid)
VALUES (?, 'a.txt', '/a.txt', 0, 5, 0, 'x', 'application/octet-stream', 31, ?)`, aliceID, uuid[:]); err != nil {
		t.Fatal(err)
	}
	if err := res.WrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatal(err)
	}

	count := func(userID int64) int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(ctx, `
SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Owner guard: unwrapping the file's owner is a no-op — a revoke must
	// never orphan the file from its owner.
	if err := res.UnwrapKeyFor(ctx, uuid, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := count(aliceID); got != 1 {
		t.Errorf("owner rows after owner unwrap = %d, want 1 (owner guard)", got)
	}

	// Recipient unwrap deletes the row and is idempotent.
	if err := res.UnwrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatal(err)
	}
	if got := count(bobID); got != 0 {
		t.Errorf("recipient rows after unwrap = %d, want 0", got)
	}
	if err := res.UnwrapKeyFor(ctx, uuid, "bob"); err != nil {
		t.Fatalf("re-unwrap: %v", err)
	}
	if got := count(aliceID); got != 1 {
		t.Errorf("owner rows after recipient unwrap = %d, want 1", got)
	}

	// Unknown uid is a no-op.
	if err := res.UnwrapKeyFor(ctx, uuid, "ghost"); err != nil {
		t.Errorf("unwrap unknown uid: %v", err)
	}
}

func TestSQLResolverReWrapSharees(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	bobID := seedResolverUser(t, db, "bob")
	carolID := seedResolverUser(t, db, "carol")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	oldUUID := writeV3(t, fs, "alice/a.txt", []byte("old content"))
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, key_uuid)
VALUES (?, 'a.txt', '/a.txt', 0, 5, 0, 'x', 'application/octet-stream', 31, ?)`, aliceID, oldUUID[:]); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"bob", "carol"} {
		if err := res.WrapKeyFor(ctx, oldUUID, uid); err != nil {
			t.Fatal(err)
		}
	}

	// The overwrite mints a fresh key (owner row from Allocate); the
	// filecache row now points at it.
	newUUID := writeV3(t, fs, "alice/a.txt", []byte("new content"))
	if _, err := db.Exec(ctx, `UPDATE files SET key_uuid = ? WHERE key_uuid = ?`, newUUID[:], oldUUID[:]); err != nil {
		t.Fatal(err)
	}

	if err := res.ReWrapSharees(ctx, oldUUID, newUUID); err != nil {
		t.Fatal(err)
	}

	// Recipients carried to the new key: rows open with the pinned AD and
	// hold the new FK.
	fkNew, err := res.Resolve(ctx, newUUID)
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{bobID, carolID} {
		var wrapped []byte
		if err := db.QueryRow(ctx, `
SELECT wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = ?`, newUUID[:], userID).Scan(&wrapped); err != nil {
			t.Fatalf("recipient %d not carried: %v", userID, err)
		}
		uk, found, err := res.loadUK(ctx, userID)
		if err != nil || !found {
			t.Fatalf("recipient %d UK: %v %v", userID, found, err)
		}
		fk, err := wrapOpen(uk, wrapped, fkAD(newUUID, userID))
		if err != nil {
			t.Fatalf("carried wrap for %d does not open with the pinned AD: %v", userID, err)
		}
		if !bytes.Equal(fk, fkNew) {
			t.Errorf("carried FK for %d differs from the new owner FK", userID)
		}
	}

	// Old rows all gone; the owner has exactly one new-UUID row (the carry
	// skipped the owner rather than duplicating Allocate's row).
	var oldRows int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM file_keys WHERE key_uuid = ?`, oldUUID[:]).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 {
		t.Errorf("old-uuid rows = %d, want 0", oldRows)
	}
	var ownerRows int64
	if err := db.QueryRow(ctx, `
SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`, newUUID[:], aliceID).Scan(&ownerRows); err != nil {
		t.Fatal(err)
	}
	if ownerRows != 1 {
		t.Errorf("owner rows on new uuid = %d, want 1", ownerRows)
	}

	// Idempotent: a re-run is a no-op.
	if err := res.ReWrapSharees(ctx, oldUUID, newUUID); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	var total int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM file_keys WHERE key_uuid = ?`, newUUID[:]).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("new-uuid rows after re-run = %d, want 3 (owner + 2 recipients)", total)
	}

	// old == new is a no-op.
	if err := res.ReWrapSharees(ctx, newUUID, newUUID); err != nil {
		t.Errorf("same-uuid rewrap: %v", err)
	}
}
