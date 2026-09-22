package encrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func testFS(t *testing.T) (*FS, *localfs.FS) {
	t.Helper()
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs, err := New(testKey(t), inner)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inner
}

func writeAll(t *testing.T, fs storage.Storage, p string, data []byte) {
	t.Helper()
	ctx := context.Background()
	wc, err := fs.Create(ctx, p, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, fs storage.Storage, p string) []byte {
	t.Helper()
	rc, err := fs.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestNewValidatesKey(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New([]byte("short"), inner); err == nil {
		t.Fatal("short key must fail")
	}
	key := testKey(t)
	fs, err := New(key, inner)
	if err != nil {
		t.Fatal(err)
	}
	// The key is copied: mutating the caller's slice must not corrupt the FS.
	for i := range key {
		key[i] = 0
	}
	writeAll(t, fs, "a.txt", []byte("hello"))
	if got := readAll(t, fs, "a.txt"); string(got) != "hello" {
		t.Fatalf("round trip after key mutation = %q", got)
	}
	if _, err := New(testKey(t), nil); err == nil {
		t.Fatal("nil inner must fail")
	}
}

func TestRoundTripSizes(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	sizes := []int{0, 1, 100, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 17, 5 * 1024 * 1024}
	for _, size := range sizes {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join("dir", strings.ReplaceAll(t.Name(), "/", "_")+".bin")
		writeAll(t, fs, p, data)
		got := readAll(t, fs, p)
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip mismatch (got %d bytes)", size, len(got))
		}

		info, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(size) {
			t.Errorf("size %d: Stat reports %d", size, info.Size)
		}

		raw, err := inner.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if size > 0 && raw.Size <= int64(size) {
			t.Errorf("size %d: stored size %d should exceed plaintext (header+tags)", size, raw.Size)
		}
		entries, err := fs.List(ctx, "dir")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Size != int64(size) {
			t.Errorf("size %d: List = %+v", size, entries)
		}
		if err := fs.Delete(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyFileIsHeaderOnly(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	writeAll(t, fs, "empty.bin", nil)
	raw, err := inner.Stat(ctx, "empty.bin")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Size != int64(headerSize) {
		t.Fatalf("stored empty file = %d bytes, want header only (%d)", raw.Size, headerSize)
	}
	if got := readAll(t, fs, "empty.bin"); len(got) != 0 {
		t.Fatalf("read = %d bytes", len(got))
	}
}

func TestSeek(t *testing.T) {
	fs, _ := testFS(t)
	data := make([]byte, 2*ChunkSize+1000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	writeAll(t, fs, "seek.bin", data)

	rc, err := fs.Open(context.Background(), "seek.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	for _, off := range []int64{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, ChunkSize + 500, 2 * ChunkSize, int64(len(data)) - 1} {
		if _, err := rc.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data[off:]) {
			t.Fatalf("seek %d: read mismatch (%d bytes)", off, len(got))
		}
	}

	// SeekEnd and SeekCurrent.
	if _, err := rc.Seek(-10, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail := make([]byte, 10)
	if _, err := io.ReadFull(rc, tail); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, data[len(data)-10:]) {
		t.Fatal("SeekEnd tail mismatch")
	}
	if _, err := rc.Seek(-20, io.SeekCurrent); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[len(data)-20:]) {
		t.Fatal("SeekCurrent mismatch")
	}
	if _, err := rc.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek must fail")
	}
	if _, err := rc.Seek(0, 99); err == nil {
		t.Fatal("invalid whence must fail")
	}
	// Seeking past the end reads EOF.
	if _, err := rc.Seek(int64(len(data))+100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read past end = %d, %v", n, err)
	}
}

func TestLegacyPlaintextPassthrough(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	plain := []byte("written before encryption was enabled")
	writeAll(t, inner, "legacy.txt", plain)

	if got := readAll(t, fs, "legacy.txt"); !bytes.Equal(got, plain) {
		t.Fatalf("passthrough = %q", got)
	}
	info, err := fs.Stat(ctx, "legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(plain)) {
		t.Fatalf("Stat size = %d", info.Size)
	}
	entries, err := fs.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Size != int64(len(plain)) {
		t.Fatalf("List = %+v", entries)
	}
	// Short files (< magic length) pass through as well.
	writeAll(t, inner, "tiny.txt", []byte("abc"))
	if got := readAll(t, fs, "tiny.txt"); string(got) != "abc" {
		t.Fatalf("tiny passthrough = %q", got)
	}
}

func TestMixedDirectory(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	writeAll(t, inner, "old.txt", []byte("legacy plaintext content"))
	writeAll(t, fs, "new.txt", []byte("freshly sealed content here"))
	entries, err := fs.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int64{}
	for _, e := range entries {
		sizes[e.Path] = e.Size
	}
	if sizes["old.txt"] != int64(len("legacy plaintext content")) {
		t.Errorf("old.txt size = %d", sizes["old.txt"])
	}
	if sizes["new.txt"] != int64(len("freshly sealed content here")) {
		t.Errorf("new.txt size = %d", sizes["new.txt"])
	}
	if got := readAll(t, fs, "new.txt"); string(got) != "freshly sealed content here" {
		t.Fatalf("new.txt = %q", got)
	}
}

// rawBytes reads the sealed (or plaintext) bytes straight from the backend.
func rawBytes(t *testing.T, inner storage.Storage, p string) []byte {
	t.Helper()
	rc, err := inner.Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// rewriteRaw replaces the stored bytes, bypassing encryption.
func rewriteRaw(t *testing.T, inner storage.Storage, p string, data []byte) {
	t.Helper()
	writeAll(t, inner, p, data)
}

func TestTamperedCiphertextDetected(t *testing.T) {
	fs, inner := testFS(t)
	writeAll(t, fs, "doc.bin", bytes.Repeat([]byte("A"), ChunkSize+10))

	raw := rawBytes(t, inner, "doc.bin")
	if !bytes.Equal(raw[:len(magic)], []byte(magic)) {
		t.Fatal("stored file must carry the magic header")
	}
	// Flip a byte inside the first chunk's ciphertext.
	raw[headerSize+20] ^= 0xFF
	rewriteRaw(t, inner, "doc.bin", raw)

	rc, err := fs.Open(context.Background(), "doc.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered read err = %v, want ErrIntegrity", err)
	}
}

func TestTamperedSaltDetected(t *testing.T) {
	fs, inner := testFS(t)
	writeAll(t, fs, "salt.bin", []byte("some content worth protecting"))
	raw := rawBytes(t, inner, "salt.bin")
	raw[len(magic)+3] ^= 0x01 // corrupt the salt -> wrong data key
	rewriteRaw(t, inner, "salt.bin", raw)

	rc, err := fs.Open(context.Background(), "salt.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("salt-tampered read err = %v, want ErrIntegrity", err)
	}
}

func TestWrongMasterKey(t *testing.T) {
	fs, inner := testFS(t)
	writeAll(t, fs, "secret.bin", []byte("confidential payload"))

	other, err := New(testKey(t), inner)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := other.Open(context.Background(), "secret.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong-key read err = %v, want ErrIntegrity", err)
	}
}

func TestTruncatedFileDetected(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	writeAll(t, fs, "trunc.bin", bytes.Repeat([]byte("B"), ChunkSize+100))

	// Truncate mid-chunk: the last sealed chunk loses bytes.
	raw := rawBytes(t, inner, "trunc.bin")
	rewriteRaw(t, inner, "trunc.bin", raw[:len(raw)-10])
	rc, err := fs.Open(ctx, "trunc.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); err == nil {
		t.Error("mid-chunk truncation must fail reads")
	}
	_ = rc.Close()

	// Truncate inside the header.
	writeAll(t, fs, "hdr.bin", []byte("header truncation test content"))
	raw = rawBytes(t, inner, "hdr.bin")
	rewriteRaw(t, inner, "hdr.bin", raw[:headerSize-5])
	if _, err := fs.Open(ctx, "hdr.bin"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("truncated header open err = %v, want ErrIntegrity", err)
	}
	if _, err := fs.Stat(ctx, "hdr.bin"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("truncated header stat err = %v, want ErrIntegrity", err)
	}
}

func TestCiphertextUniqueness(t *testing.T) {
	fs, inner := testFS(t)
	content := bytes.Repeat([]byte("identical-content-"), 1000)
	writeAll(t, fs, "one.bin", content)
	writeAll(t, fs, "two.bin", content)
	a := rawBytes(t, inner, "one.bin")
	b := rawBytes(t, inner, "two.bin")
	if bytes.Equal(a, b) {
		t.Fatal("identical content in two files must not produce identical ciphertext (random per-file salt)")
	}
	// Same path rewritten also reseals with a fresh salt.
	writeAll(t, fs, "one.bin", content)
	c := rawBytes(t, inner, "one.bin")
	if bytes.Equal(a, c) {
		t.Fatal("rewriting a file must not reuse the salt")
	}
}

func TestPassthroughOps(t *testing.T) {
	fs, _ := testFS(t)
	ctx := context.Background()
	if err := fs.Mkdir(ctx, "sub"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "sub"); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("Mkdir err = %v", err)
	}
	writeAll(t, fs, "sub/f.txt", []byte("rename me"))
	if err := fs.Rename(ctx, "sub/f.txt", "sub/g.txt"); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, fs, "sub/g.txt"); string(got) != "rename me" {
		t.Fatalf("after rename = %q", got)
	}
	if err := fs.Delete(ctx, "sub"); !errors.Is(err, storage.ErrNotEmpty) {
		t.Fatalf("Delete non-empty err = %v", err)
	}
	if _, err := fs.Stat(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat err = %v", err)
	}
	if _, err := fs.Open(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open err = %v", err)
	}
	if _, err := fs.Create(ctx, "../escape", 0); !errors.Is(err, storage.ErrInvalidPath) {
		t.Fatalf("Create err = %v", err)
	}
	// Listing a directory with a subdirectory does not touch sizes.
	entries, err := fs.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	foundDir := false
	for _, e := range entries {
		if e.Path == "sub" && e.IsDir {
			foundDir = true
		}
	}
	if !foundDir {
		t.Fatalf("List = %+v", entries)
	}
}

func TestLoadMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	key := testKey(t)
	writeKey := func(content string, perm os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), perm); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, perm); err != nil {
			t.Fatal(err)
		}
	}

	writeKey(base64.StdEncoding.EncodeToString(key)+"\n", 0o600)
	got, err := LoadMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("loaded key mismatch")
	}

	writeKey(base64.StdEncoding.EncodeToString(key), 0o644)
	if _, err := LoadMasterKey(path); err == nil || !strings.Contains(err.Error(), "chmod 0600") {
		t.Fatalf("group-readable key err = %v", err)
	}

	writeKey("not!base64!", 0o600)
	if _, err := LoadMasterKey(path); err == nil || !strings.Contains(err.Error(), "base64") {
		t.Fatalf("bad base64 err = %v", err)
	}

	writeKey(base64.StdEncoding.EncodeToString([]byte("16-bytes-of-key!")), 0o600)
	if _, err := LoadMasterKey(path); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("short key err = %v", err)
	}

	if _, err := LoadMasterKey(filepath.Join(dir, "missing.key")); err == nil {
		t.Fatal("missing key file must fail")
	}
	if _, err := LoadMasterKey("  "); err == nil {
		t.Fatal("empty path must fail")
	}
}
