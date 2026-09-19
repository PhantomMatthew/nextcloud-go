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
	ErrNotFound         = errors.New("webdav: not found")
	ErrNotDir           = errors.New("webdav: not a directory")
	ErrIsDir            = errors.New("webdav: is a directory")
	ErrForbidden        = errors.New("webdav: forbidden")
	ErrParentMissing    = errors.New("webdav: parent collection missing")
	ErrExists           = errors.New("webdav: target already exists")
	ErrLocked           = errors.New("webdav: locked")
	ErrBadRequest       = errors.New("webdav: bad request")
	ErrPrecondition     = errors.New("webdav: precondition failed")
	ErrConflict         = errors.New("webdav: conflict")
	ErrMethodNotAllowed = errors.New("webdav: method not allowed")
)

type Entry struct {
	Path          string
	IsDir         bool
	Size          int64
	ETag          string
	ModTime       time.Time
	NumericID     uint64
	Permissions   int
	Shareable     bool
	Mounted       bool
	Shared        bool
	ContentType   string
	Checksum      string
	TrashOriginal string
	TrashDeleted  int64
	Favorite      int
	LockToken     string
	LockOwner     string
	LockTimeout   time.Duration
}

type FS interface {
	Stat(ctx context.Context, user, path string) (*Entry, error)
	List(ctx context.Context, user, path string) ([]*Entry, error)
	Read(ctx context.Context, user, path string) (io.ReadCloser, *Entry, error)
	Write(ctx context.Context, user, path string, r io.Reader, mtime *time.Time) (*Entry, bool, error)
	Mkdir(ctx context.Context, user, path string) (*Entry, error)
	Remove(ctx context.Context, user, path string) error
	Move(ctx context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite bool) (*Entry, bool, error)
	Copy(ctx context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite, depthInfinity bool) (*Entry, bool, error)
}

// CollectionMeta is optional MKCOL metadata (chunked upload v2).
type CollectionMeta struct {
	Destination string
	TotalLength int64
}

// MetaMkdirFS is implemented by filesystems that accept MKCOL headers.
type MetaMkdirFS interface {
	MkdirMeta(ctx context.Context, user, path string, meta CollectionMeta) (*Entry, error)
}

// PropPatchOp is one set or remove in a PROPPATCH body.
type PropPatchOp struct {
	Remove bool
	Space  string
	Name   string
	Value  string
}

// PropPatchResult is the per-property status for a 207 Multi-Status.
type PropPatchResult struct {
	Space  string
	Name   string
	Status int
}

// PropPatchFS is implemented by filesystems that persist PROPPATCH properties.
type PropPatchFS interface {
	PatchProps(ctx context.Context, user, path string, ops []PropPatchOp) ([]PropPatchResult, error)
}

// LockRequest is an exclusive write LOCK or refresh.
type LockRequest struct {
	Refresh bool
	Owner   string
	Timeout time.Duration
	Token   string
}

// LockInfo is the active exclusive write lock.
type LockInfo struct {
	Token   string
	Owner   string
	Timeout time.Duration
	Path    string
}

// LockFS is implemented by filesystems that support RFC 4918 LOCK/UNLOCK.
type LockFS interface {
	Lock(ctx context.Context, user, path string, req LockRequest) (*LockInfo, error)
	Unlock(ctx context.Context, user, path, token string) error
	CheckLock(ctx context.Context, user, path, ifHeader string) error
}

type InMemoryFS struct {
	mu     sync.RWMutex
	users  map[string]*userTree
	clock  func() time.Time
	nextID uint64
	idMu   sync.Mutex
	locks  map[string]map[string]*memLock
}

type memLock struct {
	Token   string
	Owner   string
	Timeout time.Time
	Created time.Time
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
		locks:  make(map[string]map[string]*memLock),
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
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if np == "/" {
		fs.applyMemLockLocked(user, "/", t.root)
		return t.root, nil
	}
	if f, ok := t.files[np]; ok {
		fs.applyMemLockLocked(user, np, f.entry)
		return f.entry, nil
	}
	return nil, ErrNotFound
}

func (fs *InMemoryFS) List(_ context.Context, user, p string) ([]*Entry, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if np != "/" {
		f, ok := t.files[np]
		if !ok {
			return nil, ErrNotFound
		}
		if !f.entry.IsDir {
			return nil, ErrNotDir
		}
	}
	out := make([]*Entry, 0, len(t.files))
	for fp, f := range t.files {
		if path.Dir(fp) == np {
			fs.applyMemLockLocked(user, fp, f.entry)
			out = append(out, f.entry)
		}
	}
	return out, nil
}

func (fs *InMemoryFS) Mkdir(_ context.Context, user, p string) (*Entry, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np == "/" {
		return nil, ErrExists
	}
	parent := path.Dir(np)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, exists := t.files[np]; exists {
		return nil, ErrExists
	}
	if parent != "/" {
		pf, ok := t.files[parent]
		if !ok {
			return nil, ErrParentMissing
		}
		if !pf.entry.IsDir {
			return nil, ErrNotDir
		}
	}
	id := fs.allocID()
	mt := fs.clock().UTC()
	entry := &Entry{
		Path:        np,
		IsDir:       true,
		ModTime:     mt,
		NumericID:   id,
		Permissions: PermAll,
		Shareable:   true,
		ContentType: "httpd/unix-directory",
	}
	entry.ETag = ComputeETag(0, entry.ModTime, entry.Path)
	t.files[np] = &fileNode{entry: entry}
	return entry, nil
}

func (fs *InMemoryFS) Remove(_ context.Context, user, p string) error {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	if np == "/" {
		return ErrForbidden
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	target, ok := t.files[np]
	if !ok {
		return ErrNotFound
	}
	if target.entry.IsDir {
		prefix := np + "/"
		for fp := range t.files {
			if strings.HasPrefix(fp, prefix) {
				delete(t.files, fp)
			}
		}
	}
	delete(t.files, np)
	fs.deleteMemLocksLocked(user, np)
	return nil
}

func (fs *InMemoryFS) Move(_ context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite bool) (*Entry, bool, error) {
	if srcUser != dstUser {
		return nil, false, ErrForbidden
	}
	t := fs.ensureUser(srcUser)
	src := normalizePath(srcPath)
	dst := normalizePath(dstPath)
	if src == "/" || dst == "/" {
		return nil, false, ErrForbidden
	}
	if src == dst {
		return nil, false, ErrForbidden
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcNode, ok := t.files[src]
	if !ok {
		return nil, false, ErrNotFound
	}
	if srcNode.entry.IsDir && strings.HasPrefix(dst+"/", src+"/") {
		return nil, false, ErrForbidden
	}

	dstParent := path.Dir(dst)
	if dstParent != "/" {
		pf, ok := t.files[dstParent]
		if !ok {
			return nil, false, ErrParentMissing
		}
		if !pf.entry.IsDir {
			return nil, false, ErrNotDir
		}
	}

	created := true
	if existing, exists := t.files[dst]; exists {
		if !overwrite {
			return nil, false, ErrExists
		}
		if existing.entry.IsDir {
			prefix := dst + "/"
			for fp := range t.files {
				if strings.HasPrefix(fp, prefix) {
					delete(t.files, fp)
				}
			}
		}
		delete(t.files, dst)
		created = false
	}

	mt := fs.clock().UTC()
	moveOne := func(oldPath, newPath string) {
		node := t.files[oldPath]
		delete(t.files, oldPath)
		node.entry.Path = newPath
		node.entry.ModTime = mt
		size := node.entry.Size
		node.entry.ETag = ComputeETag(size, mt, newPath)
		t.files[newPath] = node
	}

	if srcNode.entry.IsDir {
		oldPrefix := src + "/"
		newPrefix := dst + "/"
		var children []string
		for fp := range t.files {
			if strings.HasPrefix(fp, oldPrefix) {
				children = append(children, fp)
			}
		}
		moveOne(src, dst)
		for _, oldChild := range children {
			newChild := newPrefix + strings.TrimPrefix(oldChild, oldPrefix)
			moveOne(oldChild, newChild)
		}
	} else {
		moveOne(src, dst)
	}
	fs.renameMemLocksLocked(srcUser, src, dst)
	return t.files[dst].entry, created, nil
}

func (fs *InMemoryFS) Copy(_ context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite, depthInfinity bool) (*Entry, bool, error) {
	if srcUser != dstUser {
		return nil, false, ErrForbidden
	}
	t := fs.ensureUser(srcUser)
	src := normalizePath(srcPath)
	dst := normalizePath(dstPath)
	if src == "/" || dst == "/" {
		return nil, false, ErrForbidden
	}
	if src == dst {
		return nil, false, ErrForbidden
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	srcNode, ok := t.files[src]
	if !ok {
		return nil, false, ErrNotFound
	}
	if srcNode.entry.IsDir && strings.HasPrefix(dst+"/", src+"/") {
		return nil, false, ErrForbidden
	}

	dstParent := path.Dir(dst)
	if dstParent != "/" {
		pf, ok := t.files[dstParent]
		if !ok {
			return nil, false, ErrParentMissing
		}
		if !pf.entry.IsDir {
			return nil, false, ErrNotDir
		}
	}

	created := true
	if existing, exists := t.files[dst]; exists {
		if !overwrite {
			return nil, false, ErrExists
		}
		if existing.entry.IsDir {
			prefix := dst + "/"
			for fp := range t.files {
				if strings.HasPrefix(fp, prefix) {
					delete(t.files, fp)
				}
			}
		}
		delete(t.files, dst)
		created = false
	}

	mt := fs.clock().UTC()
	cloneOne := func(srcFP, dstFP string) {
		orig := t.files[srcFP]
		newEntry := *orig.entry
		newEntry.Path = dstFP
		newEntry.NumericID = fs.allocID()
		newEntry.ModTime = mt
		newEntry.ETag = ComputeETag(newEntry.Size, mt, dstFP)
		var data []byte
		if orig.data != nil {
			data = make([]byte, len(orig.data))
			copy(data, orig.data)
		}
		t.files[dstFP] = &fileNode{entry: &newEntry, data: data}
	}

	if srcNode.entry.IsDir {
		cloneOne(src, dst)
		if depthInfinity {
			oldPrefix := src + "/"
			newPrefix := dst + "/"
			var children []string
			for fp := range t.files {
				if strings.HasPrefix(fp, oldPrefix) {
					children = append(children, fp)
				}
			}
			for _, oldChild := range children {
				newChild := newPrefix + strings.TrimPrefix(oldChild, oldPrefix)
				cloneOne(oldChild, newChild)
			}
		}
	} else {
		cloneOne(src, dst)
	}
	return t.files[dst].entry, created, nil
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
	if existed {
		entry.Favorite = existing.entry.Favorite
	}
	t.files[np] = &fileNode{entry: entry, data: data}
	return entry, !existed, nil
}

func (fs *InMemoryFS) PatchProps(_ context.Context, user, p string, ops []PropPatchOp) ([]PropPatchResult, error) {
	t := fs.ensureUser(user)
	np := normalizePath(p)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var entry *Entry
	if np == "/" {
		entry = t.root
	} else {
		f, ok := t.files[np]
		if !ok {
			return nil, ErrNotFound
		}
		entry = f.entry
	}
	out := make([]PropPatchResult, 0, len(ops))
	for _, op := range ops {
		res := PropPatchResult{Space: op.Space, Name: op.Name, Status: 403}
		if op.Name != "favorite" || (op.Space != "" && op.Space != "http://owncloud.org/ns") {
			out = append(out, res)
			continue
		}
		if op.Remove {
			entry.Favorite = 0
			res.Status = 200
			out = append(out, res)
			continue
		}
		val := strings.TrimSpace(op.Value)
		if val != "0" && val != "1" {
			out = append(out, res)
			continue
		}
		if val == "1" {
			entry.Favorite = 1
		} else {
			entry.Favorite = 0
		}
		res.Status = 200
		out = append(out, res)
	}
	return out, nil
}
