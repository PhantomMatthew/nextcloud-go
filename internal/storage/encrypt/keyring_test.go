package encrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

// ringFS builds a localfs-backed encrypt FS whose ring is previous keys
// (IDs 0..n-1) plus the current key (ID n).
func ringFS(t *testing.T, current []byte, previous ...[]byte) (*FS, *localfs.FS) {
	t.Helper()
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs, err := NewWithPrevious(current, previous, inner)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inner
}

func TestNewWithPreviousValidation(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyA, keyB := testKey(t), testKey(t)

	if _, err := NewWithPrevious([]byte("short"), nil, inner); err == nil {
		t.Error("short current key must fail")
	}
	if _, err := NewWithPrevious(keyA, [][]byte{[]byte("short")}, inner); err == nil {
		t.Error("short previous key must fail")
	}
	if _, err := NewWithPrevious(keyA, nil, nil); err == nil {
		t.Error("nil inner must fail")
	}
	if _, err := NewWithPrevious(keyA, [][]byte{keyA}, inner); err == nil ||
		!strings.Contains(err.Error(), "identical") {
		t.Errorf("current duplicated in previous err = %v", err)
	}
	if _, err := NewWithPrevious(keyA, [][]byte{keyB, keyB}, inner); err == nil ||
		!strings.Contains(err.Error(), "identical") {
		t.Errorf("duplicate previous keys err = %v", err)
	}

	// 255 previous keys + the current one fill the ring exactly.
	full := make([][]byte, MaxKeys-1)
	for i := range full {
		k := make([]byte, MasterKeySize)
		k[0] = byte(i)
		k[1] = byte(i >> 8)
		full[i] = k
	}
	if _, err := NewWithPrevious(keyA, full, inner); err != nil {
		t.Errorf("full ring (%d keys) must load: %v", MaxKeys, err)
	}
	tooMany := make([][]byte, MaxKeys)
	copy(tooMany, full)
	tooMany[MaxKeys-1] = testKey(t)
	if _, err := NewWithPrevious(keyA, tooMany, inner); err == nil ||
		!strings.Contains(err.Error(), "max") {
		t.Errorf("%d keys err = %v", MaxKeys+1, err)
	}

	// The ring keys are copied: mutating the caller's slices must not
	// corrupt the FS.
	current, previous := testKey(t), testKey(t)
	fs, err := NewWithPrevious(current, [][]byte{previous}, inner)
	if err != nil {
		t.Fatal(err)
	}
	for i := range current {
		current[i] = 0
	}
	for i := range previous {
		previous[i] = 0
	}
	writeAll(t, fs, "copy.bin", []byte("key copy check"))
	if got := readAll(t, fs, "copy.bin"); string(got) != "key copy check" {
		t.Fatalf("round trip after key mutation = %q", got)
	}
}

func TestSingleKeyWritesV1Header(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// New and NewWithPrevious without previous keys are the same
	// single-key ring and must emit the v1 header bit-identically to
	// pre-keyring deployments.
	mk := func(t *testing.T, current []byte, previous [][]byte) *FS {
		t.Helper()
		f, err := NewWithPrevious(current, previous, inner)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	rings := map[string]*FS{
		"nilPrevious":   mk(t, testKey(t), nil),
		"emptyPrevious": mk(t, testKey(t), [][]byte{}),
	}
	if f, err := New(testKey(t), inner); err != nil {
		t.Fatal(err)
	} else {
		rings["new"] = f
	}
	for name, fs := range rings {
		writeAll(t, fs, name+".bin", []byte("v1 bit-compat content"))
		raw := rawBytes(t, inner, name+".bin")
		if !bytes.Equal(raw[:len(magic)], []byte(magic)) {
			t.Errorf("%s: stored file does not start with the v1 magic", name)
		}
		if got := readAll(t, fs, name+".bin"); string(got) != "v1 bit-compat content" {
			t.Errorf("%s: round trip = %q", name, got)
		}
	}
	// An empty single-key file is the 40-byte v1 header exactly.
	fs, err := New(testKey(t), inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, fs, "empty.bin", nil)
	raw, err := inner.Stat(context.Background(), "empty.bin")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Size != int64(headerSize) {
		t.Fatalf("stored empty file = %d bytes, want the %d-byte v1 header", raw.Size, headerSize)
	}
}

func TestTwoKeyRingWritesV2ID1(t *testing.T) {
	keyA, keyB := testKey(t), testKey(t)
	fs, inner := ringFS(t, keyB, keyA)
	content := bytes.Repeat([]byte("rotate me "), 10000) // multi-chunk
	writeAll(t, fs, "doc.bin", content)

	raw := rawBytes(t, inner, "doc.bin")
	if !bytes.Equal(raw[:len(magicV2)], []byte(magicV2)) {
		t.Fatal("multi-key ring must write the v2 magic")
	}
	if raw[len(magicV2)] != 1 {
		t.Fatalf("v2 key-ID byte = %d, want 1 (current key of a two-key ring)", raw[len(magicV2)])
	}
	if got := readAll(t, fs, "doc.bin"); !bytes.Equal(got, content) {
		t.Fatalf("v2 round trip = %d bytes, want %d", len(got), len(content))
	}

	// An empty v2 file is the 41-byte v2 header exactly.
	writeAll(t, fs, "empty.bin", nil)
	rawInfo, err := inner.Stat(context.Background(), "empty.bin")
	if err != nil {
		t.Fatal(err)
	}
	if rawInfo.Size != int64(headerSizeV2) {
		t.Fatalf("stored empty v2 file = %d bytes, want the %d-byte v2 header", rawInfo.Size, headerSizeV2)
	}
	if got := readAll(t, fs, "empty.bin"); len(got) != 0 {
		t.Fatalf("empty v2 read = %d bytes", len(got))
	}
}

func TestRingReadsV1File(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyA, keyB := testKey(t), testKey(t)
	single, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("sealed under the old master key before rotation")
	writeAll(t, single, "legacy.bin", content)
	plain := []byte("never sealed at all")
	writeAll(t, inner, "plain.txt", plain)

	// The rotation ring holds the old key at ID 0 and seals new writes
	// under the new key at ID 1; the v1 file reads transparently.
	ring, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got := readAll(t, ring, "legacy.bin"); !bytes.Equal(got, content) {
		t.Fatalf("v1 read through ring = %q", got)
	}
	if got := readAll(t, ring, "plain.txt"); !bytes.Equal(got, plain) {
		t.Fatalf("plaintext passthrough = %q", got)
	}
	info, err := ring.Stat(ctx, "legacy.bin")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("v1 Stat = %d, want %d", info.Size, len(content))
	}
	entries, err := ring.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Path] = e.Size
	}
	if sizes["legacy.bin"] != int64(len(content)) || sizes["plain.txt"] != int64(len(plain)) {
		t.Fatalf("List sizes = %v", sizes)
	}
}

// TestV2IDZeroRead pins the v2 read path for key ID 0: the ID byte selects
// ring key 0 and the salt shifts one byte right.
func TestV2IDZeroRead(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyA, keyB := testKey(t), testKey(t)
	single, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hand-crafted v2 header with key id zero")
	writeAll(t, single, "crafted.bin", content)

	// Rewrite the v1 header as v2 with ID 0: same salt, same data key.
	raw := rawBytes(t, inner, "crafted.bin")
	v2 := append(append(append([]byte{}, []byte(magicV2)...), 0x00), raw[len(magic):]...)
	rewriteRaw(t, inner, "crafted.bin", v2)

	ring, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, ring, "crafted.bin"); !bytes.Equal(got, content) {
		t.Fatalf("v2 ID-0 read = %q", got)
	}
	info, err := ring.Stat(context.Background(), "crafted.bin")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) {
		t.Fatalf("v2 ID-0 Stat = %d, want %d", info.Size, len(content))
	}
}

func TestUnknownKeyID(t *testing.T) {
	keyA, keyB := testKey(t), testKey(t)
	ring, inner := ringFS(t, keyB, keyA)
	writeAll(t, ring, "sealed.bin", []byte("sealed under key id 1"))

	// A ring without key ID 1 (key B alone) fails Open and Stat with
	// ErrUnknownKeyID — the operator removed a key from the ring, a config
	// error, distinguishable from ErrIntegrity (wrong key / corruption).
	alone, err := New(keyB, inner)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = alone.Open(ctx, "sealed.bin")
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("Open err = %v, want ErrUnknownKeyID", err)
	}
	if errors.Is(err, ErrIntegrity) {
		t.Fatalf("Open err = %v must not be ErrIntegrity", err)
	}
	if !strings.Contains(err.Error(), "sealed.bin") || !strings.Contains(err.Error(), "id 1") {
		t.Fatalf("err must name the path and id: %v", err)
	}
	if _, err := alone.Stat(ctx, "sealed.bin"); !errors.Is(err, ErrUnknownKeyID) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("Stat err = %v, want ErrUnknownKeyID (not ErrIntegrity)", err)
	}

	// A known ID with the wrong key material is ErrIntegrity, not
	// ErrUnknownKeyID.
	wrongRing, err := NewWithPrevious(testKey(t), [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := wrongRing.Open(ctx, "sealed.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("wrong-key read err = %v, want ErrIntegrity (not ErrUnknownKeyID)", err)
	}
}

func TestV2StatListSizes(t *testing.T) {
	fs, _ := ringFS(t, testKey(t), testKey(t))
	ctx := context.Background()
	sizes := []int{0, 1, 100, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 17}
	for i, size := range sizes {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		p := fmt.Sprintf("dir/f%d.bin", i)
		writeAll(t, fs, p, data)
		info, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(size) {
			t.Errorf("size %d: Stat reports %d", size, info.Size)
		}
	}
	entries, err := fs.List(ctx, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(sizes) {
		t.Fatalf("List = %d entries, want %d", len(entries), len(sizes))
	}
	for i, e := range entries {
		if e.Size != int64(sizes[i]) {
			t.Errorf("%s: List size = %d, want %d", e.Path, e.Size, sizes[i])
		}
	}
}

func TestLoadKeyring(t *testing.T) {
	dir := t.TempDir()
	writeKeyFile := func(name string, key []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keyA, keyB := testKey(t), testKey(t)
	pathA := writeKeyFile("a.key", keyA)
	pathB := writeKeyFile("b.key", keyB)

	current, previous, err := LoadKeyring(pathB, []string{pathA})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, keyB) || len(previous) != 1 || !bytes.Equal(previous[0], keyA) {
		t.Fatal("keyring mismatch")
	}

	// A missing previous key fails fast and names the offending path.
	missing := filepath.Join(dir, "missing.key")
	if _, _, err := LoadKeyring(pathB, []string{pathA, missing}); err == nil ||
		!strings.Contains(err.Error(), missing) {
		t.Fatalf("missing previous key err = %v", err)
	}
	// A missing master key fails too.
	if _, _, err := LoadKeyring(missing, []string{pathA}); err == nil {
		t.Fatal("missing master key must fail")
	}
}
