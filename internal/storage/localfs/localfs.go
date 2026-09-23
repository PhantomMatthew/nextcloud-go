// Package localfs implements storage.Storage on a jailed local directory.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// FS is a local-directory backend rooted at root.
type FS struct {
	root string
}

// New returns an FS jailed to root, creating the directory if needed.
func New(root string) (*FS, error) {
	if root == "" {
		return nil, fmt.Errorf("localfs: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("localfs: abs: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("localfs: mkdir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("localfs: resolve root: %w", err)
	}
	return &FS{root: resolved}, nil
}

func (f *FS) resolve(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return "", storage.ErrInvalidPath
	}
	slash := filepath.ToSlash(p)
	if strings.HasPrefix(slash, "/") {
		return "", storage.ErrInvalidPath
	}
	for _, seg := range strings.Split(slash, "/") {
		if seg == ".." {
			return "", storage.ErrInvalidPath
		}
	}
	full := filepath.Clean(filepath.Join(f.root, filepath.FromSlash(slash)))
	rel, err := filepath.Rel(f.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", storage.ErrInvalidPath
	}
	return full, nil
}

func (f *FS) confined(path string) error {
	eval, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			parent := filepath.Dir(path)
			if parent == path {
				return storage.ErrInvalidPath
			}
			return f.confined(parent)
		}
		return storage.ErrInvalidPath
	}
	rel, err := filepath.Rel(f.root, eval)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return storage.ErrInvalidPath
	}
	return nil
}

func (f *FS) Stat(_ context.Context, p string) (*storage.FileInfo, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	if err := f.confined(full); err != nil && !errors.Is(err, storage.ErrInvalidPath) {
		return nil, err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return nil, mapExistErr(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if err := f.confined(full); err != nil {
			return nil, err
		}
		info, err = os.Stat(full)
		if err != nil {
			return nil, mapExistErr(err)
		}
	}
	return fileInfo(p, info), nil
}

func (f *FS) Open(_ context.Context, p string) (io.ReadSeekCloser, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	if err := f.confined(full); err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, mapExistErr(err)
	}
	if info.IsDir() {
		return nil, storage.ErrIsDir
	}
	fh, err := os.Open(full)
	if err != nil {
		return nil, mapExistErr(err)
	}
	return fh, nil
}

func (f *FS) Create(_ context.Context, p string, _ int64) (io.WriteCloser, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(full)
	if err := f.confined(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("localfs: create dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".ncgo-tmp-*")
	if err != nil {
		return nil, fmt.Errorf("localfs: temp: %w", err)
	}
	return &atomicFile{tmp: tmp, dest: full}, nil
}

func (f *FS) Delete(_ context.Context, p string) error {
	full, err := f.resolve(p)
	if err != nil {
		return err
	}
	if err := f.confined(full); err != nil {
		return err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return mapExistErr(err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(full)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return storage.ErrNotEmpty
		}
	}
	if err := os.Remove(full); err != nil {
		return mapExistErr(err)
	}
	return nil
}

func (f *FS) List(_ context.Context, p string) ([]*storage.FileInfo, error) {
	full, err := f.resolve(p)
	if err != nil {
		return nil, err
	}
	if err := f.confined(full); err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, mapExistErr(err)
	}
	if !info.IsDir() {
		return nil, storage.ErrNotDir
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	out := make([]*storage.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		child := filepath.ToSlash(filepath.Join(p, e.Name()))
		out = append(out, fileInfo(child, fi))
	}
	return out, nil
}

func (f *FS) Rename(_ context.Context, src, dst string) error {
	from, err := f.resolve(src)
	if err != nil {
		return err
	}
	to, err := f.resolve(dst)
	if err != nil {
		return err
	}
	if err := f.confined(from); err != nil {
		return err
	}
	if err := f.confined(filepath.Dir(to)); err != nil {
		return err
	}
	if _, err := os.Lstat(from); err != nil {
		return mapExistErr(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func (f *FS) Mkdir(_ context.Context, p string) error {
	full, err := f.resolve(p)
	if err != nil {
		return err
	}
	if err := f.confined(filepath.Dir(full)); err != nil {
		return err
	}
	if err := os.Mkdir(full, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return storage.ErrExists
		}
		// A missing parent reports the sentinel like Stat/Open do, so
		// callers (DAV, plugin ABI) can map it instead of seeing a raw
		// *PathError.
		return mapExistErr(err)
	}
	return nil
}

func fileInfo(p string, info os.FileInfo) *storage.FileInfo {
	return &storage.FileInfo{
		Path:    filepath.ToSlash(p),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
		IsDir:   info.IsDir(),
	}
}

func mapExistErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return storage.ErrNotFound
	}
	return err
}

type atomicFile struct {
	tmp  *os.File
	dest string
}

func (a *atomicFile) Write(p []byte) (int, error) {
	return a.tmp.Write(p)
}

func (a *atomicFile) Close() error {
	name := a.tmp.Name()
	if err := a.tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, a.dest); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
