package encrypt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

// stubResolver is an in-memory KeyResolver for envelope tests: Allocate
// mints a fresh (uuid, FK) pair, Resolve fails with ErrUnresolvableKey for
// UUIDs it never handed out.
type stubResolver struct {
	mu       sync.Mutex
	fks      map[[16]byte][]byte
	allocKey []string
}

func newStubResolver() *stubResolver {
	return &stubResolver{fks: make(map[[16]byte][]byte)}
}

func (s *stubResolver) Allocate(_ context.Context, storageKey string) ([16]byte, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fk := make([]byte, fileKeySize)
	if _, err := rand.Read(fk); err != nil {
		return [16]byte{}, nil, err
	}
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return [16]byte{}, nil, err
	}
	s.fks[uuid] = fk
	s.allocKey = append(s.allocKey, storageKey)
	return uuid, fk, nil
}

func (s *stubResolver) Resolve(_ context.Context, keyUUID [16]byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fk, ok := s.fks[keyUUID]
	if !ok {
		return nil, fmt.Errorf("stub: key uuid %s: %w", hex.EncodeToString(keyUUID[:]), ErrUnresolvableKey)
	}
	return fk, nil
}

// resolverFS builds a localfs-backed encrypt FS carrying a stub resolver.
func resolverFS(t *testing.T, res KeyResolver) (*FS, *localfs.FS) {
	t.Helper()
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs, err := NewWithResolver(testKey(t), nil, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inner
}

// writeV3 writes through Create and returns the key UUID the writer
// reported via storage.KeyUUIDWriter.
func writeV3(t *testing.T, fs storage.Storage, p string, data []byte) [16]byte {
	t.Helper()
	wc, err := fs.Create(context.Background(), p, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(data); err != nil {
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
		t.Fatal("v3 writer reported no key uuid")
	}
	return uuid
}

func TestV3RoundTrip(t *testing.T) {
	res := newStubResolver()
	fs, inner := resolverFS(t, res)
	ctx := context.Background()

	sizes := map[string]int{
		"empty.bin": 0,
		"one.bin":   1,
		"hundred":   100,
		"edge-1":    ChunkSize - 1,
		"edge":      ChunkSize,
		"edge+1":    ChunkSize + 1,
		"multi":     3*ChunkSize + 17,
	}
	for p, size := range sizes {
		data := bytes.Repeat([]byte{0x5A}, size)
		uuid := writeV3(t, fs, p, data)
		raw := rawBytes(t, inner, p)
		if string(raw[:len(magicV3)]) != magicV3 {
			t.Fatalf("%s: missing v3 magic", p)
		}
		if !bytes.Equal(raw[len(magicV3):len(magicV3)+keyUUIDSize], uuid[:]) {
			t.Errorf("%s: header key uuid mismatch", p)
		}
		wantStored := int64(headerSizeV3) + int64(size) + int64((size+ChunkSize-1)/ChunkSize)*tagSize
		if size == 0 {
			wantStored = int64(headerSizeV3) // empty file stores the 56-byte header only
		}
		if int64(len(raw)) != wantStored {
			t.Errorf("%s: stored size = %d, want %d", p, len(raw), wantStored)
		}
		if got := readAll(t, fs, p); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip = %d bytes, want %d", p, len(got), len(data))
		}
		info, err := fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(size) {
			t.Errorf("%s: stat size = %d, want %d", p, info.Size, size)
		}
	}
	// Every write allocated through the resolver with the file's storage key.
	if len(res.allocKey) != len(sizes) {
		t.Fatalf("allocations = %d, want %d", len(res.allocKey), len(sizes))
	}

	// List reports plaintext sizes too.
	infos, err := fs.List(ctx, ".")
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]int64, len(infos))
	for _, fi := range infos {
		byPath[fi.Path] = fi.Size
	}
	for p, size := range sizes {
		if byPath[p] != int64(size) {
			t.Errorf("list %s size = %d, want %d", p, byPath[p], size)
		}
	}
}

func TestV3HeaderSniff(t *testing.T) {
	v1 := append([]byte(magic), make([]byte, saltSize)...)
	v2 := append(append([]byte(magicV2), 0x07), make([]byte, saltSize)...)
	v3head := append(append([]byte(magicV3), make([]byte, keyUUIDSize)...), make([]byte, saltSize)...)
	for _, tc := range []struct {
		head    []byte
		version headerVersion
		keyID   int
		size    int
	}{
		{v1, versionV1, 0, headerSize},
		{v2, versionV2, 7, headerSizeV2},
		{v3head, versionV3, 0, headerSizeV3},
	} {
		l, ok := headerLayout(tc.head)
		if !ok || l.version != tc.version || l.keyID != tc.keyID || l.size != tc.size {
			t.Errorf("headerLayout(%q...) = %+v %v, want version %d keyID %d size %d",
				tc.head[:8], l, ok, tc.version, tc.keyID, tc.size)
		}
	}
	if _, ok := headerLayout([]byte("plain text file")); ok {
		t.Error("plaintext must not sniff as sealed")
	}
	if maxHeader != headerSizeV3 {
		t.Errorf("maxHeader = %d, want %d", maxHeader, headerSizeV3)
	}
}

func TestV3UnknownKeyUUID(t *testing.T) {
	res := newStubResolver()
	fs, _ := resolverFS(t, res)
	uuid := writeV3(t, fs, "a.bin", []byte("payload"))

	// Forget the key: the resolver can no longer unwrap it.
	res.mu.Lock()
	delete(res.fks, uuid)
	res.mu.Unlock()

	rc, err := fs.Open(context.Background(), "a.bin")
	if err == nil {
		_ = rc.Close()
		t.Fatal("open with unknown key uuid must fail")
	}
	if !errors.Is(err, ErrUnresolvableKey) {
		t.Errorf("err = %v, want ErrUnresolvableKey", err)
	}
	if errors.Is(err, ErrIntegrity) {
		t.Error("unresolvable key must never surface as ErrIntegrity")
	}
	if !strings.Contains(err.Error(), hex.EncodeToString(uuid[:])) {
		t.Errorf("err must name the key uuid: %v", err)
	}
}

func TestV3NilResolverOpen(t *testing.T) {
	res := newStubResolver()
	fs, inner := resolverFS(t, res)
	writeV3(t, fs, "v3.bin", []byte("v3 payload"))

	// The same backend and ring without a resolver: opening the v3 file is
	// ErrUnresolvableKey, not a panic and not ErrIntegrity.
	plain, err := New(testKey(t), inner)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := plain.Open(context.Background(), "v3.bin")
	if err == nil {
		_ = rc.Close()
		t.Fatal("v3 file on a nil-resolver FS must fail")
	}
	if !errors.Is(err, ErrUnresolvableKey) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrUnresolvableKey (never ErrIntegrity)", err)
	}

	// Stat stays DB-free: sizes come from the header alone, no resolver
	// needed, and the v2 ring-key check does not apply to v3.
	info, err := plain.Stat(context.Background(), "v3.bin")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len("v3 payload")) {
		t.Errorf("nil-resolver stat size = %d", info.Size)
	}
}

func TestV3Tamper(t *testing.T) {
	res := newStubResolver()
	fs, inner := resolverFS(t, res)
	writeV3(t, fs, "tamper.bin", bytes.Repeat([]byte{0x11}, 100))

	// Flip one ciphertext byte (past the 56-byte header).
	raw := rawBytes(t, inner, "tamper.bin")
	raw[headerSizeV3+3] ^= 0xFF
	writeAll(t, inner, "tamper.bin", raw)

	rc, err := fs.Open(context.Background(), "tamper.bin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("read err = %v, want ErrIntegrity", err)
	}
}

func TestV3MixedTree(t *testing.T) {
	// One backend, three writers: v1 (single-key ring), v2 (two-key ring at
	// ID 1), v3 (resolver). The resolver FS reads them all.
	keyA, keyB := testKey(t), testKey(t)
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	v1fs, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	v2fs, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	res := newStubResolver()
	v3fs, err := NewWithResolver(keyB, [][]byte{keyA}, inner, res)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	writeAll(t, v1fs, "old/v1.bin", []byte("v1 content"))
	writeAll(t, v2fs, "mid/v2.bin", []byte("v2 content!"))
	writeV3(t, v3fs, "new/v3.bin", []byte("v3 content"))
	writeAll(t, inner, "plain.txt", []byte("legacy plain"))

	for p, want := range map[string]string{
		"old/v1.bin": "v1 content",
		"mid/v2.bin": "v2 content!",
		"new/v3.bin": "v3 content",
		"plain.txt":  "legacy plain",
	} {
		if got := readAll(t, v3fs, p); string(got) != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
		info, err := v3fs.Stat(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size != int64(len(want)) {
			t.Errorf("%s stat = %d, want %d", p, info.Size, len(want))
		}
	}
}
