package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// DAV implements webdav.FS on storage.Storage plus the filecache Store.
type DAV struct {
	Storage  storage.Storage
	Meta     Store
	Users    users.Store
	Clock    func() time.Time
	Trash    *Trash
	Versions *Versions
	Props    PropertyStore
}

// NewDAV returns a DAV adapter.
func NewDAV(st storage.Storage, meta Store, u users.Store) *DAV {
	return &DAV{Storage: st, Meta: meta, Users: u, Clock: time.Now}
}

func (d *DAV) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

func (d *DAV) resolveUser(ctx context.Context, uid string) (*users.User, error) {
	if uid == "" || strings.Contains(uid, "/") || uid == ".." {
		return nil, webdav.ErrForbidden
	}
	u, err := d.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, webdav.ErrForbidden
		}
		return nil, err
	}
	if _, err := d.Meta.EnsureRoot(ctx, u.ID); err != nil {
		return nil, err
	}
	if err := d.ensureHome(ctx, uid); err != nil {
		return nil, err
	}
	return u, nil
}

func (d *DAV) ensureHome(ctx context.Context, uid string) error {
	_, err := d.Storage.Stat(ctx, uid)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return mapStorage(err)
	}
	if err := d.Storage.Mkdir(ctx, uid); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	return nil
}

func storageKey(uid, rel string) (string, error) {
	np, err := NormalizePath(rel)
	if err != nil {
		return "", webdav.ErrForbidden
	}
	rest := strings.TrimPrefix(np, "/")
	if rest == "" {
		return uid, nil
	}
	return uid + "/" + rest, nil
}

func (d *DAV) toEntry(ctx context.Context, userID int64, f *File) *webdav.Entry {
	var id uint64
	if f.ID > 0 {
		id = uint64(f.ID)
	}
	return &webdav.Entry{
		Path:        f.Path,
		IsDir:       f.IsDir,
		Size:        f.Size,
		ETag:        f.ETag,
		ModTime:     f.Mtime,
		NumericID:   id,
		Permissions: f.Permissions,
		Shareable:   true,
		ContentType: f.MIME,
		Checksum:    f.Checksum,
		Favorite:    d.favoriteValue(ctx, userID, f.Path),
	}
}

func (d *DAV) favoriteValue(ctx context.Context, userID int64, p string) int {
	if d == nil || d.Props == nil {
		return 0
	}
	prop, err := d.Props.Get(ctx, userID, p, PropNSOwnCloud, PropFavorite)
	if err != nil || prop == nil || prop.Value != "1" {
		return 0
	}
	return 1
}

func davPropSpace(space string) bool {
	s := strings.TrimRight(space, "/")
	return s == "DAV:" || s == "DAV"
}

func isProtectedLiveProp(space, name string) bool {
	if !davPropSpace(space) && space != "" {
		return false
	}
	switch name {
	case "getetag", "resourcetype", "getcontentlength", "getcontenttype", "quota-used-bytes", "quota-available-bytes":
		return true
	}
	return false
}

func isFavoriteProp(space, name string) bool {
	if name != PropFavorite {
		return false
	}
	return space == "" || space == PropNSOwnCloud
}

// PatchProps applies PROPPATCH operations. Only oc:favorite is persisted.
func (d *DAV) PatchProps(ctx context.Context, user, p string, ops []webdav.PropPatchOp) ([]webdav.PropPatchResult, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return nil, mapMeta(err)
	}
	if _, err := d.Meta.GetByPath(ctx, u.ID, np); err != nil {
		return nil, mapMeta(err)
	}
	out := make([]webdav.PropPatchResult, 0, len(ops))
	for _, op := range ops {
		res := webdav.PropPatchResult{Space: op.Space, Name: op.Name, Status: http.StatusForbidden}
		if isProtectedLiveProp(op.Space, op.Name) || !isFavoriteProp(op.Space, op.Name) || d.Props == nil {
			out = append(out, res)
			continue
		}
		if op.Remove {
			if err := d.Props.Remove(ctx, u.ID, np, PropNSOwnCloud, PropFavorite); err != nil {
				return nil, err
			}
			res.Status = http.StatusOK
			out = append(out, res)
			continue
		}
		val := strings.TrimSpace(op.Value)
		if val != "0" && val != "1" {
			out = append(out, res)
			continue
		}
		if err := d.Props.Set(ctx, &FileProperty{
			UserID: u.ID,
			Path:   np,
			NS:     PropNSOwnCloud,
			Name:   PropFavorite,
			Value:  val,
		}); err != nil {
			return nil, err
		}
		res.Status = http.StatusOK
		out = append(out, res)
	}
	return out, nil
}

func (d *DAV) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	f, err := d.Meta.GetByPath(ctx, u.ID, p)
	if err != nil {
		return nil, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, f), nil
}

func (d *DAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	dir, err := d.Meta.GetByPath(ctx, u.ID, p)
	if err != nil {
		return nil, mapMeta(err)
	}
	if !dir.IsDir {
		return nil, webdav.ErrNotDir
	}
	children, err := d.Meta.ListChildren(ctx, u.ID, dir.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*webdav.Entry, 0, len(children))
	for i := range children {
		out = append(out, d.toEntry(ctx, u.ID, &children[i]))
	}
	return out, nil
}

func (d *DAV) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	f, err := d.Meta.GetByPath(ctx, u.ID, p)
	if err != nil {
		return nil, nil, mapMeta(err)
	}
	if f.IsDir {
		return nil, nil, webdav.ErrIsDir
	}
	key, err := storageKey(user, p)
	if err != nil {
		return nil, nil, err
	}
	rc, err := d.Storage.Open(ctx, key)
	if err != nil {
		return nil, nil, mapStorage(err)
	}
	return rc, d.toEntry(ctx, u.ID, f), nil
}

func (d *DAV) Write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time) (*webdav.Entry, bool, error) {
	return d.write(ctx, user, p, r, mtime, true)
}

func (d *DAV) write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, snapshot bool) (*webdav.Entry, bool, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	if np == "/" {
		return nil, false, webdav.ErrIsDir
	}
	parentPath := path.Dir(np)
	if parentPath == "." {
		parentPath = "/"
	}
	parent, err := d.Meta.GetByPath(ctx, u.ID, parentPath)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	if !parent.IsDir {
		return nil, false, webdav.ErrNotDir
	}

	existing, err := d.Meta.GetByPath(ctx, u.ID, np)
	created := false
	switch {
	case errors.Is(err, ErrNotFound):
		created = true
	case err != nil:
		return nil, false, mapMeta(err)
	case existing.IsDir:
		return nil, false, webdav.ErrIsDir
	}

	if !created && snapshot && d.Versions != nil {
		if err := d.Versions.Snapshot(ctx, user, existing); err != nil {
			return nil, false, err
		}
	}

	key, err := storageKey(user, np)
	if err != nil {
		return nil, false, err
	}
	wc, err := d.Storage.Create(ctx, key, 0)
	if err != nil {
		return nil, false, mapStorage(err)
	}
	sum := sha256.New()
	n, copyErr := io.Copy(wc, io.TeeReader(r, sum))
	closeErr := wc.Close()
	if copyErr != nil || closeErr != nil {
		cause := copyErr
		if cause == nil {
			cause = closeErr
		}
		return nil, false, d.compensateDelete(ctx, key, cause)
	}
	mt := d.now()
	if mtime != nil {
		mt = mtime.UTC()
	}
	checksum := "SHA256:" + hex.EncodeToString(sum.Sum(nil))
	var f *File
	if created {
		f = &File{
			UserID:      u.ID,
			Path:        np,
			IsDir:       false,
			Size:        n,
			Mtime:       mt,
			Checksum:    checksum,
			MIME:        "application/octet-stream",
			Permissions: webdav.PermAll,
		}
		if err := d.Meta.Insert(ctx, f); err != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(err))
		}
	} else {
		existing.Size = n
		existing.Mtime = mt
		existing.Checksum = checksum
		existing.ETag = ComputeFileETag(existing.ID, mt, n)
		if err := d.Meta.UpdateMeta(ctx, existing); err != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(err))
		}
		f = existing
	}
	if err := d.Meta.RecalcAncestors(ctx, u.ID, f.ParentID, mt); err != nil {
		return nil, false, err
	}
	got, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, got), created, nil
}

func (d *DAV) Mkdir(ctx context.Context, user, p string) (*webdav.Entry, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return nil, mapMeta(err)
	}
	if np == "/" {
		return nil, webdav.ErrExists
	}
	key, err := storageKey(user, np)
	if err != nil {
		return nil, err
	}
	if err := d.Storage.Mkdir(ctx, key); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, mapStorage(err)
	}
	f := &File{
		UserID:      u.ID,
		Path:        np,
		IsDir:       true,
		Mtime:       d.now(),
		MIME:        "httpd/unix-directory",
		Permissions: webdav.PermAll,
	}
	if err := d.Meta.Insert(ctx, f); err != nil {
		return nil, d.compensateDelete(ctx, key, mapMeta(err))
	}
	if err := d.Meta.RecalcAncestors(ctx, u.ID, f.ParentID, d.now()); err != nil {
		return nil, err
	}
	got, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err != nil {
		return nil, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, got), nil
}

func (d *DAV) Remove(ctx context.Context, user, p string) error {
	if d.Trash != nil {
		return d.Trash.MoveToTrash(ctx, user, p, user)
	}
	return d.Purge(ctx, user, p)
}

// Purge permanently deletes path from storage and filecache.
func (d *DAV) Purge(ctx context.Context, user, p string) error {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	f, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err != nil {
		return mapMeta(err)
	}
	parent := f.ParentID
	nodes := []File{*f}
	if f.IsDir {
		desc, err := d.collect(ctx, u.ID, f.ID)
		if err != nil {
			return err
		}
		nodes = append(nodes, desc...)
	}
	sort.Slice(nodes, func(i, j int) bool { return len(nodes[i].Path) > len(nodes[j].Path) })
	for _, n := range nodes {
		key, err := storageKey(user, n.Path)
		if err != nil {
			return err
		}
		if err := d.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrNotEmpty) {
			return mapStorage(err)
		}
	}
	if err := d.Meta.DeleteSubtree(ctx, u.ID, np); err != nil {
		return mapMeta(err)
	}
	if d.Versions != nil {
		if err := d.Versions.DeleteByPath(ctx, user, np); err != nil {
			return err
		}
	}
	if d.Props != nil {
		if err := d.Props.DeleteByPath(ctx, u.ID, np); err != nil {
			return err
		}
	}
	return d.Meta.RecalcAncestors(ctx, u.ID, parent, d.now())
}

func (d *DAV) collect(ctx context.Context, userID, parentID int64) ([]File, error) {
	children, err := d.Meta.ListChildren(ctx, userID, parentID)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, c := range children {
		out = append(out, c)
		if c.IsDir {
			more, err := d.collect(ctx, userID, c.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, more...)
		}
	}
	return out, nil
}

func (d *DAV) Move(ctx context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite bool) (*webdav.Entry, bool, error) {
	if srcUser != dstUser {
		return nil, false, webdav.ErrForbidden
	}
	u, err := d.resolveUser(ctx, srcUser)
	if err != nil {
		return nil, false, err
	}
	src, err := NormalizePath(srcPath)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	dst, err := NormalizePath(dstPath)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	srcFile, err := d.Meta.GetByPath(ctx, u.ID, src)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	oldParent := srcFile.ParentID
	_, dstErr := d.Meta.GetByPath(ctx, u.ID, dst)
	created := errors.Is(dstErr, ErrNotFound)
	if dstErr != nil && !created {
		return nil, false, mapMeta(dstErr)
	}
	if !created {
		if !overwrite {
			return nil, false, webdav.ErrExists
		}
		if err := d.Purge(ctx, srcUser, dst); err != nil {
			return nil, false, err
		}
		created = false
	}
	from, err := storageKey(srcUser, src)
	if err != nil {
		return nil, false, err
	}
	to, err := storageKey(srcUser, dst)
	if err != nil {
		return nil, false, err
	}
	if err := d.Storage.Rename(ctx, from, to); err != nil {
		return nil, false, mapStorage(err)
	}
	if err := d.Meta.RenameSubtree(ctx, u.ID, src, dst, d.now()); err != nil {
		if rb := d.Storage.Rename(ctx, to, from); rb != nil && !errors.Is(rb, storage.ErrNotFound) {
			return nil, false, errors.Join(mapMeta(err), rb)
		}
		return nil, false, mapMeta(err)
	}
	if d.Versions != nil {
		if err := d.Versions.RenamePath(ctx, srcUser, src, dst); err != nil {
			return nil, false, err
		}
	}
	if d.Props != nil {
		if err := d.Props.RenamePath(ctx, u.ID, src, dst); err != nil {
			return nil, false, err
		}
	}
	now := d.now()
	moved, err := d.Meta.GetByPath(ctx, u.ID, dst)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	if err := d.Meta.RecalcAncestors(ctx, u.ID, oldParent, now); err != nil {
		return nil, false, err
	}
	if err := d.Meta.RecalcAncestors(ctx, u.ID, moved.ParentID, now); err != nil {
		return nil, false, err
	}
	got, err := d.Meta.GetByPath(ctx, u.ID, dst)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, got), created, nil
}

func (d *DAV) Copy(ctx context.Context, srcUser, srcPath, dstUser, dstPath string, overwrite, depthInfinity bool) (*webdav.Entry, bool, error) {
	if srcUser != dstUser {
		return nil, false, webdav.ErrForbidden
	}
	srcEntry, err := d.Stat(ctx, srcUser, srcPath)
	if err != nil {
		return nil, false, err
	}
	_, dstErr := d.Stat(ctx, srcUser, dstPath)
	created := errors.Is(dstErr, webdav.ErrNotFound)
	if dstErr != nil && !created {
		return nil, false, dstErr
	}
	if !created {
		if !overwrite {
			return nil, false, webdav.ErrExists
		}
		if err := d.Purge(ctx, srcUser, dstPath); err != nil {
			return nil, false, err
		}
	}
	if err := d.copyOne(ctx, srcUser, srcPath, dstPath, srcEntry.IsDir, depthInfinity); err != nil {
		return nil, false, err
	}
	got, err := d.Stat(ctx, srcUser, dstPath)
	if err != nil {
		return nil, false, err
	}
	return got, created, nil
}

func (d *DAV) copyOne(ctx context.Context, uid, src, dst string, isDir, depthInfinity bool) error {
	if isDir {
		if _, err := d.Mkdir(ctx, uid, dst); err != nil && !errors.Is(err, webdav.ErrExists) {
			return err
		}
		if err := d.copyProps(ctx, uid, src, dst); err != nil {
			return err
		}
		if !depthInfinity {
			return nil
		}
		children, err := d.List(ctx, uid, src)
		if err != nil {
			return err
		}
		for _, c := range children {
			name := path.Base(c.Path)
			if err := d.copyOne(ctx, uid, c.Path, path.Join(dst, name), c.IsDir, true); err != nil {
				return err
			}
		}
		return nil
	}
	rc, _, err := d.Read(ctx, uid, src)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _, err = d.Write(ctx, uid, dst, rc, nil)
	if err != nil {
		return err
	}
	return d.copyProps(ctx, uid, src, dst)
}

func (d *DAV) copyProps(ctx context.Context, uid, src, dst string) error {
	if d.Props == nil {
		return nil
	}
	u, err := d.resolveUser(ctx, uid)
	if err != nil {
		return err
	}
	return d.Props.CopyPath(ctx, u.ID, src, dst)
}

func (d *DAV) ingestFromStorage(ctx context.Context, user, p string) error {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	key, err := storageKey(user, np)
	if err != nil {
		return err
	}
	info, err := d.Storage.Stat(ctx, key)
	if err != nil {
		return mapStorage(err)
	}
	mt := d.now()
	if info.IsDir {
		if np != "/" {
			f := &File{
				UserID:      u.ID,
				Path:        np,
				IsDir:       true,
				Mtime:       mt,
				MIME:        "httpd/unix-directory",
				Permissions: webdav.PermAll,
			}
			if err := d.Meta.Insert(ctx, f); err != nil && !errors.Is(err, ErrExists) {
				return mapMeta(err)
			}
		}
		kids, err := d.Storage.List(ctx, key)
		if err != nil {
			return mapStorage(err)
		}
		sort.Slice(kids, func(i, j int) bool { return kids[i].Path < kids[j].Path })
		for _, k := range kids {
			child := path.Join(np, path.Base(k.Path))
			if err := d.ingestFromStorage(ctx, user, child); err != nil {
				return err
			}
		}
		dir, err := d.Meta.GetByPath(ctx, u.ID, np)
		if err != nil {
			return mapMeta(err)
		}
		return d.Meta.RecalcAncestors(ctx, u.ID, dir.ParentID, mt)
	}
	f := &File{
		UserID:      u.ID,
		Path:        np,
		IsDir:       false,
		Size:        info.Size,
		Mtime:       mt,
		MIME:        "application/octet-stream",
		Permissions: webdav.PermAll,
	}
	if err := d.Meta.Insert(ctx, f); err != nil {
		return mapMeta(err)
	}
	return d.Meta.RecalcAncestors(ctx, u.ID, f.ParentID, mt)
}

func (d *DAV) compensateDelete(ctx context.Context, key string, cause error) error {
	if err := d.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return errors.Join(cause, err)
	}
	return cause
}

func mapMeta(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return webdav.ErrNotFound
	case errors.Is(err, ErrExists):
		return webdav.ErrExists
	case errors.Is(err, ErrParentMissing):
		return webdav.ErrParentMissing
	case errors.Is(err, ErrNotDir):
		return webdav.ErrNotDir
	case errors.Is(err, ErrIsDir):
		return webdav.ErrIsDir
	case errors.Is(err, ErrForbidden):
		return webdav.ErrForbidden
	default:
		return err
	}
}

func mapStorage(err error) error {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return webdav.ErrNotFound
	case errors.Is(err, storage.ErrExists):
		return webdav.ErrExists
	case errors.Is(err, storage.ErrNotDir):
		return webdav.ErrNotDir
	case errors.Is(err, storage.ErrIsDir):
		return webdav.ErrIsDir
	case errors.Is(err, storage.ErrInvalidPath):
		return webdav.ErrForbidden
	default:
		return err
	}
}

var _ webdav.FS = (*DAV)(nil)
