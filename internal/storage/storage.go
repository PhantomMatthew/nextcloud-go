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
