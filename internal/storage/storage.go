// Package storage defines backend-agnostic file storage.
package storage

import (
	"context"
	"io"
	"time"
)

// FileInfo describes a stored object or directory.
type FileInfo struct {
	Path    string
	Size    int64
	ModTime time.Time
	IsDir   bool
}

// Storage is a filesystem-like backend.
type Storage interface {
	Stat(ctx context.Context, p string) (*FileInfo, error)
	Open(ctx context.Context, p string) (io.ReadSeekCloser, error)
	Create(ctx context.Context, p string, size int64) (io.WriteCloser, error)
	Delete(ctx context.Context, p string) error
	List(ctx context.Context, p string) ([]*FileInfo, error)
	Rename(ctx context.Context, src, dst string) error
	Mkdir(ctx context.Context, p string) error
}

// KeyUUIDWriter is an optional interface a WriteCloser returned by Create
// may implement when the storage layer seals content under a per-file key
// (the v3 envelope of internal/storage/encrypt, ADR-0097). SealedKeyUUID
// reports the 16-byte key UUID written into the sealed file's header; ok is
// false when the writer does not seal under a per-file key (legacy v1/v2
// writers simply do not implement this interface). Callers that persist
// file metadata assert on it after a successful Close and record the UUID
// so the key can be resolved without parsing sealed headers.
type KeyUUIDWriter interface {
	SealedKeyUUID() (keyUUID [16]byte, ok bool)
}
