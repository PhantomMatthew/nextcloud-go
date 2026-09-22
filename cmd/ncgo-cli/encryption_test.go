package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
