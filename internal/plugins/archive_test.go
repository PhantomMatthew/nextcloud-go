package plugins

import (
	"errors"
	"strings"
	"testing"
)

const testManifestTOML = `
[plugin]
id = "com.example.pack"
name = "Pack"
version = "1.0.0"
abi = "ncgo-abi/1"

[entry_points]
module = "pack.wasm"
on_install = "ncgo_on_install"
`

func testMembers() map[string][]byte {
	return map[string][]byte{
		"plugin.toml":  []byte(testManifestTOML),
		"pack.wasm":    {0x00, 0x61, 0x73, 0x6d},
		"README.md":    []byte("# pack"),
		"i18n/en.json": []byte(`{"hello":"world"}`),
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	raw, err := WriteArchive(testMembers())
	if err != nil {
		t.Fatal(err)
	}
	a, err := ReadArchive(raw)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.Plugin.ID != "com.example.pack" {
		t.Fatalf("id = %q", a.Manifest.Plugin.ID)
	}
	if string(a.ManifestRaw) != testManifestTOML {
		t.Fatalf("manifest raw mismatch")
	}
	if len(a.Module) != 4 {
		t.Fatalf("module = %d bytes", len(a.Module))
	}
	if len(a.Files) != 2 || string(a.Files["README.md"]) != "# pack" {
		t.Fatalf("files = %v", a.Files)
	}
	if a.Signature != nil {
		t.Fatal("unexpected signature")
	}
	// Members must rebuild the full signed set.
	members := a.Members()
	if len(members) != 4 || members["pack.wasm"] == nil || members["plugin.toml"] == nil {
		t.Fatalf("members = %v", members)
	}
}

func TestArchiveInvalid(t *testing.T) {
	if _, err := ReadArchive([]byte("not a zip")); !errors.Is(err, ErrArchiveInvalid) {
		t.Fatalf("err = %v", err)
	}
	// Missing plugin.toml.
	raw, err := WriteArchive(map[string][]byte{"x.wasm": {0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(raw); !errors.Is(err, ErrArchiveInvalid) {
		t.Fatalf("err = %v", err)
	}
	// Missing module named by the manifest.
	raw, err = WriteArchive(map[string][]byte{"plugin.toml": []byte(testManifestTOML)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(raw); !errors.Is(err, ErrArchiveInvalid) ||
		!strings.Contains(err.Error(), "pack.wasm") {
		t.Fatalf("err = %v", err)
	}
	// Bad manifest.
	raw, err = WriteArchive(map[string][]byte{
		"plugin.toml": []byte("[plugin]\nid = \"x\"\n"),
		"pack.wasm":   {0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchive(raw); !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("err = %v", err)
	}
}
