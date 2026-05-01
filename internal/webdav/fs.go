package webdav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound       = errors.New("webdav: not found")
	ErrNotDir         = errors.New("webdav: not a directory")
	ErrIsDir          = errors.New("webdav: is a directory")
	ErrForbidden      = errors.New("webdav: forbidden")
	ErrParentMissing  = errors.New("webdav: parent collection missing")
)

type Entry struct {
	Path        string
	IsDir       bool
	Size        int64
	ETag        string
	ModTime     time.Time
	NumericID   uint64
	Permissions int
	Shareable   bool
	Mounted     bool
	Shared      bool
	ContentType string
}

type FS interface {
	Stat(ctx context.Context, user, path string) (*Entry, error)
	List(ctx context.Context, user, path string) ([]*Entry, error)
	Read(ctx context.Context, user, path string) (io.ReadCloser, *Entry, error)
	Write(ctx context.Context, user, path string, r io.Reader, mtime *time.Time) (*Entry, bool, error)
}

type InMemoryFS struct {
	mu     sync.RWMutex
	users  map[string]*userTree
	clock  func() time.Time
	nextID uint64
	idMu   sync.Mutex
}

type fileNode struct {
	entry *Entry
	data  []byte
}

type userTree struct {
	root  *Entry
	files map[string]*fileNode
}

func NewInMemoryFS() *InMemoryFS {
	return &InMemoryFS{
		users:  make(map[string]*userTree),
		clock:  time.Now,
		nextID: 1,
	}
}

func (fs *InMemoryFS) ensureUser(user string) *userTree {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if t, ok := fs.users[user]; ok {
		return t
	}
	id := fs.allocID()
	t := &userTree{
		root: &Entry{
			Path:        "/",
			IsDir:       true,
			ETag:        "00000000000000000000000000000000",
			ModTime:     fs.clock().UTC(),
			NumericID:   id,
			Permissions: PermAll,
			Shareable:   true,
			ContentType: "httpd/unix-directory",
		},
		files: make(map[string]*fileNode),
	}
	fs.users[user] = t
	return t
}

func (fs *InMemoryFS) allocID() uint64 {
	fs.idMu.Lock()
	defer fs.idMu.Unlock()
	id := fs.nextID
	fs.nextID++
	return id
}

func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	return p
}

func (fs *InMemoryFS) Stat(_ context.Context, user, p string) (*Entry, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np == "/" {
		return t.root, nil
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if f, ok := t.files[np]; ok {
		return f.entry, nil
	}
	return nil, ErrNotFound
}

func (fs *InMemoryFS) List(_ context.Context, user, p string) ([]*Entry, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np != "/" {
		return nil, ErrNotFound
	}
	if !t.root.IsDir {
		return nil, ErrNotDir
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	out := make([]*Entry, 0, len(t.files))
	for fp, f := range t.files {
		if path.Dir(fp) == "/" {
			out = append(out, f.entry)
		}
	}
	return out, nil
}

func (fs *InMemoryFS) Read(_ context.Context, user, p string) (io.ReadCloser, *Entry, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np == "/" {
		return nil, nil, ErrIsDir
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	f, ok := t.files[np]
	if !ok {
		return nil, nil, ErrNotFound
	}
	if f.entry.IsDir {
		return nil, nil, ErrIsDir
	}
	return io.NopCloser(bytes.NewReader(f.data)), f.entry, nil
}

func (fs *InMemoryFS) Write(_ context.Context, user, p string, r io.Reader, mtime *time.Time) (*Entry, bool, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np == "/" {
		return nil, false, ErrIsDir
	}
	parent := path.Dir(np)
	if parent != "/" {
		fs.mu.RLock()
		_, parentOK := t.files[parent]
		fs.mu.RUnlock()
		if !parentOK {
			return nil, false, ErrParentMissing
		}
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, false, err
	}

	mt := fs.clock().UTC()
	if mtime != nil {
		mt = mtime.UTC()
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	existing, existed := t.files[np]
	if existed && existing.entry.IsDir {
		return nil, false, ErrIsDir
	}

	var id uint64
	if existed {
		id = existing.entry.NumericID
	} else {
		id = fs.allocID()
	}
	entry := &Entry{
		Path:        np,
		IsDir:       false,
		Size:        int64(len(data)),
		ModTime:     mt,
		NumericID:   id,
		Permissions: PermAll,
		Shareable:   true,
		ContentType: "application/octet-stream",
	}
	entry.ETag = ComputeETag(entry.Size, entry.ModTime, entry.Path)
	t.files[np] = &fileNode{entry: entry, data: data}
	return entry, !existed, nil
}
