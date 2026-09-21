package plugins

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func installManifest(version, caps string) []byte {
	return []byte(`
[plugin]
id = "com.example.inst"
name = "Inst"
version = "` + version + `"
abi = "ncgo-abi/1"
` + caps + `
[entry_points]
module = "hello.wasm"
on_install = "ncgo_on_install"
`)
}

func testInstaller(t *testing.T, trusted []ed25519.PublicKey) *Installer {
	t.Helper()
	h, err := NewHost(context.Background(), HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return &Installer{
		Host:        h,
		Registry:    NewRegistry(testDB(t)),
		InstallDir:  t.TempDir(),
		TrustedKeys: trusted,
		Logger:      slog.New(slog.DiscardHandler),
	}
}

func buildArchive(t *testing.T, manifest []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	members := map[string][]byte{
		"plugin.toml": manifest,
		"hello.wasm":  wasmgen.HelloModule("installed"),
	}
	if priv != nil {
		sig, err := SignMembers(members, priv)
		if err != nil {
			t.Fatal(err)
		}
		members[SignatureFile] = sig
	}
	raw, err := WriteArchive(members)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestInstallSigned(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	row, err := in.Install(context.Background(), raw, InstallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "com.example.inst" || row.Version != "1.0.0" || !row.Enabled {
		t.Fatalf("row = %+v", row)
	}
	if row.SignatureKeyID != KeyIDFromPublic(pub) {
		t.Fatalf("keyid = %q", row.SignatureKeyID)
	}
	if _, err := os.Stat(filepath.Join(in.InstallDir, "com.example.inst", "1.0.0.ncplugin")); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
}

func TestInstallUnsignedPolicy(t *testing.T) {
	in := testInstaller(t, nil)
	raw := buildArchive(t, installManifest("1.0.0", ""), nil)
	if _, err := in.Install(context.Background(), raw, InstallOptions{}); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v", err)
	}
	row, err := in.Install(context.Background(), raw, InstallOptions{ForceUnsigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.SignatureKeyID != "" {
		t.Fatalf("keyid = %q", row.SignatureKeyID)
	}
}

func TestInstallUntrustedSignature(t *testing.T) {
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, nil) // no trusted keys
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(context.Background(), raw, InstallOptions{}); !errors.Is(err, ErrSignatureUntrusted) {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallUpgradeCapsChange(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()

	v1 := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(ctx, v1, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	// Same capabilities: upgrade allowed without re-approval.
	v1Same := buildArchive(t, installManifest("1.0.1", ""), priv)
	if _, err := in.Install(ctx, v1Same, InstallOptions{}); err != nil {
		t.Fatal(err)
	}

	// Changed capabilities: requires ApproveCaps.
	caps := `
[capabilities]
events.publish = ["inst.done"]
`
	v2 := buildArchive(t, installManifest("2.0.0", caps), priv)
	if _, err := in.Install(ctx, v2, InstallOptions{}); !errors.Is(err, ErrCapsChanged) {
		t.Fatalf("err = %v", err)
	}
	row, err := in.Install(ctx, v2, InstallOptions{ApproveCaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != "2.0.0" {
		t.Fatalf("row = %+v", row)
	}
}

func TestUninstall(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	in := testInstaller(t, []ed25519.PublicKey{pub})
	ctx := context.Background()
	raw := buildArchive(t, installManifest("1.0.0", ""), priv)
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Registry.Get(ctx, "com.example.inst"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(in.InstallDir, "com.example.inst")); !os.IsNotExist(err) {
		t.Fatalf("install dir still present: %v", err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadTrustedKeys(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(pub)
	if err := os.WriteFile(filepath.Join(dir, "ops.pub"), []byte(enc+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadTrustedKeys(dir)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v %v", keys, err)
	}
	if KeyIDFromPublic(keys[0]) != KeyIDFromPublic(pub) {
		t.Fatal("key mismatch")
	}
	// Missing directory yields no keys.
	keys, err = LoadTrustedKeys(filepath.Join(dir, "nope"))
	if err != nil || keys != nil {
		t.Fatalf("keys = %v %v", keys, err)
	}
	// Malformed key file is rejected.
	if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("!!!\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustedKeys(dir); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err = %v", err)
	}
}
