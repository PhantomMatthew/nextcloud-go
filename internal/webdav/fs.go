package webdav

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("webdav: not found")
	ErrNotDir    = errors.New("webdav: not a directory")
	ErrForbidden = errors.New("webdav: forbidden")
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
}

type InMemoryFS struct {
	mu      sync.RWMutex
	users   map[string]*userTree
	clock   func() time.Time
	nextID  uint64
	idMu    sync.Mutex
}

type userTree struct {
	root *Entry
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

func (fs *InMemoryFS) Stat(_ context.Context, user, path string) (*Entry, error) {
	t := fs.ensureUser(user)
	p := normalizePath(path)
	if p == "/" {
		return t.root, nil
	}
	return nil, ErrNotFound
}

func (fs *InMemoryFS) List(_ context.Context, user, path string) ([]*Entry, error) {
	t := fs.ensureUser(user)
	p := normalizePath(path)
	if p != "/" {
		return nil, ErrNotFound
	}
	if !t.root.IsDir {
		return nil, ErrNotDir
	}
	return nil, nil
}
