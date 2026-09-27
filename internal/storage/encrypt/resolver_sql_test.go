package encrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

// resolverDB opens a migrated in-memory sqlite database for resolver tests.
func resolverDB(t *testing.T) database.DB {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedResolverUser(t *testing.T, db database.DB, uid string) int64 {
	t.Helper()
	res, err := db.Exec(context.Background(), `
INSERT INTO users (uid, display_name, password_hash, enabled, created_at, updated_at)
VALUES (?, ?, 'x', 1, 0, 0)`, uid, uid)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.RowsAffected()
	if err != nil || id != 1 {
		t.Fatalf("seed user rows = %d %v", id, err)
	}
	var userID int64
	if err := db.QueryRow(context.Background(), `SELECT id FROM users WHERE uid = ?`, uid).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return userID
}

// sqlResolverFS builds a localfs-backed encrypt FS whose resolver is a
// SQLResolver over db and the given ring (current key last).
func sqlResolverFS(t *testing.T, db database.DB, keys ...[]byte) (*FS, *localfs.FS, *SQLResolver) {
	t.Helper()
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSQLResolver(db, keys)
	if err != nil {
		t.Fatal(err)
	}
	var previous [][]byte
	if len(keys) > 1 {
		previous = keys[:len(keys)-1]
	}
	fs, err := NewWithResolver(keys[len(keys)-1], previous, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inner, res
}

func TestNewSQLResolverValidation(t *testing.T) {
	db := resolverDB(t)
	if _, err := NewSQLResolver(nil, [][]byte{testKey(t)}); err == nil {
		t.Error("nil db must fail")
	}
	if _, err := NewSQLResolver(db, nil); err == nil {
		t.Error("empty ring must fail")
	}
	if _, err := NewSQLResolver(db, [][]byte{[]byte("short")}); err == nil {
		t.Error("short ring key must fail")
	}
	// The ring is copied: mutating the caller's slice must not corrupt the
	// resolver.
	key := testKey(t)
	res, err := NewSQLResolver(db, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	for i := range key {
		key[i] = 0
	}
	allZero := true
	for _, b := range res.keys[0] {
		if b != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Error("resolver ring aliases the caller's key slice")
	}
}

func TestSQLResolverAllocateResolve(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	uuid := writeV3(t, fs, "alice/docs/a.txt", []byte("hello v3"))
	if got := readAll(t, fs, "alice/docs/a.txt"); string(got) != "hello v3" {
		t.Fatalf("round trip = %q", got)
	}

	// The UK row exists, sealed under the current (only) ring position 0.
	var sealedUK []byte
	var keyID int64
	if err := db.QueryRow(ctx, `SELECT sealed_uk, key_id FROM user_keys WHERE user_id = ?`, aliceID).Scan(&sealedUK, &keyID); err != nil {
		t.Fatal(err)
	}
	if keyID != 0 {
		t.Errorf("user_keys.key_id = %d, want 0 (single-key ring)", keyID)
	}
	uk, err := wrapOpen(res.keys[0], sealedUK, ukAD(aliceID))
	if err != nil {
		t.Fatalf("UK row does not open under ring key 0 with the pinned AD: %v", err)
	}
	if len(uk) != userKeySize {
		t.Errorf("UK length = %d", len(uk))
	}

	// The file_keys row wraps the FK for the owner under the pinned AD.
	var wrapped []byte
	if err := db.QueryRow(ctx, `
SELECT wrapped_fk FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], aliceID).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	fk, err := wrapOpen(uk, wrapped, fkAD(uuid, aliceID))
	if err != nil {
		t.Fatalf("FK wrap does not open with the pinned AD: %v", err)
	}
	if len(fk) != fileKeySize {
		t.Errorf("FK length = %d", len(fk))
	}

	// Resolve returns the same FK on repeat calls.
	again, err := res.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, fk) {
		t.Error("Resolve is not deterministic for a known uuid")
	}
}

func TestSQLResolverADBinding(t *testing.T) {
	uk := testKey(t)
	fk := testKey(t)
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		t.Fatal(err)
	}

	wrapped, err := wrapSeal(uk, fk, fkAD(uuid, 7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapOpen(uk, wrapped, fkAD(uuid, 8)); err == nil {
		t.Error("FK wrap opened with a different user_id in the AD")
	}
	other := uuid
	other[0] ^= 0xFF
	if _, err := wrapOpen(uk, wrapped, fkAD(other, 7)); err == nil {
		t.Error("FK wrap opened with a different key uuid in the AD")
	}
	if got, err := wrapOpen(uk, wrapped, fkAD(uuid, 7)); err != nil || !bytes.Equal(got, fk) {
		t.Errorf("FK wrap with the correct AD = %v %v", got, err)
	}

	sealed, err := wrapSeal(uk, fk, ukAD(7))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrapOpen(uk, sealed, ukAD(8)); err == nil {
		t.Error("UK seal opened with a different user_id in the AD")
	}
}

func TestSQLResolverConcurrentUKCreate(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	_, _, res := sqlResolverFS(t, db, testKey(t))

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	uuids := make(chan [16]byte, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			uuid, _, err := res.Allocate(ctx, "alice/file.txt")
			if err != nil {
				errs <- err
				return
			}
			uuids <- uuid
		}()
	}
	wg.Wait()
	close(errs)
	close(uuids)
	for err := range errs {
		t.Fatalf("concurrent Allocate: %v", err)
	}
	var nUK int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID).Scan(&nUK); err != nil {
		t.Fatal(err)
	}
	if nUK != 1 {
		t.Errorf("user_keys rows = %d, want 1 (the insert race re-selects)", nUK)
	}
	// Every allocation wrote its own wrap row and resolves.
	seen := map[[16]byte]bool{}
	for uuid := range uuids {
		if seen[uuid] {
			t.Error("duplicate key uuid allocated")
		}
		seen[uuid] = true
		if _, err := res.Resolve(ctx, uuid); err != nil {
			t.Errorf("resolve allocated uuid: %v", err)
		}
	}
}

func TestSQLResolverUnknownUser(t *testing.T) {
	db := resolverDB(t)
	_, _, res := sqlResolverFS(t, db, testKey(t))
	if _, _, err := res.Allocate(context.Background(), "ghost/file.txt"); err == nil ||
		!strings.Contains(err.Error(), "ghost") {
		t.Fatalf("Allocate for an unknown user err = %v", err)
	}
}

func TestSQLResolverUnknownUUID(t *testing.T) {
	db := resolverDB(t)
	_, _, res := sqlResolverFS(t, db, testKey(t))
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		t.Fatal(err)
	}
	_, err := res.Resolve(context.Background(), uuid)
	if !errors.Is(err, ErrUnresolvableKey) {
		t.Fatalf("Resolve unknown uuid err = %v, want ErrUnresolvableKey", err)
	}
	if errors.Is(err, ErrIntegrity) {
		t.Fatal("unknown uuid must never surface as ErrIntegrity")
	}
	if !strings.Contains(err.Error(), hex.EncodeToString(uuid[:])) {
		t.Errorf("err must name the uuid: %v", err)
	}
}

func TestSQLResolverOwnerFallback(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	fs, _, res := sqlResolverFS(t, db, testKey(t))

	uuid := writeV3(t, fs, "alice/a.txt", []byte("owned"))
	// Simulate the filecache row: owner-first lookup resolves through it.
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, key_uuid)
VALUES (?, 'a.txt', '/a.txt', 0, 5, 0, 'x', 'application/octet-stream', 31, ?)`, aliceID, uuid[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Resolve(ctx, uuid); err != nil {
		t.Fatalf("owner-first resolve: %v", err)
	}
	// The owner row is gone (trash, versions, upload parts): the file_keys
	// fallback still unwraps.
	if _, err := db.Exec(ctx, `DELETE FROM files WHERE key_uuid = ?`, uuid[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := res.Resolve(ctx, uuid); err != nil {
		t.Fatalf("fallback resolve: %v", err)
	}
}

func TestSQLResolverUKRingPosition(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	prev, current := testKey(t), testKey(t)
	_, _, res := sqlResolverFS(t, db, prev, current)

	if _, _, err := res.Allocate(ctx, "alice/a.txt"); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := db.QueryRow(ctx, `SELECT key_id FROM user_keys WHERE user_id = ?`, aliceID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if keyID != 1 {
		t.Errorf("user_keys.key_id = %d, want 1 (current ring position)", keyID)
	}
}

func TestSweepRekeyV3(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	seedResolverUser(t, db, "bob")
	keyA, keyB := testKey(t), testKey(t)
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Writers: v1 (single-key ring A), v2 (ring [A,B] at ID 1), v3
	// (resolver FS over the same ring).
	v1fs, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	v2fs, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSQLResolver(db, [][]byte{keyA, keyB})
	if err != nil {
		t.Fatal(err)
	}
	v3fs, err := NewWithResolver(keyB, [][]byte{keyA}, inner, res)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string][]byte{
		"alice/v1.bin": []byte("sealed v1 payload"),
		"alice/v2.bin": []byte("sealed v2 payload!"),
		"alice/v3.bin": []byte("already v3"),
		"bob/v1.bin":   bytes.Repeat([]byte{0x42}, 2*ChunkSize+7),
		"alice/plain":  []byte("legacy plain"),
	}
	writeAll(t, v1fs, "alice/v1.bin", want["alice/v1.bin"])
	writeAll(t, v2fs, "alice/v2.bin", want["alice/v2.bin"])
	writeV3(t, v3fs, "alice/v3.bin", want["alice/v3.bin"])
	writeAll(t, v1fs, "bob/v1.bin", want["bob/v1.bin"])
	writeAll(t, inner, "alice/plain", want["alice/plain"])
	v3HeaderBefore := rawBytes(t, inner, "alice/v3.bin")

	// Guard: rekey-v3 without a resolver on the FS is an error.
	if _, err := Sweep(ctx, inner, v2fs, SweepOptions{Direction: SweepRekeyV3}); err == nil {
		t.Fatal("rekey-v3 without a resolver must fail")
	}

	// Dry-run: counts the two v1 + one v2 file (bytes via plaintext Stat),
	// skips v3 and plaintext, writes nothing.
	dry, err := Sweep(ctx, inner, v3fs, SweepOptions{Direction: SweepRekeyV3, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Scanned != 5 || dry.Changed != 3 || dry.Skipped != 2 || dry.Failed != 0 {
		t.Fatalf("dry-run stats = %+v", dry)
	}
	wantBytes := int64(len(want["alice/v1.bin"]) + len(want["alice/v2.bin"]) + len(want["bob/v1.bin"]))
	if dry.Bytes != wantBytes {
		t.Errorf("dry-run bytes = %d, want %d", dry.Bytes, wantBytes)
	}
	if hasV3Magic(t, inner, "alice/v1.bin") {
		t.Fatal("dry-run must not write")
	}

	stats, err := Sweep(ctx, inner, v3fs, SweepOptions{Direction: SweepRekeyV3})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 5 || stats.Changed != 3 || stats.Skipped != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for p, data := range want {
		if !hasV3Magic(t, inner, p) && p != "alice/plain" {
			t.Errorf("%s: not v3 after rekey", p)
		}
		if got := readAll(t, v3fs, p); !bytes.Equal(got, data) {
			t.Errorf("%s: content = %d bytes, want %d", p, len(got), len(data))
		}
	}
	if string(rawBytes(t, inner, "alice/plain")) != "legacy plain" {
		t.Error("plaintext file must stay plaintext (rekey-v3 is not encrypt-all)")
	}
	if !bytes.Equal(rawBytes(t, inner, "alice/v3.bin"), v3HeaderBefore) {
		t.Error("v3 file must be skipped untouched")
	}
	// Every rekeyed file got a file_keys wrap row for its owner.
	var wrapRows int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM file_keys`).Scan(&wrapRows); err != nil {
		t.Fatal(err)
	}
	if wrapRows != 4 { // 3 rekeyed + the pre-existing v3 write
		t.Errorf("file_keys rows = %d, want 4", wrapRows)
	}

	// Idempotent: a second run skips everything.
	again, err := Sweep(ctx, inner, v3fs, SweepOptions{Direction: SweepRekeyV3})
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed != 0 || again.Skipped != 5 {
		t.Errorf("idempotent stats = %+v", again)
	}
}

func TestSweepRotateSkipsV3(t *testing.T) {
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	keyA, keyB := testKey(t), testKey(t)
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v1fs, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewSQLResolver(db, [][]byte{keyA, keyB})
	if err != nil {
		t.Fatal(err)
	}
	v3fs, err := NewWithResolver(keyB, [][]byte{keyA}, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, v1fs, "alice/old.bin", []byte("v1 payload"))
	writeV3(t, v3fs, "alice/v3.bin", []byte("v3 payload"))
	v3Before := rawBytes(t, inner, "alice/v3.bin")

	// Rotation in per-user mode: v3 files are skipped (their content keys
	// are per-user, rotation never re-seals them); a v1 file rewritten
	// through the resolver FS lands in the v3 envelope — no longer
	// reachable with the retired key either.
	stats, err := Sweep(context.Background(), inner, v3fs, SweepOptions{Direction: SweepRotate})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 1 || stats.Skipped != 1 {
		t.Fatalf("rotate stats = %+v", stats)
	}
	if !bytes.Equal(rawBytes(t, inner, "alice/v3.bin"), v3Before) {
		t.Error("v3 file must be untouched by rotate")
	}
	if !hasV3Magic(t, inner, "alice/old.bin") {
		t.Error("v1 file must land in the v3 envelope when the FS carries a resolver")
	}
	if got := readAll(t, v3fs, "alice/old.bin"); string(got) != "v1 payload" {
		t.Errorf("rotated content = %q", got)
	}
}

func hasV3Magic(t *testing.T, inner storage.Storage, p string) bool {
	t.Helper()
	raw := rawBytes(t, inner, p)
	return len(raw) >= len(magicV3) && string(raw[:len(magicV3)]) == magicV3
}

// TestSQLResolverAllocateForUserUnenrolled pins the ADR-0104 phase-1
// directory-key mint for a master-wrapped user: a fresh 32-byte key + UUID,
// a scheme=0 owner wrap, the UK lazily minted, and a Resolve round-trip —
// with no files row involved (ownerOf falls back to the wrap row).
func TestSQLResolverAllocateForUserUnenrolled(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	_, _, res := sqlResolverFS(t, db, testKey(t))

	uuid, dk, err := res.AllocateForUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(dk) != fileKeySize {
		t.Errorf("DK length = %d, want %d", len(dk), fileKeySize)
	}

	// The UK was minted lazily; the wrap row is scheme 0 and opens under the
	// pinned FK AD.
	var sealedUK []byte
	if err := db.QueryRow(ctx, `SELECT sealed_uk FROM user_keys WHERE user_id = ?`, aliceID).Scan(&sealedUK); err != nil {
		t.Fatal("UK not lazily minted:", err)
	}
	uk, err := wrapOpen(res.keys[0], sealedUK, ukAD(aliceID))
	if err != nil {
		t.Fatalf("UK row does not open: %v", err)
	}
	var wrapped []byte
	var scheme int64
	if err := db.QueryRow(ctx, `
SELECT wrapped_fk, scheme FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], aliceID).Scan(&wrapped, &scheme); err != nil {
		t.Fatal(err)
	}
	if scheme != 0 {
		t.Errorf("wrap scheme = %d, want 0 for an unenrolled user", scheme)
	}
	got, err := wrapOpen(uk, wrapped, fkAD(uuid, aliceID))
	if err != nil {
		t.Fatalf("DK wrap does not open with the pinned AD: %v", err)
	}
	if !bytes.Equal(got, dk) {
		t.Error("wrapped DK != returned DK")
	}

	// Resolve round-trips the key through the wrap row (no files row names
	// the UUID — the directory row is inserted by the caller afterwards).
	resolved, err := res.Resolve(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resolved, dk) {
		t.Error("Resolve(DK) != DK")
	}
}

// TestSQLResolverAllocateForUserEnrolled pins the mint for an enrolled user
// (ADR-0100): the wrap is a scheme=1 box under the user's public key — no
// session, no symmetric UK — and Resolve goes through the reader's identity
// ctx like any enrolled owner's key.
func TestSQLResolverAllocateForUserEnrolled(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	aliceID := seedResolverUser(t, db, "alice")
	_, _, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF

	priv, err := res.UnlockForLogin(ctx, "alice", "wonderland")
	if err != nil {
		t.Fatal(err)
	}
	uuid, dk, err := res.AllocateForUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	var scheme int64
	if err := db.QueryRow(ctx, `
SELECT scheme FROM file_keys WHERE key_uuid = ? AND user_id = ?`, uuid[:], aliceID).Scan(&scheme); err != nil {
		t.Fatal(err)
	}
	if scheme != 1 {
		t.Errorf("wrap scheme = %d, want 1 (box) for an enrolled user", scheme)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM user_keys WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("user_keys rows = %d, want 0 (enrolled users get no symmetric UK)", n)
	}

	got, err := res.Resolve(identityCtx("alice", priv), uuid)
	if err != nil {
		t.Fatalf("resolve via reader identity: %v", err)
	}
	if !bytes.Equal(got, dk) {
		t.Error("identity Resolve(DK) != DK")
	}
	if _, err := res.Resolve(ctx, uuid); !errors.Is(err, ErrKeyLocked) {
		t.Errorf("resolve without identity err = %v, want ErrKeyLocked", err)
	}
}

// TestSQLResolverAllocateForUserUnknownUID pins the never-invent rule: an
// unknown uid is an error, and no key material is persisted.
func TestSQLResolverAllocateForUserUnknownUID(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	_, _, res := sqlResolverFS(t, db, testKey(t))

	if _, _, err := res.AllocateForUser(ctx, "ghost"); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("AllocateForUser for an unknown user err = %v", err)
	}
	if n := rowCount(t, db, `SELECT COUNT(*) FROM file_keys`); n != 0 {
		t.Errorf("file_keys rows = %d, want 0 (nothing persisted on error)", n)
	}
}
