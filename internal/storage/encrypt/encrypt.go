// Package encrypt implements transparent at-rest encryption as a
// storage.Storage decorator: AES-256-GCM with 64 KiB chunked framing,
// a per-file random salt, and per-chunk nonces derived from a per-file
// data key (HMAC-SHA256 of the file key over the salt; for v1/v2 files the
// file key is a ring key, for v3 files a resolver-unwrapped per-user file
// key). Files written through the decorator are sealed; reads auto-detect
// the magic header, so encrypted files decrypt transparently while legacy
// plaintext files pass through untouched. See ADR-0052 for the threat
// model and the v1 format, ADR-0074 for the v2 key-ID header and master-key
// rotation, ADR-0097 for the v3 per-user-key envelope and the KeyResolver
// seam.
package encrypt

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

const (
	// MasterKeySize is the required master key length in bytes (AES-256).
	MasterKeySize = 32
	// ChunkSize is the plaintext size sealed per chunk.
	ChunkSize = 64 * 1024
	// MaxKeys caps the keyring: the v2 key-ID byte addresses IDs 0..255.
	MaxKeys = 256

	tagSize      = 16 // AES-GCM authentication tag
	nonceSize    = 12 // AES-GCM nonce
	saltSize     = 32
	keyUUIDSize  = 16
	headerSize   = len(magic) + saltSize                 // v1 header
	headerSizeV2 = len(magicV2) + 1 + saltSize           // v2 header: magic + key-ID byte + salt
	headerSizeV3 = len(magicV3) + keyUUIDSize + saltSize // v3 header: magic + key UUID + salt
	maxHeader    = headerSizeV3                          // read budget for header sniffing
)

// magic marks sealed files; reads auto-detect it to distinguish encrypted
// files from legacy plaintext. The last character is the format version:
// v1 headers are magic + salt with implicit key ID 0, v2 headers insert a
// key-ID byte between magic and salt (ADR-0074), v3 headers insert a 16-byte
// key UUID naming a per-user wrapped file key (ADR-0097).
const (
	magic   = "NCGOENC1"
	magicV2 = "NCGOENC2"
	magicV3 = "NCGOENC3"
)

// ErrIntegrity reports a failed GCM authentication (tampered ciphertext,
// wrong key, or a corrupted chunk).
var ErrIntegrity = errors.New("encrypt: integrity check failed")

// ErrUnknownKeyID reports a v2-sealed file whose key-ID byte names a key
// the configured keyring does not hold — an operator configuration error
// (a previous key was removed or the ring was reordered), distinct from
// ErrIntegrity's wrong-key/corruption signal.
var ErrUnknownKeyID = errors.New("encrypt: unknown key id")

// FS is a storage.Storage decorator sealing file contents at rest. keys is
// the keyring: positional key IDs 0..n-1 are previous (read-only) keys and
// the last entry is the current key, which seals all new writes. resolver,
// when non-nil, switches writes to the v3 per-user-key envelope (ADR-0097);
// a nil resolver keeps the FS v1/v2-only and bit-identical.
type FS struct {
	inner    storage.Storage
	keys     [][]byte
	resolver KeyResolver
}

// New wraps inner with transparent encryption under a single master key.
// masterKey must be exactly MasterKeySize bytes and is copied. A single-key
// ring reads and writes the v1 format bit-identically to pre-keyring
// deployments (ADR-0074).
func New(masterKey []byte, inner storage.Storage) (*FS, error) {
	return NewWithPrevious(masterKey, nil, inner)
}

// NewWithPrevious wraps inner with a keyring for master-key rotation
// (ADR-0074): previous holds retired keys (their positions are their key
// IDs, 0..n-1) and current seals new writes at key ID n. Reads pick the
// ring key named by each file's header. Every key must be exactly
// MasterKeySize bytes, the ring may hold at most MaxKeys keys, and no two
// entries may be byte-identical (ambiguous IDs are a misconfiguration).
// All keys are copied.
func NewWithPrevious(current []byte, previous [][]byte, inner storage.Storage) (*FS, error) {
	return NewWithResolver(current, previous, inner, nil)
}

// NewWithResolver is NewWithPrevious plus a per-user KeyResolver
// (ADR-0097): with a non-nil resolver, Create seals new files with the v3
// envelope (a random per-file key wrapped for the storage key's owner) and
// Open resolves v3 headers through it. A nil resolver keeps the v1/v2
// behavior bit-identical.
func NewWithResolver(current []byte, previous [][]byte, inner storage.Storage, res KeyResolver) (*FS, error) {
	if inner == nil {
		return nil, fmt.Errorf("encrypt: inner storage is nil")
	}
	if len(previous)+1 > MaxKeys {
		return nil, fmt.Errorf("encrypt: keyring holds %d keys, max %d", len(previous)+1, MaxKeys)
	}
	keys := make([][]byte, 0, len(previous)+1)
	for i, k := range previous {
		if len(k) != MasterKeySize {
			return nil, fmt.Errorf("encrypt: previous key %d must be %d bytes, got %d", i, MasterKeySize, len(k))
		}
		keys = append(keys, append([]byte(nil), k...))
	}
	if len(current) != MasterKeySize {
		return nil, fmt.Errorf("encrypt: master key must be %d bytes, got %d", MasterKeySize, len(current))
	}
	keys = append(keys, append([]byte(nil), current...))
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				return nil, fmt.Errorf("encrypt: keys %d and %d are identical; key IDs must be unambiguous", i, j)
			}
		}
	}
	return &FS{inner: inner, keys: keys, resolver: res}, nil
}

// currentKeyID is the key-ID byte written into v2 headers and the ring
// position of the key that seals new writes.
func (f *FS) currentKeyID() int {
	return len(f.keys) - 1
}

// singleKey reports whether the ring holds exactly one key; such rings keep
// the v1 wire format (no key-ID byte) for rollback compatibility.
func (f *FS) singleKey() bool {
	return len(f.keys) == 1
}

// LoadMasterKey reads a base64-encoded 32-byte master key from path. The
// file must not be readable or writable by group/other (perm bits ≤ 0600;
// the check is skipped on Windows where mode bits are not meaningful).
// Key material never appears in returned errors.
func LoadMasterKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("encrypt: master key path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("encrypt: stat master key: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("encrypt: master key file %s is accessible by group/other (mode %04o); chmod 0600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("encrypt: read master key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("encrypt: master key is not valid base64: %w", err)
	}
	if len(key) != MasterKeySize {
		return nil, fmt.Errorf("encrypt: master key must decode to %d bytes, got %d", MasterKeySize, len(key))
	}
	return key, nil
}

// LoadKeyring loads the current master key plus every configured previous
// key for a rotation keyring (ADR-0074), failing fast with an error that
// names the offending path. The result is ready for NewWithPrevious.
func LoadKeyring(masterPath string, previousPaths []string) ([]byte, [][]byte, error) {
	current, err := LoadMasterKey(masterPath)
	if err != nil {
		return nil, nil, err
	}
	previous := make([][]byte, 0, len(previousPaths))
	for _, p := range previousPaths {
		key, err := LoadMasterKey(p)
		if err != nil {
			return nil, nil, fmt.Errorf("encrypt: previous key %s: %w", p, err)
		}
		previous = append(previous, key)
	}
	return current, previous, nil
}

// dataKey derives the per-file data key from a ring key and the salt.
func dataKey(key, salt []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(salt) // hash.Hash.Write never errors
	return mac.Sum(nil)
}

func nonce(buf []byte, chunk uint64) {
	clear(buf[:4])
	binary.BigEndian.PutUint64(buf[4:], chunk)
}

// headerVersion identifies the sealed-file format generation.
type headerVersion int

const (
	versionV1 headerVersion = 1
	versionV2 headerVersion = 2
	versionV3 headerVersion = 3
)

// layout describes a sealed file's header: its format version, the ring key
// ID (v2 only; v1 implies ID 0 and v3 is not ring-addressed — it names a
// wrapped file key by UUID instead), and the header size in bytes.
type layout struct {
	version headerVersion
	keyID   int
	size    int
}

// headerLayout identifies the sealed-file format from a file's leading
// bytes: the v1 magic means key ID 0 and the v1 header size; the v2 magic
// means the key-ID byte at offset len(magicV2); the v3 magic means the key
// UUID at offset len(magicV3). ok is false when the bytes do not carry any
// magic (legacy plaintext). A head shorter than magic+1 reports key ID 0
// for v2; callers validate the full header length before trusting the ID.
func headerLayout(head []byte) (layout, bool) {
	if len(head) < len(magic) {
		return layout{}, false
	}
	switch string(head[:len(magic)]) {
	case magic:
		return layout{version: versionV1, size: headerSize}, true
	case magicV2:
		l := layout{version: versionV2, size: headerSizeV2}
		if len(head) > len(magicV2) {
			l.keyID = int(head[len(magicV2)])
		}
		return l, true
	case magicV3:
		return layout{version: versionV3, size: headerSizeV3}, true
	}
	return layout{}, false
}

// ringKey resolves a header key ID to its ring key, failing with
// ErrUnknownKeyID (naming the ID and path) when the ring does not hold it.
func (f *FS) ringKey(id int, path string) ([]byte, error) {
	if id < 0 || id >= len(f.keys) {
		return nil, fmt.Errorf("encrypt: %s: key id %d not in the keyring (%d keys): %w", path, id, len(f.keys), ErrUnknownKeyID)
	}
	return f.keys[id], nil
}

// Stat reports plaintext sizes for encrypted files.
func (f *FS) Stat(ctx context.Context, p string) (*storage.FileInfo, error) {
	info, err := f.inner.Stat(ctx, p)
	if err != nil {
		return nil, err
	}
	return f.plainInfo(ctx, info)
}

// plainInfo rewrites info.Size to the plaintext size when the file carries
// the encryption magic; plaintext files (and directories) pass through.
// The magic plus the v2 key-ID byte (9 bytes) are read to pick the right
// header layout; the size math is otherwise unchanged. Stat stays DB-free:
// v3 headers contribute only their size — the key UUID is never resolved
// here, and the ring-key existence check applies to v1/v2 only.
func (f *FS) plainInfo(ctx context.Context, info *storage.FileInfo) (*storage.FileInfo, error) {
	if info.IsDir || info.Size < int64(len(magic)) {
		return info, nil
	}
	rc, err := f.inner.Open(ctx, info.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, len(magic)+1)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("encrypt: read header of %s: %w", info.Path, err)
	}
	l, ok := headerLayout(head[:n])
	if !ok {
		return info, nil
	}
	if info.Size < int64(l.size) {
		return nil, fmt.Errorf("encrypt: %s: truncated encryption header: %w", info.Path, ErrIntegrity)
	}
	if l.version != versionV3 {
		if _, err := f.ringKey(l.keyID, info.Path); err != nil {
			return nil, err
		}
	}
	payload := info.Size - int64(l.size)
	chunks := (payload + ChunkSize + tagSize - 1) / (ChunkSize + tagSize)
	info.Size = payload - chunks*tagSize
	return info, nil
}

// Open decrypts encrypted files chunk-by-chunk and passes legacy plaintext
// files through untouched. v1 headers read with ring key 0; v2 headers name
// their key by ID and fail with ErrUnknownKeyID when the ring lacks it; v3
// headers name a wrapped file key by UUID and resolve it through the FS's
// KeyResolver, failing with ErrUnresolvableKey when the key cannot be
// unwrapped (or no resolver is configured).
func (f *FS) Open(ctx context.Context, p string) (io.ReadSeekCloser, error) {
	rc, err := f.inner.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	head := make([]byte, maxHeader)
	n, readErr := io.ReadFull(rc, head)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		_ = rc.Close()
		return nil, fmt.Errorf("encrypt: read header of %s: %w", p, readErr)
	}
	l, ok := headerLayout(head[:n])
	if !ok {
		// Legacy plaintext: rewind and hand the raw handle to the caller.
		if _, err := rc.Seek(0, io.SeekStart); err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("encrypt: rewind %s: %w", p, err)
		}
		return rc, nil
	}
	if n < l.size {
		_ = rc.Close()
		return nil, fmt.Errorf("encrypt: %s: truncated encryption header: %w", p, ErrIntegrity)
	}
	salt := head[l.size-saltSize : l.size]
	if l.version == versionV3 {
		var keyUUID [keyUUIDSize]byte
		copy(keyUUID[:], head[len(magicV3):len(magicV3)+keyUUIDSize])
		if f.resolver == nil {
			_ = rc.Close()
			return nil, fmt.Errorf("encrypt: %s: key uuid %s but no key resolver configured: %w", p, hex.EncodeToString(keyUUID[:]), ErrUnresolvableKey)
		}
		fk, err := f.resolver.Resolve(ctx, keyUUID)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("encrypt: %s: %w", p, err)
		}
		return newReader(rc, dataKey(fk, salt), l.size)
	}
	key, err := f.ringKey(l.keyID, p)
	if err != nil {
		_ = rc.Close()
		return nil, err
	}
	return newReader(rc, dataKey(key, salt), l.size)
}

// Create seals all content written through the returned WriteCloser. The
// size hint is dropped: the stored size differs from the plaintext size by
// the header plus one GCM tag per chunk, and backends treat it as a
// preallocation hint only. With a KeyResolver configured, Create seals with
// the v3 envelope: Allocate supplies a fresh wrapped file key whose UUID
// goes into the header, and the writer exposes it via
// storage.KeyUUIDWriter (ADR-0097). Without a resolver, single-key rings
// write the v1 header bit-identically to pre-keyring deployments
// (rollback-safe) and multi-key rings write the v2 header with the current
// key's ID byte (ADR-0074).
func (f *FS) Create(ctx context.Context, p string, _ int64) (io.WriteCloser, error) {
	wc, err := f.inner.Create(ctx, p, 0)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		_ = wc.Close()
		return nil, fmt.Errorf("encrypt: generate salt: %w", err)
	}
	var header []byte
	var key []byte
	var keyUUID [keyUUIDSize]byte
	v3 := f.resolver != nil
	if v3 {
		uuid, fk, err := f.resolver.Allocate(ctx, p)
		if err != nil {
			_ = wc.Close()
			return nil, fmt.Errorf("encrypt: %s: %w", p, err)
		}
		keyUUID = uuid
		header = append(append([]byte{}, []byte(magicV3)...), keyUUID[:]...)
		header = append(header, salt...)
		key = fk
	} else {
		header = append(append([]byte{}, []byte(magic)...), salt...)
		key = f.keys[0]
		if !f.singleKey() {
			//nolint:gosec // G115: the constructor caps the ring at MaxKeys, so the current ID always fits a byte
			header = append(append(append([]byte{}, []byte(magicV2)...), byte(f.currentKeyID())), salt...)
			key = f.keys[f.currentKeyID()]
		}
	}
	if _, err := wc.Write(header); err != nil {
		_ = wc.Close()
		return nil, fmt.Errorf("encrypt: write header: %w", err)
	}
	w, err := newWriter(wc, dataKey(key, salt))
	if err != nil {
		_ = wc.Close()
		return nil, err
	}
	if v3 {
		return &keyedWriter{writer: w, keyUUID: keyUUID, fk: append([]byte(nil), key...)}, nil
	}
	return w, nil
}

// Delete passes through (names and directory structure are not encrypted).
func (f *FS) Delete(ctx context.Context, p string) error {
	return f.inner.Delete(ctx, p)
}

// List reports plaintext sizes for encrypted files.
func (f *FS) List(ctx context.Context, p string) ([]*storage.FileInfo, error) {
	infos, err := f.inner.List(ctx, p)
	if err != nil {
		return nil, err
	}
	for i, info := range infos {
		infos[i], err = f.plainInfo(ctx, info)
		if err != nil {
			return nil, err
		}
	}
	return infos, nil
}

// Rename passes through (names and directory structure are not encrypted).
func (f *FS) Rename(ctx context.Context, src, dst string) error {
	return f.inner.Rename(ctx, src, dst)
}

// Mkdir passes through (names and directory structure are not encrypted).
func (f *FS) Mkdir(ctx context.Context, p string) error {
	return f.inner.Mkdir(ctx, p)
}

// reader is an io.ReadSeekCloser decrypting sealed chunks on demand. The
// last decrypted chunk is cached so sequential reads touch the backend once
// per chunk.
type reader struct {
	inner      io.ReadSeekCloser
	aead       cipher.AEAD
	hdr        int64
	storedSize int64
	plainSize  int64
	pos        int64
	cachedIdx  int64
	cached     []byte
}

func newReader(inner io.ReadSeekCloser, key []byte, hdr int) (*reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	end, err := inner.Seek(0, io.SeekEnd)
	if err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("encrypt: size sealed file: %w", err)
	}
	payload := end - int64(hdr)
	if payload < 0 {
		_ = inner.Close()
		return nil, fmt.Errorf("encrypt: truncated encryption header: %w", ErrIntegrity)
	}
	chunks := (payload + ChunkSize + tagSize - 1) / (ChunkSize + tagSize)
	return &reader{
		inner:      inner,
		aead:       aead,
		hdr:        int64(hdr),
		storedSize: end,
		plainSize:  payload - chunks*tagSize,
		cachedIdx:  -1,
	}, nil
}

func newAEAD(dataKey []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt: init GCM: %w", err)
	}
	return aead, nil
}

// chunk decrypts chunk idx, verifying its GCM tag.
func (r *reader) chunk(idx int64) ([]byte, error) {
	if idx == r.cachedIdx {
		return r.cached, nil
	}
	off := r.hdr + idx*(ChunkSize+tagSize)
	sealedLen := min(int64(ChunkSize+tagSize), r.storedSize-off)
	if sealedLen <= tagSize {
		return nil, fmt.Errorf("encrypt: chunk %d truncated: %w", idx, io.ErrUnexpectedEOF)
	}
	sealed := make([]byte, sealedLen)
	if _, err := r.inner.Seek(off, io.SeekStart); err != nil {
		return nil, fmt.Errorf("encrypt: seek chunk %d: %w", idx, err)
	}
	if _, err := io.ReadFull(r.inner, sealed); err != nil {
		return nil, fmt.Errorf("encrypt: read chunk %d: %w", idx, err)
	}
	var n [nonceSize]byte
	nonce(n[:], uint64(idx)) //nolint:gosec // G115: idx derives from pos, which Seek/Read keep non-negative
	plain, err := r.aead.Open(nil, n[:], sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("encrypt: chunk %d: %w", idx, ErrIntegrity)
	}
	r.cachedIdx = idx
	r.cached = plain
	return plain, nil
}

func (r *reader) Read(p []byte) (int, error) {
	if r.pos >= r.plainSize {
		return 0, io.EOF
	}
	total := 0
	for len(p) > 0 && r.pos < r.plainSize {
		idx := r.pos / ChunkSize
		plain, err := r.chunk(idx)
		if err != nil {
			return total, err
		}
		n := copy(p, plain[r.pos%ChunkSize:])
		p = p[n:]
		r.pos += int64(n)
		total += n
	}
	return total, nil
}

func (r *reader) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = r.pos
	case io.SeekEnd:
		base = r.plainSize
	default:
		return 0, fmt.Errorf("encrypt: invalid seek whence %d", whence)
	}
	pos := base + offset
	if pos < 0 {
		return 0, fmt.Errorf("encrypt: negative seek position %d", pos)
	}
	r.pos = pos
	return pos, nil
}

func (r *reader) Close() error {
	return r.inner.Close()
}

// writer buffers one plaintext chunk, sealing and flushing it when full;
// Close seals the tail.
type writer struct {
	inner io.WriteCloser
	aead  cipher.AEAD
	buf   []byte
	idx   uint64
}

func newWriter(inner io.WriteCloser, dataKey []byte) (*writer, error) {
	aead, err := newAEAD(dataKey)
	if err != nil {
		return nil, err
	}
	return &writer{inner: inner, aead: aead}, nil
}

func (w *writer) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(ChunkSize-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		total += n
		if len(w.buf) == ChunkSize {
			if err := w.flush(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// flush seals the buffered chunk and writes it to the inner file.
func (w *writer) flush() error {
	var n [nonceSize]byte
	nonce(n[:], w.idx)
	sealed := w.aead.Seal(nil, n[:], w.buf, nil)
	if _, err := w.inner.Write(sealed); err != nil {
		return fmt.Errorf("encrypt: write chunk %d: %w", w.idx, err)
	}
	w.idx++
	w.buf = w.buf[:0]
	return nil
}

// Close seals any buffered tail chunk (an empty file stores the header
// only) and closes the inner writer.
func (w *writer) Close() error {
	var err error
	if len(w.buf) > 0 {
		err = w.flush()
	}
	return errors.Join(err, w.inner.Close())
}

// keyedWriter wraps a v3 writer with the key UUID its header carries, so
// callers persisting file metadata can record it (storage.KeyUUIDWriter),
// and a copy of the plaintext file key it minted (storage.FileKeyWriter), so
// the write path can thread it to the key-share hooks (ADR-0101). v1/v2
// writers implement neither interface.
type keyedWriter struct {
	*writer
	keyUUID [keyUUIDSize]byte
	fk      []byte
}

// SealedKeyUUID implements storage.KeyUUIDWriter.
func (w *keyedWriter) SealedKeyUUID() ([16]byte, bool) {
	return w.keyUUID, true
}

// PlainFileKey implements storage.FileKeyWriter. The copy stays live for
// the writer's lifetime: callers (the file DAV) read it after Close, once
// the sealed write succeeded.
func (w *keyedWriter) PlainFileKey() ([]byte, bool) {
	return append([]byte(nil), w.fk...), true
}
