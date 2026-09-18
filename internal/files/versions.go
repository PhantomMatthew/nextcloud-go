package files

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const defaultVersionRetention = 30 * 24 * time.Hour

// Versions implements webdav.FS for /remote.php/dav/versions/{user}/.
type Versions struct {
	Storage   storage.Storage
	Meta      VersionStore
	Files     *DAV
	Users     users.Store
	Clock     func() time.Time
	Retention time.Duration
}

// NewVersions returns a versions adapter.
func NewVersions(st storage.Storage, meta VersionStore, dav *DAV, u users.Store) *Versions {
	return &Versions{
		Storage:   st,
		Meta:      meta,
		Files:     dav,
		Users:     u,
		Clock:     time.Now,
		Retention: defaultVersionRetention,
	}
}

func (v *Versions) now() time.Time {
	if v.Clock != nil {
		return v.Clock().UTC()
	}
	return time.Now().UTC()
}

func (v *Versions) retention() time.Duration {
	if v.Retention > 0 {
		return v.Retention
	}
	return defaultVersionRetention
}

func (v *Versions) resolveUser(ctx context.Context, uid string) (*users.User, error) {
	if uid == "" || strings.Contains(uid, "/") || uid == ".." {
		return nil, webdav.ErrForbidden
	}
	user, err := v.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, webdav.ErrForbidden
		}
		return nil, err
	}
	return user, nil
}

func versionStorageKey(uid string, id int64) string {
	return "versions/" + uid + "/" + strconv.FormatInt(id, 10)
}

func parseNumericFileID(s string) (int64, bool) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func splitVersionsPath(p string) (coll, fileID, rev string, err error) {
	np, err := NormalizePath(p)
	if err != nil {
		return "", "", "", webdav.ErrForbidden
	}
	if np == "/" {
		return "", "", "", nil
	}
	rel := strings.TrimPrefix(np, "/")
	parts := strings.Split(rel, "/")
	coll = parts[0]
	if coll != "versions" && coll != "restore" {
		return "", "", "", webdav.ErrForbidden
	}
	if len(parts) == 1 {
		return coll, "", "", nil
	}
	if coll == "restore" {
		return coll, "", "", nil
	}
	if len(parts) > 3 {
		return "", "", "", webdav.ErrForbidden
	}
	fileID = parts[1]
	if len(parts) == 3 {
		rev = parts[2]
	}
	return coll, fileID, rev, nil
}

func (v *Versions) uniqueRevision(ctx context.Context, userID int64, filePath string, ts time.Time) (string, error) {
	base := strconv.FormatInt(ts.Unix(), 10)
	rev := base
	for i := 0; i < 1000; i++ {
		if i > 0 {
			rev = base + "-" + strconv.Itoa(i)
		}
		_, err := v.Meta.Get(ctx, userID, filePath, rev)
		if errors.Is(err, ErrNotFound) {
			return rev, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", webdav.ErrExists
}

func (v *Versions) ensureRoot(ctx context.Context, uid string) error {
	if err := v.Storage.Mkdir(ctx, "versions"); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	if err := v.Storage.Mkdir(ctx, "versions/"+uid); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	return nil
}

func (v *Versions) fileForID(ctx context.Context, usr *users.User, fileID string) (*File, error) {
	id, ok := parseNumericFileID(fileID)
	if !ok {
		return nil, webdav.ErrForbidden
	}
	if v.Files == nil {
		return nil, webdav.ErrForbidden
	}
	f, err := v.Files.Meta.GetByID(ctx, id)
	if err != nil {
		return nil, mapMeta(err)
	}
	if f.UserID != usr.ID {
		return nil, webdav.ErrForbidden
	}
	if f.IsDir {
		return nil, webdav.ErrIsDir
	}
	return f, nil
}

func (v *Versions) collEntry(p string, mt time.Time) *webdav.Entry {
	return &webdav.Entry{
		Path:        p,
		IsDir:       true,
		ModTime:     mt,
		Permissions: webdav.PermRead | webdav.PermDelete,
		ContentType: "httpd/unix-directory",
		ETag:        webdav.ComputeETag(0, mt, p),
	}
}

func (v *Versions) revEntry(ver *FileVersion, p string) *webdav.Entry {
	mt := ver.Created
	if mt.IsZero() {
		mt = v.now()
	}
	id := uint64(0)
	if ver.ID > 0 {
		id = uint64(ver.ID)
	}
	return &webdav.Entry{
		Path:        p,
		IsDir:       false,
		Size:        ver.Size,
		ModTime:     mt,
		NumericID:   id,
		Permissions: webdav.PermRead | webdav.PermDelete,
		ContentType: "application/octet-stream",
		ETag:        webdav.ComputeETag(ver.Size, mt, ver.Revision),
		Checksum:    ver.Checksum,
	}
}

func (v *Versions) expire(ctx context.Context, uid string, userID int64, filePath string) error {
	cutoff := v.now().Add(-v.retention())
	expired, err := v.Meta.DeleteExpired(ctx, userID, filePath, cutoff)
	if err != nil {
		return mapMeta(err)
	}
	for i := range expired {
		key := versionStorageKey(uid, expired[i].ID)
		if err := v.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return mapStorage(err)
		}
	}
	return nil
}

func (v *Versions) copyToVersion(ctx context.Context, uid string, ver *FileVersion, srcKey string) error {
	if err := v.ensureRoot(ctx, uid); err != nil {
		return err
	}
	rc, err := v.Storage.Open(ctx, srcKey)
	if err != nil {
		return mapStorage(err)
	}
	defer rc.Close()
	dst := versionStorageKey(uid, ver.ID)
	wc, err := v.Storage.Create(ctx, dst, ver.Size)
	if err != nil {
		return mapStorage(err)
	}
	if _, err := io.Copy(wc, rc); err != nil {
		closeErr := wc.Close()
		delErr := v.Storage.Delete(ctx, dst)
		return errors.Join(err, closeErr, delErr)
	}
	if err := wc.Close(); err != nil {
		delErr := v.Storage.Delete(ctx, dst)
		return errors.Join(err, delErr)
	}
	return nil
}

// Snapshot copies the current file bytes into the versions store.
func (v *Versions) Snapshot(ctx context.Context, user string, f *File) error {
	if f == nil || f.IsDir {
		return nil
	}
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	if f.UserID != usr.ID {
		return webdav.ErrForbidden
	}
	rev, err := v.uniqueRevision(ctx, usr.ID, f.Path, f.Mtime)
	if err != nil {
		return err
	}
	from, err := storageKey(user, f.Path)
	if err != nil {
		return err
	}
	item := &FileVersion{
		UserID:   usr.ID,
		Path:     f.Path,
		Revision: rev,
		Size:     f.Size,
		Checksum: f.Checksum,
		Created:  v.now(),
	}
	if err := v.Meta.Insert(ctx, item); err != nil {
		return mapMeta(err)
	}
	if err := v.copyToVersion(ctx, user, item, from); err != nil {
		if delErr := v.Meta.Delete(ctx, usr.ID, f.Path, rev); delErr != nil && !errors.Is(delErr, ErrNotFound) {
			return errors.Join(err, delErr)
		}
		return err
	}
	return nil
}

// RestoreVersion snapshots the current file then copies revision bytes over it.
func (v *Versions) RestoreVersion(ctx context.Context, user, fileID, revision, destUser string) (*webdav.Entry, bool, error) {
	if user != destUser {
		return nil, false, webdav.ErrForbidden
	}
	if v.Files == nil {
		return nil, false, webdav.ErrForbidden
	}
	if !ValidRevision(revision) {
		return nil, false, webdav.ErrForbidden
	}
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	f, err := v.fileForID(ctx, usr, fileID)
	if err != nil {
		return nil, false, err
	}
	ver, err := v.Meta.Get(ctx, usr.ID, f.Path, revision)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	key := versionStorageKey(user, ver.ID)
	rc, err := v.Storage.Open(ctx, key)
	if err != nil {
		return nil, false, mapStorage(err)
	}
	defer rc.Close()
	return v.Files.write(ctx, user, f.Path, rc, nil, true)
}

// RollbackLatest restores the newest snapshot without creating another version, then deletes it.
func (v *Versions) RollbackLatest(ctx context.Context, user, p string) error {
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	items, err := v.Meta.ListByPath(ctx, usr.ID, np)
	if err != nil {
		return mapMeta(err)
	}
	if len(items) == 0 {
		return ErrNotFound
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Created.After(items[j].Created) })
	latest := items[0]
	key := versionStorageKey(user, latest.ID)
	rc, err := v.Storage.Open(ctx, key)
	if err != nil {
		return mapStorage(err)
	}
	defer rc.Close()
	if _, _, err := v.Files.write(ctx, user, np, rc, nil, false); err != nil {
		return err
	}
	if err := v.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return mapStorage(err)
	}
	if err := v.Meta.Delete(ctx, usr.ID, np, latest.Revision); err != nil && !errors.Is(err, ErrNotFound) {
		return mapMeta(err)
	}
	return nil
}

// DeleteByPath removes all versions for path and descendants.
func (v *Versions) DeleteByPath(ctx context.Context, user, p string) error {
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	deleted, err := v.Meta.DeleteByPath(ctx, usr.ID, p)
	if err != nil {
		return mapMeta(err)
	}
	for i := range deleted {
		key := versionStorageKey(user, deleted[i].ID)
		if err := v.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return mapStorage(err)
		}
	}
	return nil
}

// RenamePath rewrites version paths after a files MOVE.
func (v *Versions) RenamePath(ctx context.Context, user, src, dst string) error {
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	if err := v.Meta.RenamePath(ctx, usr.ID, src, dst); err != nil {
		return mapMeta(err)
	}
	return nil
}

func (v *Versions) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	coll, fileID, rev, err := splitVersionsPath(p)
	if err != nil {
		return nil, err
	}
	mt := v.now()
	if coll == "" || (coll == "restore" && rev == "" && fileID == "") {
		return v.collEntry("/", mt), nil
	}
	if coll == "restore" {
		return v.collEntry("/", mt), nil
	}
	if fileID == "" {
		return v.collEntry("/", mt), nil
	}
	f, err := v.fileForID(ctx, usr, fileID)
	if err != nil {
		return nil, err
	}
	if rev == "" {
		return v.collEntry("/", mt), nil
	}
	if err := v.expire(ctx, user, usr.ID, f.Path); err != nil {
		return nil, err
	}
	ver, err := v.Meta.Get(ctx, usr.ID, f.Path, rev)
	if err != nil {
		return nil, mapMeta(err)
	}
	return v.revEntry(ver, "/"+rev), nil
}

func (v *Versions) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	coll, fileID, rev, err := splitVersionsPath(p)
	if err != nil {
		return nil, err
	}
	if rev != "" {
		return nil, webdav.ErrNotDir
	}
	mt := v.now()
	if coll == "" {
		return []*webdav.Entry{
			v.collEntry("/versions", mt),
			v.collEntry("/restore", mt),
		}, nil
	}
	if coll == "restore" || fileID == "" {
		return nil, nil
	}
	f, err := v.fileForID(ctx, usr, fileID)
	if err != nil {
		return nil, err
	}
	if err := v.expire(ctx, user, usr.ID, f.Path); err != nil {
		return nil, err
	}
	items, err := v.Meta.ListByPath(ctx, usr.ID, f.Path)
	if err != nil {
		return nil, mapMeta(err)
	}
	out := make([]*webdav.Entry, 0, len(items))
	for i := range items {
		out = append(out, v.revEntry(&items[i], "/"+items[i].Revision))
	}
	return out, nil
}

func (v *Versions) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	ent, err := v.Stat(ctx, user, p)
	if err != nil {
		return nil, nil, err
	}
	if ent.IsDir {
		return nil, nil, webdav.ErrIsDir
	}
	_, fileID, rev, err := splitVersionsPath(p)
	if err != nil {
		return nil, nil, err
	}
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return nil, nil, err
	}
	f, err := v.fileForID(ctx, usr, fileID)
	if err != nil {
		return nil, nil, err
	}
	ver, err := v.Meta.Get(ctx, usr.ID, f.Path, rev)
	if err != nil {
		return nil, nil, mapMeta(err)
	}
	rc, err := v.Storage.Open(ctx, versionStorageKey(user, ver.ID))
	if err != nil {
		return nil, nil, mapStorage(err)
	}
	return rc, ent, nil
}

func (v *Versions) Write(context.Context, string, string, io.Reader, *time.Time) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

func (v *Versions) Mkdir(context.Context, string, string) (*webdav.Entry, error) {
	return nil, webdav.ErrForbidden
}

func (v *Versions) Remove(ctx context.Context, user, p string) error {
	_, fileID, rev, err := splitVersionsPath(p)
	if err != nil {
		return err
	}
	if rev == "" {
		return webdav.ErrForbidden
	}
	usr, err := v.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	f, err := v.fileForID(ctx, usr, fileID)
	if err != nil {
		return err
	}
	ver, err := v.Meta.Get(ctx, usr.ID, f.Path, rev)
	if err != nil {
		return mapMeta(err)
	}
	if err := v.Storage.Delete(ctx, versionStorageKey(user, ver.ID)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return mapStorage(err)
	}
	if err := v.Meta.Delete(ctx, usr.ID, f.Path, rev); err != nil && !errors.Is(err, ErrNotFound) {
		return mapMeta(err)
	}
	return nil
}

func (v *Versions) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

func (v *Versions) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

var _ webdav.FS = (*Versions)(nil)
