// Package encrypt implements transparent at-rest encryption as a
// storage.Storage decorator: AES-256-GCM with 64 KiB chunked framing,
// a per-file random salt, and per-chunk nonces derived from a per-file
// data key (HMAC-SHA256 of the master key over the salt). Files written
// through the decorator are sealed; reads auto-detect the magic header,
// so encrypted files decrypt transparently while legacy plaintext files
// pass through untouched. See ADR-0052 for the threat model and format.
package encrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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

	tagSize    = 16 // AES-GCM authentication tag
	nonceSize  = 12 // AES-GCM nonce
	saltSize   = 32
	headerSize = len(magic) + saltSize
)

// magic marks sealed files; reads auto-detect it to distinguish encrypted
// files from legacy plaintext.
const magic = "NCGOENC1"

// ErrIntegrity reports a failed GCM authentication (tampered ciphertext,
// wrong key, or a corrupted chunk).
var ErrIntegrity = errors.New("encrypt: integrity check failed")

// FS is a storage.Storage decorator sealing file contents at rest.
type FS struct {
	inner     storage.Storage
	masterKey []byte
}

// New wraps inner with transparent encryption. masterKey must be exactly
// MasterKeySize bytes and is copied.
func New(masterKey []byte, inner storage.Storage) (*FS, error) {
	if len(masterKey) != MasterKeySize {
		return nil, fmt.Errorf("encrypt: master key must be %d bytes, got %d", MasterKeySize, len(masterKey))
	}
	if inner == nil {
		return nil, fmt.Errorf("encrypt: inner storage is nil")
	}
	key := make([]byte, MasterKeySize)
	copy(key, masterKey)
	return &FS{inner: inner, masterKey: key}, nil
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

// dataKey derives the per-file data key from the master key and salt.
func (f *FS) dataKey(salt []byte) []byte {
	mac := hmac.New(sha256.New, f.masterKey)
	_, _ = mac.Write(salt) // hash.Hash.Write never errors
	return mac.Sum(nil)
}

func nonce(buf []byte, chunk uint64) {
	clear(buf[:4])
	binary.BigEndian.PutUint64(buf[4:], chunk)
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
func (f *FS) plainInfo(ctx context.Context, info *storage.FileInfo) (*storage.FileInfo, error) {
	if info.IsDir || info.Size < int64(len(magic)) {
		return info, nil
	}
	rc, err := f.inner.Open(ctx, info.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(rc, head); err != nil {
		return nil, fmt.Errorf("encrypt: read header of %s: %w", info.Path, err)
	}
	if string(head) != magic {
		return info, nil
	}
	if info.Size < int64(headerSize) {
		return nil, fmt.Errorf("encrypt: %s: truncated encryption header: %w", info.Path, ErrIntegrity)
	}
	payload := info.Size - int64(headerSize)
	chunks := (payload + ChunkSize + tagSize - 1) / (ChunkSize + tagSize)
	info.Size = payload - chunks*tagSize
	return info, nil
}

// Open decrypts encrypted files chunk-by-chunk and passes legacy plaintext
// files through untouched.
func (f *FS) Open(ctx context.Context, p string) (io.ReadSeekCloser, error) {
	rc, err := f.inner.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	head := make([]byte, headerSize)
	n, readErr := io.ReadFull(rc, head)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		_ = rc.Close()
		return nil, fmt.Errorf("encrypt: read header of %s: %w", p, readErr)
	}
	if n < len(magic) || string(head[:len(magic)]) != magic {
		// Legacy plaintext: rewind and hand the raw handle to the caller.
		if _, err := rc.Seek(0, io.SeekStart); err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("encrypt: rewind %s: %w", p, err)
		}
		return rc, nil
	}
	if n < headerSize {
		_ = rc.Close()
		return nil, fmt.Errorf("encrypt: %s: truncated encryption header: %w", p, ErrIntegrity)
	}
	return newReader(rc, f.dataKey(head[len(magic):headerSize]))
}

// Create seals all content written through the returned WriteCloser. The
// size hint is dropped: the stored size differs from the plaintext size by
// the header plus one GCM tag per chunk, and backends treat it as a
// preallocation hint only.
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
	header := append(append([]byte{}, []byte(magic)...), salt...)
	if _, err := wc.Write(header); err != nil {
		_ = wc.Close()
		return nil, fmt.Errorf("encrypt: write header: %w", err)
	}
	w, err := newWriter(wc, f.dataKey(salt))
	if err != nil {
		_ = wc.Close()
		return nil, err
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
	storedSize int64
	plainSize  int64
	pos        int64
	cachedIdx  int64
	cached     []byte
}

func newReader(inner io.ReadSeekCloser, dataKey []byte) (*reader, error) {
	aead, err := newAEAD(dataKey)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	end, err := inner.Seek(0, io.SeekEnd)
	if err != nil {
		_ = inner.Close()
		return nil, fmt.Errorf("encrypt: size sealed file: %w", err)
	}
	payload := end - int64(headerSize)
	if payload < 0 {
		_ = inner.Close()
		return nil, fmt.Errorf("encrypt: truncated encryption header: %w", ErrIntegrity)
	}
	chunks := (payload + ChunkSize + tagSize - 1) / (ChunkSize + tagSize)
	return &reader{
		inner:      inner,
		aead:       aead,
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
	off := int64(headerSize) + idx*(ChunkSize+tagSize)
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
