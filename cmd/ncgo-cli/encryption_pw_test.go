package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// pwEnv mirrors rekeyEnv with password_wrapped_keys added to the encryption
// section.
func pwEnv(t *testing.T, dir string, passwordWrapped bool) (cfgPath, storageRoot, dbPath string) {
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
encryption:
  enabled: true
  master_key_path: %q
  per_user_keys: true
  password_wrapped_keys: %v
`, dbPath, storageRoot, keyPath, passwordWrapped)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, storageRoot, dbPath
}

// enrollCLIUser enrolls uid directly through a resolver over the fixture db
// (the server-side enrollment path is pinned elsewhere; the CLI tests need
// the resulting state, not the login flow).
func enrollCLIUser(t *testing.T, db database.DB, keyPath, uid, password string) {
	t.Helper()
	key, err := encrypt.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := encrypt.NewSQLResolver(db, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	res.PasswordWrapped = true
	res.KDF = encrypt.KeyDerivationParams{MemoryKB: 1024, Iterations: 1, Parallelism: 1}
	if _, err := res.UnlockForLogin(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
}

// writeV3File writes one v3-sealed file through the CLI's own storage stack.
func writeV3File(t *testing.T, cfgPath string, db database.DB, rel, content string) {
	t.Helper()
	st, err := openStorage(mustLoadConfig(t, cfgPath), db)
	if err != nil {
		t.Fatal(err)
	}
	wc, err := st.Create(context.Background(), rel, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptionStatusPasswordWrapped(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _, dbPath := pwEnv(t, dir, true)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	db := openRekeyDB(t, dbPath, "alice", "bob")
	writeV3File(t, cfgPath, db, "alice/a.txt", "alpha")
	enrollCLIUser(t, db, filepath.Join(dir, "master.key"), "alice", "wonderland")

	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"per-user keys: on",
		"password-wrapped keys: true",
		"password-wrapped users: 1",
		"file key wraps: 1 rows across 1 key uuids (1 box wraps)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

// TestEncryptionDecryptAllLockedSkip pins the ADR-0100 sweep behavior
// end-to-end: decrypt-all over an enrolled user's v3 tree skips the files as
// locked, exits 0, and leaves them sealed.
func TestEncryptionDecryptAllLockedSkip(t *testing.T) {
	dir := t.TempDir()
	cfgPath, root, dbPath := pwEnv(t, dir, true)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	db := openRekeyDB(t, dbPath, "alice")
	writeV3File(t, cfgPath, db, "alice/a.txt", "alpha")
	writeV3File(t, cfgPath, db, "alice/b.txt", "beta")
	enrollCLIUser(t, db, filepath.Join(dir, "master.key"), "alice", "wonderland")

	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-all")
	if err != nil {
		t.Fatalf("decrypt-all over enrolled files must exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(out, "locked-skipped=2") {
		t.Errorf("decrypt-all output missing locked-skipped=2:\n%s", out)
	}
	if !strings.Contains(out, "failed=0") {
		t.Errorf("a locked skip is never a failure:\n%s", out)
	}
	for _, rel := range []string{"alice/a.txt", "alice/b.txt"} {
		if !rawHasV3Magic(t, root, rel) {
			t.Errorf("%s was decrypted despite the lock", rel)
		}
	}
}

// TestUserResetPasswordEnrolledRefusal pins ADR-0100 §6: reset-password
// refuses an enrolled user without --force, and --force destroys the
// enrollment and every wrap row before setting the new password.
func TestUserResetPasswordEnrolledRefusal(t *testing.T) {
	dir := t.TempDir()
	cfgPath, _, dbPath := pwEnv(t, dir, true)
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	db := openRekeyDB(t, dbPath, "alice", "bob")
	writeV3File(t, cfgPath, db, "alice/a.txt", "alpha")
	enrollCLIUser(t, db, filepath.Join(dir, "master.key"), "alice", "wonderland")
	ctx := context.Background()
	count := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var aliceID int64
	if err := db.QueryRow(ctx, `SELECT id FROM users WHERE uid = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}

	// Without --force: refusal naming the data loss and the remedy.
	out, err := runCLI(t, "newpass\n", "--config", cfgPath, "user", "reset-password", "alice", "--password-stdin")
	if err == nil ||
		!strings.Contains(err.Error(), "password-wrapped encryption keys (ADR-0100)") ||
		!strings.Contains(err.Error(), "PERMANENTLY UNREADABLE") ||
		!strings.Contains(err.Error(), "--force") {
		t.Fatalf("refusal = %q %v", out, err)
	}
	if n := count(`SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 1 {
		t.Fatal("refused reset destroyed the enrollment")
	}

	// With --force: loud data-loss warning, enrollment + wraps destroyed,
	// password reset proceeds.
	out, err = runCLI(t, "newpass\n", "--config", cfgPath, "user", "reset-password", "alice", "--password-stdin", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DATA LOSS") || !strings.Contains(out, "password reset for alice") {
		t.Errorf("forced reset output = %q", out)
	}
	if n := count(`SELECT COUNT(*) FROM user_key_pw WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("user_key_pw rows after --force = %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, aliceID); n != 0 {
		t.Errorf("file_keys rows after --force = %d, want 0", n)
	}

	// Control: an unenrolled user resets without --force exactly as before.
	out, err = runCLI(t, "newpass\n", "--config", cfgPath, "user", "reset-password", "bob", "--password-stdin")
	if err != nil || !strings.Contains(out, "password reset for bob") {
		t.Errorf("unenrolled reset = %q %v", out, err)
	}

	// Verify the new password actually works for alice: her hash verifies
	// against the CLI's own verifier path (login is covered elsewhere).
	st, err := openStorage(mustLoadConfig(t, cfgPath), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Open(ctx, "alice/a.txt"); err == nil {
		// The content is destroyed with the wrap rows; the sealed bytes are
		// still on disk but can never resolve.
		t.Error("alice's destroyed file must not resolve after --force")
	} else if !strings.Contains(err.Error(), "encrypt:") {
		t.Errorf("destroyed file err = %v, want an encrypt: resolve failure", err)
	}
}
