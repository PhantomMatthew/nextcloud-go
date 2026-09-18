package files

import (
	"context"
	"errors"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const defaultTrashRetention = 30 * 24 * time.Hour

// Trash implements webdav.FS for /remote.php/dav/trashbin/{user}/.
type Trash struct {
	Storage   storage.Storage
	Sessions  TrashStore
	Files     *DAV
	Users     users.Store
	Clock     func() time.Time
	Retention time.Duration
}

// NewTrash returns a trashbin adapter.
func NewTrash(st storage.Storage, sessions TrashStore, dav *DAV, u users.Store) *Trash {
	return &Trash{
		Storage:   st,
		Sessions:  sessions,
		Files:     dav,
		Users:     u,
		Clock:     time.Now,
		Retention: defaultTrashRetention,
	}
}

func (t *Trash) now() time.Time {
	if t.Clock != nil {
		return t.Clock().UTC()
	}
	return time.Now().UTC()
}

func (t *Trash) retention() time.Duration {
	if t.Retention > 0 {
		return t.Retention
	}
	return defaultTrashRetention
}

func (t *Trash) resolveUser(ctx context.Context, uid string) (*users.User, error) {
	if uid == "" || strings.Contains(uid, "/") || uid == ".." {
		return nil, webdav.ErrForbidden
	}
	user, err := t.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, webdav.ErrForbidden
		}
		return nil, err
	}
	return user, nil
}

func trashStorageKey(uid, loc string) (string, error) {
	if !ValidLocationID(loc) {
		return "", webdav.ErrForbidden
	}
	return "trash/" + uid + "/" + loc, nil
}

func splitTrashPath(p string) (coll, loc string, err error) {
	np, err := NormalizePath(p)
	if err != nil {
		return "", "", webdav.ErrForbidden
	}
	if np == "/" {
		return "", "", nil
	}
	rel := strings.TrimPrefix(np, "/")
	coll, loc, ok := strings.Cut(rel, "/")
	if !ok {
		return coll, "", nil
	}
	if coll != "trash" && coll != "restore" {
		return "", "", webdav.ErrForbidden
	}
	if strings.Contains(loc, "/") {
		return "", "", webdav.ErrForbidden
	}
	return coll, loc, nil
}

func makeLocationID(name string, ts time.Time) string {
	base := path.Base(name)
	if base == "" || base == "." || base == "/" {
		base = "unnamed"
	}
	return base + ".d" + strconv.FormatInt(ts.Unix(), 10)
}

func (t *Trash) uniqueLocationID(ctx context.Context, userID int64, name string, ts time.Time) (string, error) {
	base := makeLocationID(name, ts)
	loc := base
	for i := 0; i < 1000; i++ {
		if i > 0 {
			loc = base + "-" + strconv.Itoa(i)
		}
		_, err := t.Sessions.GetByLocation(ctx, userID, loc)
		if errors.Is(err, ErrNotFound) {
			return loc, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", webdav.ErrExists
}

func (t *Trash) ensureTrashRoot(ctx context.Context, uid string) error {
	if err := t.Storage.Mkdir(ctx, "trash"); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	if err := t.Storage.Mkdir(ctx, "trash/"+uid); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	return nil
}

func (t *Trash) itemEntry(item *TrashItem, p string) *webdav.Entry {
	mt := item.Deleted
	if mt.IsZero() {
		mt = t.now()
	}
	id := uint64(0)
	if item.ID > 0 {
		id = uint64(item.ID)
	}
	ct := "application/octet-stream"
	if item.IsDir {
		ct = "httpd/unix-directory"
	}
	return &webdav.Entry{
		Path:          p,
		IsDir:         item.IsDir,
		Size:          item.Size,
		ModTime:       mt,
		NumericID:     id,
		Permissions:   webdav.PermRead | webdav.PermDelete,
		Shareable:     false,
		ContentType:   ct,
		ETag:          webdav.ComputeETag(item.Size, mt, item.LocationID),
		TrashOriginal: strings.TrimPrefix(item.OriginalPath, "/"),
		TrashDeleted:  item.Deleted.Unix(),
	}
}

func (t *Trash) collEntry(p string, mt time.Time) *webdav.Entry {
	return &webdav.Entry{
		Path:        p,
		IsDir:       true,
		ModTime:     mt,
		Permissions: webdav.PermRead | webdav.PermDelete,
		ContentType: "httpd/unix-directory",
		ETag:        webdav.ComputeETag(0, mt, p),
	}
}

func (t *Trash) expire(ctx context.Context, user string, userID int64) error {
	items, err := t.Sessions.List(ctx, userID)
	if err != nil {
		return mapMeta(err)
	}
	cutoff := t.now().Add(-t.retention())
	for i := range items {
		if items[i].Deleted.After(cutoff) {
			continue
		}
		if err := t.PurgeLocation(ctx, user, items[i].LocationID); err != nil && !errors.Is(err, webdav.ErrNotFound) {
			return err
		}
	}
	return nil
}

// MoveToTrash relocates a files path into the trashbin.
func (t *Trash) MoveToTrash(ctx context.Context, user, p, deletedBy string) error {
	if t.Files == nil {
		return webdav.ErrForbidden
	}
	usr, err := t.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	if np == "/" {
		return webdav.ErrForbidden
	}
	f, err := t.Files.Meta.GetByPath(ctx, usr.ID, np)
	if err != nil {
		return mapMeta(err)
	}
	parent := f.ParentID
	now := t.now()
	loc, err := t.uniqueLocationID(ctx, usr.ID, f.Name, now)
	if err != nil {
		return err
	}
	from, err := storageKey(user, np)
	if err != nil {
		return err
	}
	to, err := trashStorageKey(user, loc)
	if err != nil {
		return err
	}
	if err := t.ensureTrashRoot(ctx, user); err != nil {
		return err
	}
	if err := t.Storage.Rename(ctx, from, to); err != nil {
		return mapStorage(err)
	}
	item := &TrashItem{
		UserID:       usr.ID,
		OriginalPath: np,
		LocationID:   loc,
		Name:         f.Name,
		IsDir:        f.IsDir,
		Size:         f.Size,
		Deleted:      now,
		DeletedBy:    deletedBy,
	}
	if err := t.Sessions.Insert(ctx, item); err != nil {
		if rb := t.Storage.Rename(ctx, to, from); rb != nil && !errors.Is(rb, storage.ErrNotFound) {
			return errors.Join(mapMeta(err), rb)
		}
		return mapMeta(err)
	}
	if err := t.Files.Meta.DeleteSubtree(ctx, usr.ID, np); err != nil {
		return mapMeta(err)
	}
	return t.Files.Meta.RecalcAncestors(ctx, usr.ID, parent, now)
}

// Restore moves a trash item back into files. Empty destPath uses original_path.
func (t *Trash) Restore(ctx context.Context, user, locationID, destUser, destPath string, overwrite bool) (*webdav.Entry, bool, error) {
	if user != destUser {
		return nil, false, webdav.ErrForbidden
	}
	if t.Files == nil {
		return nil, false, webdav.ErrForbidden
	}
	usr, err := t.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	if !ValidLocationID(locationID) {
		return nil, false, webdav.ErrForbidden
	}
	item, err := t.Sessions.GetByLocation(ctx, usr.ID, locationID)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	dest := destPath
	if dest == "" {
		dest = item.OriginalPath
	}
	np, err := NormalizePath(dest)
	if err != nil || np == "/" {
		return nil, false, webdav.ErrBadRequest
	}
	dest = np

	_, statErr := t.Files.Stat(ctx, destUser, dest)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, webdav.ErrNotFound) {
		return nil, false, statErr
	}
	if exists && !overwrite {
		return nil, false, webdav.ErrExists
	}
	if exists {
		if err := t.Files.Purge(ctx, destUser, dest); err != nil {
			return nil, false, err
		}
	}

	parentPath := path.Dir(dest)
	if parentPath == "." {
		parentPath = "/"
	}
	if _, err := t.Files.Stat(ctx, destUser, parentPath); err != nil {
		return nil, false, err
	}

	from, err := trashStorageKey(user, locationID)
	if err != nil {
		return nil, false, err
	}
	to, err := storageKey(user, dest)
	if err != nil {
		return nil, false, err
	}
	if err := t.Storage.Rename(ctx, from, to); err != nil {
		return nil, false, mapStorage(err)
	}
	if err := t.Files.ingestFromStorage(ctx, destUser, dest); err != nil {
		if rb := t.Storage.Rename(ctx, to, from); rb != nil && !errors.Is(rb, storage.ErrNotFound) {
			return nil, false, errors.Join(err, rb)
		}
		return nil, false, err
	}
	if t.Files.Versions != nil && dest != item.OriginalPath {
		if err := t.Files.Versions.RenamePath(ctx, user, item.OriginalPath, dest); err != nil {
			return nil, false, err
		}
	}
	if t.Files.Props != nil && dest != item.OriginalPath {
		if err := t.Files.Props.RenamePath(ctx, usr.ID, item.OriginalPath, dest); err != nil {
			return nil, false, err
		}
	}
	if err := t.Sessions.Delete(ctx, usr.ID, locationID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, false, mapMeta(err)
	}
	got, err := t.Files.Stat(ctx, destUser, dest)
	if err != nil {
		return nil, false, err
	}
	return got, !exists, nil
}

// PurgeLocation permanently deletes a trash item and its bytes.
func (t *Trash) PurgeLocation(ctx context.Context, user, locationID string) error {
	usr, err := t.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	if !ValidLocationID(locationID) {
		return webdav.ErrForbidden
	}
	item, err := t.Sessions.GetByLocation(ctx, usr.ID, locationID)
	if err != nil {
		return mapMeta(err)
	}
	key, err := trashStorageKey(user, locationID)
	if err != nil {
		return err
	}
	if err := t.deleteStorageTree(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return mapStorage(err)
	}
	if err := t.Sessions.Delete(ctx, usr.ID, locationID); err != nil && !errors.Is(err, ErrNotFound) {
		return mapMeta(err)
	}
	if t.Files != nil && t.Files.Versions != nil {
		if err := t.Files.Versions.DeleteByPath(ctx, user, item.OriginalPath); err != nil {
			return err
		}
	}
	if t.Files != nil && t.Files.Props != nil {
		if err := t.Files.Props.DeleteByPath(ctx, usr.ID, item.OriginalPath); err != nil {
			return err
		}
	}
	return nil
}

func (t *Trash) deleteStorageTree(ctx context.Context, key string) error {
	info, err := t.Storage.Stat(ctx, key)
	if err != nil {
		return err
	}
	if info.IsDir {
		kids, err := t.Storage.List(ctx, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		sort.Slice(kids, func(i, j int) bool { return len(kids[i].Path) > len(kids[j].Path) })
		for _, k := range kids {
			if err := t.deleteStorageTree(ctx, k.Path); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
	}
	return t.Storage.Delete(ctx, key)
}

func (t *Trash) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	usr, err := t.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	coll, loc, err := splitTrashPath(p)
	if err != nil {
		return nil, err
	}
	mt := t.now()
	if coll == "" {
		return t.collEntry("/", mt), nil
	}
	if loc == "" {
		if coll != "trash" && coll != "restore" {
			return nil, webdav.ErrForbidden
		}
		return t.collEntry("/", mt), nil
	}
	if err := t.expire(ctx, user, usr.ID); err != nil {
		return nil, err
	}
	item, err := t.Sessions.GetByLocation(ctx, usr.ID, loc)
	if err != nil {
		return nil, mapMeta(err)
	}
	return t.itemEntry(item, "/"+loc), nil
}

func (t *Trash) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	usr, err := t.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	coll, loc, err := splitTrashPath(p)
	if err != nil {
		return nil, err
	}
	if loc != "" {
		ent, err := t.Stat(ctx, user, p)
		if err != nil {
			return nil, err
		}
		if !ent.IsDir {
			return nil, webdav.ErrNotDir
		}
		return nil, nil
	}
	mt := t.now()
	if coll == "" {
		return []*webdav.Entry{
			t.collEntry("/trash", mt),
			t.collEntry("/restore", mt),
		}, nil
	}
	if coll == "restore" {
		return nil, nil
	}
	if coll != "trash" {
		return nil, webdav.ErrForbidden
	}
	if err := t.expire(ctx, user, usr.ID); err != nil {
		return nil, err
	}
	items, err := t.Sessions.List(ctx, usr.ID)
	if err != nil {
		return nil, mapMeta(err)
	}
	out := make([]*webdav.Entry, 0, len(items))
	for i := range items {
		out = append(out, t.itemEntry(&items[i], "/"+items[i].LocationID))
	}
	return out, nil
}

func (t *Trash) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	ent, err := t.Stat(ctx, user, p)
	if err != nil {
		return nil, nil, err
	}
	if ent.IsDir {
		return nil, nil, webdav.ErrIsDir
	}
	_, loc, err := splitTrashPath(p)
	if err != nil {
		return nil, nil, err
	}
	key, err := trashStorageKey(user, loc)
	if err != nil {
		return nil, nil, err
	}
	rc, err := t.Storage.Open(ctx, key)
	if err != nil {
		return nil, nil, mapStorage(err)
	}
	return rc, ent, nil
}

func (t *Trash) Write(context.Context, string, string, io.Reader, *time.Time) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

func (t *Trash) Mkdir(context.Context, string, string) (*webdav.Entry, error) {
	return nil, webdav.ErrForbidden
}

func (t *Trash) Remove(ctx context.Context, user, p string) error {
	_, loc, err := splitTrashPath(p)
	if err != nil {
		return err
	}
	if loc == "" {
		return webdav.ErrForbidden
	}
	return t.PurgeLocation(ctx, user, loc)
}

func (t *Trash) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

func (t *Trash) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

var _ webdav.FS = (*Trash)(nil)
