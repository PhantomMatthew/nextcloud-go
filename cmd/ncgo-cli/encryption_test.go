package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// encConfig writes a minimal valid config file with the given encryption
// section and returns its path.
func encConfig(t *testing.T, section string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `
database:
  driver: sqlite
  dsn: ":memory:"
` + section
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEncryptionInit(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	cfgPath := encConfig(t, fmt.Sprintf(`
encryption:
  enabled: false
  master_key_path: %q
`, keyPath))

	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote master key to "+keyPath) || !strings.Contains(out, "encryption is disabled") {
		t.Fatalf("init output = %q", out)
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, strings.TrimSpace(string(raw))) {
		t.Error("key material must never be printed")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %04o", info.Mode().Perm())
	}
	key, err := encrypt.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != encrypt.MasterKeySize {
		t.Fatalf("key length = %d", len(key))
	}

	// Refuses to overwrite without --force.
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("re-init err = %v", err)
	}

	// --force overwrites (and the key changes).
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init", "--force"); err != nil {
		t.Fatal(err)
	}
	key2, err := encrypt.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) == string(key2) {
		t.Fatal("--force must generate a fresh key")
	}
}

func TestEncryptionInitKeyPathFlag(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "flag.key")
	cfgPath := encConfig(t, "")
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "init", "--key-path", keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, keyPath) {
		t.Fatalf("init output = %q", out)
	}
	if _, err := encrypt.LoadMasterKey(keyPath); err != nil {
		t.Fatal(err)
	}

	// Neither flag nor config path → error.
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err == nil ||
		!strings.Contains(err.Error(), "no key path") {
		t.Fatalf("init without path err = %v", err)
	}
}

func TestEncryptionStatus(t *testing.T) {
	// Disabled, no key path: healthy, exit 0.
	cfgPath := encConfig(t, "")
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encryption.enabled: false") || !strings.Contains(out, "(not set)") {
		t.Fatalf("status = %q", out)
	}

	// Enabled with a valid key: OK.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "master.key")
	enabledCfg := encConfig(t, fmt.Sprintf(`
encryption:
  enabled: true
  master_key_path: %q
`, keyPath))
	if _, err := runCLI(t, "", "--config", enabledCfg, "encryption", "init"); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", "--config", enabledCfg, "encryption", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encryption.enabled: true") || !strings.Contains(out, "master key: OK") || !strings.Contains(out, "0600") {
		t.Fatalf("status = %q", out)
	}

	// Missing key file: fails closed.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", "--config", enabledCfg, "encryption", "status")
	if err == nil || !strings.Contains(out, "MISSING") {
		t.Fatalf("missing key status = %q %v", out, err)
	}

	// Unusable key file (too-open perms): fails closed.
	if err := os.WriteFile(keyPath, []byte("AAAA\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = runCLI(t, "", "--config", enabledCfg, "encryption", "status")
	if err == nil || !strings.Contains(out, "UNUSABLE") {
		t.Fatalf("bad key status = %q %v", out, err)
	}

	// Enabled without a key path is rejected by config validation.
	noPathCfg := encConfig(t, `
encryption:
  enabled: true
`)
	if _, err := runCLI(t, "", "--config", noPathCfg, "encryption", "status"); err == nil ||
		!strings.Contains(err.Error(), "encryption.master_key_path") {
		t.Fatalf("enabled-without-path err = %v", err)
	}
}

// sweepEnv builds a config with a localfs storage backend and an encryption
// section, generates the master key when enabled, and returns the config
// path plus the storage root.
func sweepEnv(t *testing.T, enabled bool) (cfgPath, storageRoot string) {
	t.Helper()
	dir := t.TempDir()
	storageRoot = filepath.Join(dir, "storage")
	keyPath := filepath.Join(dir, "master.key")
	cfgPath = encConfig(t, fmt.Sprintf(`
storage:
  default_backend: local
  backends:
    local:
      type: localfs
      root: %q
encryption:
  enabled: %v
  master_key_path: %q
`, storageRoot, enabled, keyPath))
	if enabled {
		if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "init"); err != nil {
			t.Fatal(err)
		}
	}
	return cfgPath, storageRoot
}

// writeRaw writes a plaintext file straight into the storage root,
// bypassing the encryption layer.
func writeRaw(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// rawHasMagic reports whether the stored file starts with the encryption
// magic.
func rawHasMagic(t *testing.T, root, rel string) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(string(raw), "NCGOENC1")
}

// writeSealed writes a file through the encryption-wrapped storage the
// server would use, so it lands sealed at rest.
func writeSealed(t *testing.T, cfgPath, rel, content string) {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStorage(cfg)
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

func TestEncryptionSweepCommands(t *testing.T) {
	cfgPath, root := sweepEnv(t, true)
	writeRaw(t, root, "alice/a.txt", "alice plain a")
	writeRaw(t, root, "alice/sub/b.txt", "alice plain b")
	writeRaw(t, root, "bob/c.txt", "bob plain c")
	writeSealed(t, cfgPath, "alice/s.bin", "already sealed")

	// --user limits the sweep to that user's subtree.
	out, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-all", "--user", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encrypt-all: scanned=3 sealed=2 skipped=1 failed=0") {
		t.Fatalf("encrypt-all --user output = %q", out)
	}
	if !rawHasMagic(t, root, "alice/a.txt") || !rawHasMagic(t, root, "alice/sub/b.txt") {
		t.Fatal("alice files must be sealed")
	}
	if rawHasMagic(t, root, "bob/c.txt") {
		t.Fatal("bob/c.txt is outside --user alice and must stay plaintext")
	}

	// Whole tree, dry-run first: counts but does not write.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-all", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry-run encrypt-all: scanned=4 changed=1 skipped=3 failed=0") {
		t.Fatalf("dry-run output = %q", out)
	}
	if rawHasMagic(t, root, "bob/c.txt") {
		t.Fatal("dry-run must not write")
	}

	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "encrypt-all: scanned=4 sealed=1 skipped=3 failed=0") {
		t.Fatalf("encrypt-all output = %q", out)
	}
	if !strings.Contains(out, "encrypt-all: 4/4 files") {
		t.Fatalf("progress line missing: %q", out)
	}
	if !rawHasMagic(t, root, "bob/c.txt") {
		t.Fatal("bob/c.txt must be sealed after the full sweep")
	}

	// Idempotent: a second run skips everything.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sealed=0 skipped=4") {
		t.Fatalf("idempotent output = %q", out)
	}

	// decrypt-all restores the exact plaintext bytes.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "decrypt-all: scanned=4 decrypted=4 skipped=0 failed=0") {
		t.Fatalf("decrypt-all output = %q", out)
	}
	for rel, want := range map[string]string{
		"alice/a.txt":     "alice plain a",
		"alice/sub/b.txt": "alice plain b",
		"bob/c.txt":       "bob plain c",
		"alice/s.bin":     "already sealed",
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != want {
			t.Errorf("%s = %q, want %q", rel, raw, want)
		}
	}

	// A second decrypt-all skips everything.
	out, err = runCLI(t, "", "--config", cfgPath, "encryption", "decrypt-all")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "decrypted=0 skipped=4") {
		t.Fatalf("idempotent decrypt output = %q", out)
	}
}

func TestEncryptionSweepRequiresEnabled(t *testing.T) {
	cfgPath, _ := sweepEnv(t, false)
	for _, sub := range []string{"encrypt-all", "decrypt-all"} {
		if _, err := runCLI(t, "", "--config", cfgPath, "encryption", sub); err == nil ||
			!strings.Contains(err.Error(), "encryption.enabled") {
			t.Fatalf("%s with encryption disabled err = %v", sub, err)
		}
	}
}

func TestEncryptionSweepKeyProblems(t *testing.T) {
	// Enabled but the key file is missing.
	dir := t.TempDir()
	cfgPath := encConfig(t, fmt.Sprintf(`
storage:
  default_backend: local
  backends:
    local:
      type: localfs
      root: %q
encryption:
  enabled: true
  master_key_path: %q
`, filepath.Join(dir, "storage"), filepath.Join(dir, "master.key")))
	if _, err := runCLI(t, "", "--config", cfgPath, "encryption", "encrypt-all"); err == nil ||
		!strings.Contains(err.Error(), "encryption") {
		t.Fatalf("missing key err = %v", err)
	}

	// Invalid --user values are rejected before touching storage.
	cfgPath2, _ := sweepEnv(t, true)
	for _, bad := range []string{"a/b", "..", `a\b`} {
		if _, err := runCLI(t, "", "--config", cfgPath2, "encryption", "encrypt-all", "--user", bad); err == nil ||
			!strings.Contains(err.Error(), "invalid --user") {
			t.Fatalf("--user %q err = %v", bad, err)
		}
	}
}
