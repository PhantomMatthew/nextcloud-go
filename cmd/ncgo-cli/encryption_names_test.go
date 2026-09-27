package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// namesEnv writes a localfs-backed per-user-keys config; filenameEncryption
// toggles encryption.filename_encryption (ADR-0104), passwordWrapped toggles
// encryption.password_wrapped_keys (ADR-0100). Small argon2id params keep
// enrollment fast in tests.
func namesEnv(t *testing.T, dir string, filenameEncryption, passwordWrapped bool) (cfgPath, storageRoot, dbPath string) {
	t.Helper()
	storageRoot = filepath.Join(dir, "storage")
	dbPath = filepath.Join(dir, "ncgo.db")
	keyPath := filepath.Join(dir, "master.key")
	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	cfg := fmt.Sprintf(`
database:
  driver: sqlite
  dsn: %q
storage:
  default_backend: local
  backends:
    local:
      type: localfs
      root: %q
auth:
  argon2id:
    memory_kb: 8
    iterations: 1
    parallelism: 1
encryption:
  enabled: true
  master_key_path: %q
  per_user_keys: true
  filename_encryption: %v
  password_wrapped_keys: %v
`, dbPath, storageRoot, keyPath, filenameEncryption, passwordWrapped)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storageRoot, dbPath
}

// seedPlaintextTree seeds alice's pre-sweep tree: the root row, one v3-sealed
// file (real storage content), one trash row + object, one share of the file
// to bob. Returns the file's key uuid and the trash location id.
func seedPlaintextTree(t *testing.T, cfgPath, dbPath string) (dbClosable func()) {
	t.Helper()
	ctx := context.Background()
	db := openRekeyDB(t, dbPath, "alice", "bob")
	st, err := openStorage(mustLoadConfig(t, cfgPath), db)
	if err != nil {
		t.Fatal(err)
	}
	content := "plaintext content"
	wc, err := st.Create(ctx, "alice/a.txt", int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
	kw, ok := wc.(storage.KeyUUIDWriter)
	if !ok {
		t.Fatal("v3 writer does not implement storage.KeyUUIDWriter")
	}
	uuid, ok := kw.SealedKeyUUID()
	if !ok {
		t.Fatal("no key uuid reported")
	}
	var aliceID, bobID int64
	for uid, dst := range map[string]*int64{"alice": &aliceID, "bob": &bobID} {
		if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = ?`, uid).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions)
VALUES (?, '', '/', 1, 0, 0, 'x', 'httpd/unix-directory', 31)`, aliceID); err != nil {
		t.Fatal(err)
	}
	var rootID int64
	if err := db.QueryRow(ctx, `SELECT id FROM files WHERE user_id = ? AND path = '/'`, aliceID).Scan(&rootID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, parent_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, key_uuid)
VALUES (?, ?, 'a.txt', '/a.txt', 0, ?, 0, 'x', 'application/octet-stream', 31, ?)`, aliceID, rootID, len(content), uuid[:]); err != nil {
		t.Fatal(err)
	}
	// Trash row + object (plaintext location id — the pre-sweep state).
	if _, err := db.Exec(ctx, `
INSERT INTO trash_items (user_id, original_path, location_id, name, is_dir, size, deleted_ms, deleted_by)
VALUES (?, '/note.txt', 'note.txt.d1700000000', 'note.txt', 0, 4, 1700000000000, 'alice')`, aliceID); err != nil {
		t.Fatal(err)
	}
	tw, err := st.Create(ctx, "trash/alice/note.txt.d1700000000", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("note")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sharing.NewSQLShareStore(db).Insert(ctx, &files.Share{
		OwnerUserID: aliceID, ShareType: files.ShareTypeUser, Path: "/a.txt",
		ItemType: "file", Token: "names-share-token", Permissions: 1, ShareWith: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	return func() { _ = db.Close() }
}

// TestEncryptionNameSweepCommands pins the CLI half of ADR-0104 phase 4:
// dry-run → real run → idempotent re-run → explicit-skip exit codes →
// decrypt-names round trip, with the trash object following its location id.
func TestEncryptionNameSweepCommands(t *testing.T) {
	dir := t.TempDir()
	cfgPath, storageRoot, dbPath := namesEnv(t, dir, true, false)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	cleanup := seedPlaintextTree(t, cfgPath, dbPath)
	ctx := context.Background()

	// Dry-run: reports, writes nothing.
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry-run encrypt-names: alice: would convert files=2 folders-keyed=1 locks=0 versions=0 trash=1 trash-objects=1 shares=1 share-wraps=0 skipped-rows=0") {
		t.Fatalf("dry-run alice line: %q", out)
	}
	if !strings.Contains(out, "dry-run encrypt-names: converted=2 skipped=0 failed=0") {
		t.Fatalf("dry-run summary (bob converts treeless): %q", out)
	}
	db := openRekeyDB(t, dbPath)
	defer func() { _ = db.Close() }()
	var scheme int
	if err := db.QueryRow(ctx, `SELECT name_scheme FROM users WHERE uid = 'alice'`).Scan(&scheme); err != nil {
		t.Fatal(err)
	}
	if scheme != 0 {
		t.Fatal("dry-run flipped the scheme")
	}

	// Real run.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encrypt-names: alice: converted files=2 folders-keyed=1") ||
		!strings.Contains(out, "encrypt-names: converted=2 skipped=0 failed=0") {
		t.Fatalf("encrypt-names output = %q", out)
	}
	if err := db.QueryRow(ctx, `SELECT name_scheme FROM users WHERE uid = 'alice'`).Scan(&scheme); err != nil {
		t.Fatal(err)
	}
	if scheme != 1 {
		t.Fatalf("alice name_scheme = %d, want 1", scheme)
	}
	// The files rows tokenized; the file kept its FK; the root carries a DK.
	var fileName, filePath string
	var fileKey, rootKey []byte
	if err := db.QueryRow(ctx, `SELECT name, path, key_uuid FROM files WHERE is_dir = 0`).Scan(&fileName, &filePath, &fileKey); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fileName, "txt") || strings.Contains(filePath, "a.txt") || len(fileKey) != 16 {
		t.Errorf("file row = (%q, %q, %d key bytes)", fileName, filePath, len(fileKey))
	}
	if err := db.QueryRow(ctx, `SELECT key_uuid FROM files WHERE path = '/'`).Scan(&rootKey); err != nil {
		t.Fatal(err)
	}
	if len(rootKey) != 16 {
		t.Fatal("root has no directory key after the sweep")
	}
	// Trash row + object moved to the token id.
	var trashLoc string
	if err := db.QueryRow(ctx, `SELECT location_id FROM trash_items`).Scan(&trashLoc); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(trashLoc, "note.txt") || !strings.HasSuffix(trashLoc, ".d1700000000") {
		t.Errorf("trash location id = %q", trashLoc)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "trash", "alice", trashLoc)); err != nil {
		t.Errorf("trash object missing under %q: %v", trashLoc, err)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "trash", "alice", "note.txt.d1700000000")); !os.IsNotExist(err) {
		t.Errorf("old trash object should be gone: %v", err)
	}
	// The share row is ciphertext with sealed metadata.
	var sharePath, mountEnc, absEnc string
	if err := db.QueryRow(ctx, `SELECT file_path, mount_name_enc, abs_path_enc FROM shares`).Scan(&sharePath, &mountEnc, &absEnc); err != nil {
		t.Fatal(err)
	}
	if sharePath != filePath || mountEnc == "" || absEnc == "" {
		t.Errorf("share = (%q, %q, %q), want the /a.txt ciphertext %q + sealed fields", sharePath, mountEnc, absEnc, filePath)
	}

	// Idempotent: every user is scheme 1 → the default set is empty.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encrypt-names: converted=0 skipped=0 failed=0") {
		t.Fatalf("idempotent output = %q", out)
	}
	// An explicitly requested user who skips exits nonzero.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names", "--user", "alice")
	if err == nil || !strings.Contains(out, "skipped (already in the target scheme)") {
		t.Fatalf("--user skip = %q, %v", out, err)
	}
	// Unknown --user errors.
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names", "--user", "nobody"); err == nil {
		t.Fatal("unknown --user must error")
	}

	// decrypt-names reverses both users (the decommission flow also works
	// with filename_encryption already off — tested below).
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-names")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "decrypt-names: alice: converted files=2 folders-keyed=0") ||
		!strings.Contains(out, "decrypt-names: converted=2 skipped=0 failed=0") {
		t.Fatalf("decrypt-names output = %q", out)
	}
	if err := db.QueryRow(ctx, `SELECT name, path, key_uuid FROM files WHERE is_dir = 0`).Scan(&fileName, &filePath, &fileKey); err != nil {
		t.Fatal(err)
	}
	if fileName != "a.txt" || filePath != "/a.txt" || len(fileKey) != 16 {
		t.Errorf("file row after decrypt = (%q, %q, %d key bytes)", fileName, filePath, len(fileKey))
	}
	if err := db.QueryRow(ctx, `SELECT key_uuid FROM files WHERE path = '/'`).Scan(&rootKey); err != nil {
		t.Fatal(err)
	}
	if len(rootKey) != 0 {
		t.Error("root still carries a directory key after decrypt-names")
	}
	if err := db.QueryRow(ctx, `SELECT location_id FROM trash_items`).Scan(&trashLoc); err != nil {
		t.Fatal(err)
	}
	if trashLoc != "note.txt.d1700000000" {
		t.Errorf("trash location id = %q after decrypt", trashLoc)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, "trash", "alice", trashLoc)); err != nil {
		t.Errorf("trash object not back: %v", err)
	}
	if err := db.QueryRow(ctx, `SELECT file_path, mount_name_enc, abs_path_enc FROM shares`).Scan(&sharePath, &mountEnc, &absEnc); err != nil {
		t.Fatal(err)
	}
	if sharePath != "/a.txt" || mountEnc != "" || absEnc != "" {
		t.Errorf("share after decrypt = (%q, %q, %q)", sharePath, mountEnc, absEnc)
	}
	cleanup()
}

// TestEncryptionNameSweepFlagGuards pins the config gates: encrypt-names
// requires filename_encryption; decrypt-names does not (the flag-off state IS
// the decommission flow); both require per-user keys.
func TestEncryptionNameSweepFlagGuards(t *testing.T) {
	dir := t.TempDir()
	// filename_encryption off: encrypt-names refuses, decrypt-names runs.
	cfgPath, _, dbPath := namesEnv(t, dir, false, false)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	openRekeyDB(t, dbPath, "alice")
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names"); err == nil ||
		!strings.Contains(err.Error(), "filename_encryption") {
		t.Fatalf("encrypt-names without the flag err = %v", err)
	}
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-names")
	if err != nil {
		t.Fatalf("decrypt-names with the flag off (nothing to do) = %v", err)
	}
	if !strings.Contains(out, "decrypt-names: converted=0 skipped=0 failed=0") {
		t.Fatalf("decrypt-names idle output = %q", out)
	}
}

// TestEncryptionNameSweepEnrolled pins the enrolled-user rules: the offline
// CLI never converts enrolled users — encrypt-names skips them (they convert
// at password login), decrypt-names fails them with the unenroll-first hint.
func TestEncryptionNameSweepEnrolled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath, _, dbPath := namesEnv(t, dir, true, true)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	db := openRekeyDB(t, dbPath, "alice", "bob")
	defer func() { _ = db.Close() }()
	masterKey, err := encrypt.LoadMasterKey(mustLoadConfig(t, cfgPath).Encryption.MasterKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := encrypt.NewSQLResolver(db, [][]byte{masterKey})
	if err != nil {
		t.Fatal(err)
	}
	res.PasswordWrapped = true
	res.KDF = encrypt.KeyDerivationParams{MemoryKB: 8, Iterations: 1, Parallelism: 1}
	// Enroll alice (a password login runs the state machine); bob stays
	// master-wrapped.
	priv, err := res.UnlockForLogin(ctx, "alice", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != 32 {
		t.Fatalf("unlocked key = %d bytes", len(priv))
	}

	// encrypt-names: alice is skipped with the login note, bob converts;
	// skips are not failures.
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encrypt-names: alice: skipped (enrolled in password-wrapped keys — converts at their next password login)") {
		t.Fatalf("enrolled skip line missing: %q", out)
	}
	if !strings.Contains(out, "encrypt-names: converted=1 skipped=1 failed=0") {
		t.Fatalf("summary = %q", out)
	}
	// An explicitly requested enrolled user exits nonzero (the conversion did
	// not happen).
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-names", "--user", "alice"); err == nil {
		t.Fatal("--user on an enrolled user must exit nonzero")
	}

	// Drive alice's conversion the way the login hook does (the sweep with
	// the unlocked-key ctx), then prove decrypt-names refuses her offline.
	sweep := &files.NameSweep{
		DB:      db,
		Store:   files.NewSQLStore(db),
		Keys:    res,
		Users:   users.NewSQLStore(db),
		Storage: nil, // no trash rows — no storage moves happen
	}
	sweepCtx := auth.WithUser(ctx, &auth.Principal{UID: "alice", Enabled: true, UnlockedKey: priv})
	if _, err := sweep.EncryptUser(sweepCtx, "alice"); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-names")
	if err == nil {
		t.Fatal("decrypt-names with an enrolled scheme-1 user must fail")
	}
	if !strings.Contains(out, "unenroll first") ||
		!strings.Contains(out, "decrypt-names: converted=1 skipped=0 failed=1") {
		t.Fatalf("decrypt-names enrolled output = %q", out)
	}
}

// TestEncryptionStatusNameScheme pins the phase-4 status additions: the
// rollout line always prints in per-user mode; a scheme-1 folder without a
// directory key is the sweep-miss alarm and joins the nonzero-exit rule.
func TestEncryptionStatusNameScheme(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _, dbPath := rekeyEnv(t, dir, true)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	db := openRekeyDB(t, dbPath, "alice")
	ctx := context.Background()
	defer func() { _ = db.Close() }()

	// Clean state: the rollout line prints, exit 0, no warning.
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "filename encryption: 0/1 users scheme-1, 0 tokenized rows") {
		t.Fatalf("status rollout line missing: %q", out)
	}
	if strings.Contains(out, "WITHOUT a directory key") {
		t.Fatalf("clean status must not warn: %q", out)
	}

	// Hand-break a scheme-1 folder: user flipped, folder row tokenized but
	// keyless (the sweep-miss state) — names of its children are broken.
	var aliceID int64
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE users SET name_scheme = 1 WHERE id = ?`, aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO files (user_id, name, path, is_dir, size, mtime_ms, etag, mime, permissions, name_scheme)
VALUES (?, 'tok', '/tok', 1, 0, 0, 'x', 'httpd/unix-directory', 31, 1)`, aliceID); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "status")
	if err == nil {
		t.Fatal("status with a keyless scheme-1 folder must exit nonzero")
	}
	if !strings.Contains(out, "filename encryption: 1/1 users scheme-1, 1 tokenized rows") ||
		!strings.Contains(out, "scheme-1 folders WITHOUT a directory key: 1") {
		t.Fatalf("status = %q", out)
	}
}
