package files

import (
	"context"
	"crypto/md5"  //nolint:gosec // OC-Checksum verification of client-supplied hashes
	"crypto/sha1" //nolint:gosec // OC-Checksum SHA1
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
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

const maxChunks = 10000

// Uploads implements webdav.FS for /remote.php/dav/uploads/{user}/{transferId}/.
type Uploads struct {
	Storage  storage.Storage
	Sessions UploadStore
	Files    *DAV
	Users    users.Store
	Clock    func() time.Time
}

// NewUploads returns an uploads adapter.
func NewUploads(st storage.Storage, sessions UploadStore, dav *DAV, u users.Store) *Uploads {
	return &Uploads{Storage: st, Sessions: sessions, Files: dav, Users: u, Clock: time.Now}
}

func (u *Uploads) now() time.Time {
	if u.Clock != nil {
		return u.Clock().UTC()
	}
	return time.Now().UTC()
}

func (u *Uploads) resolveUser(ctx context.Context, uid string) (*users.User, error) {
	if uid == "" || strings.Contains(uid, "/") || uid == ".." {
		return nil, webdav.ErrForbidden
	}
	user, err := u.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, webdav.ErrForbidden
		}
		return nil, err
	}
	return user, nil
}

func splitUploadPath(p string) (tid, rest string, err error) {
	np, err := NormalizePath(p)
	if err != nil {
		return "", "", webdav.ErrForbidden
	}
	if np == "/" {
		return "", "", nil
	}
	rel := strings.TrimPrefix(np, "/")
	tid, rest, ok := strings.Cut(rel, "/")
	if !ok {
		return tid, "", nil
	}
	if strings.Contains(rest, "/") {
		return "", "", webdav.ErrForbidden
	}
	return tid, rest, nil
}

func chunkStorageKey(uid, tid, name string) (string, error) {
	if !ValidTransferID(tid) {
		return "", webdav.ErrForbidden
	}
	if name == "" {
		return "uploads/" + uid + "/" + tid, nil
	}
	if name != ".file" {
		if _, ok := parseChunkName(name); !ok {
			return "", webdav.ErrForbidden
		}
	}
	return "uploads/" + uid + "/" + tid + "/" + name, nil
}

func parseChunkName(name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	for _, r := range name {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(name, 10, 64)
	if err != nil || n < 1 || n > maxChunks {
		return 0, false
	}
	return n, true
}

func (u *Uploads) sessionEntry(sess *UploadSession, p string) *webdav.Entry {
	mt := sess.Created
	if mt.IsZero() {
		mt = u.now()
	}
	id := uint64(0)
	if sess.ID > 0 {
		id = uint64(sess.ID)
	}
	return &webdav.Entry{
		Path:        p,
		IsDir:       true,
		ModTime:     mt,
		NumericID:   id,
		Permissions: webdav.PermAll,
		Shareable:   false,
		ContentType: "httpd/unix-directory",
		ETag:        webdav.ComputeETag(0, mt, p),
	}
}

func (u *Uploads) Stat(ctx context.Context, user, p string) (*webdav.Entry, error) {
	usr, err := u.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return nil, err
	}
	if tid == "" {
		mt := u.now()
		return &webdav.Entry{
			Path:        "/",
			IsDir:       true,
			ModTime:     mt,
			Permissions: webdav.PermAll,
			ContentType: "httpd/unix-directory",
			ETag:        webdav.ComputeETag(0, mt, "/"),
		}, nil
	}
	sess, err := u.Sessions.Get(ctx, usr.ID, tid)
	if err != nil {
		return nil, mapMeta(err)
	}
	if rest == "" {
		return u.sessionEntry(sess, "/"+tid), nil
	}
	if rest == ".file" {
		return nil, webdav.ErrNotFound
	}
	n, ok := parseChunkName(rest)
	if !ok {
		return nil, webdav.ErrForbidden
	}
	key, err := chunkStorageKey(user, tid, rest)
	if err != nil {
		return nil, err
	}
	info, err := u.Storage.Stat(ctx, key)
	if err != nil {
		return nil, mapStorage(err)
	}
	mt := u.now()
	id := uint64(0)
	if sess.ID > 0 {
		id = uint64(sess.ID)*uint64(maxChunks+1) + uint64(n) //nolint:gosec // bounded chunk index
	}
	return &webdav.Entry{
		Path:        "/" + tid + "/" + rest,
		IsDir:       false,
		Size:        info.Size,
		ModTime:     mt,
		NumericID:   id,
		Permissions: webdav.PermAll,
		ContentType: "application/octet-stream",
		ETag:        webdav.ComputeETag(info.Size, mt, rest),
	}, nil
}

func (u *Uploads) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	usr, err := u.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, webdav.ErrNotDir
	}
	if tid == "" {
		key := "uploads/" + user
		infos, err := u.Storage.List(ctx, key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil, nil
			}
			return nil, mapStorage(err)
		}
		out := make([]*webdav.Entry, 0, len(infos))
		for _, info := range infos {
			name := path.Base(info.Path)
			if !ValidTransferID(name) {
				continue
			}
			sess, err := u.Sessions.Get(ctx, usr.ID, name)
			if err != nil {
				continue
			}
			out = append(out, u.sessionEntry(sess, "/"+name))
		}
		return out, nil
	}
	sess, err := u.Sessions.Get(ctx, usr.ID, tid)
	if err != nil {
		return nil, mapMeta(err)
	}
	key, err := chunkStorageKey(user, tid, "")
	if err != nil {
		return nil, err
	}
	infos, err := u.Storage.List(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil
		}
		return nil, mapStorage(err)
	}
	out := make([]*webdav.Entry, 0, len(infos))
	for _, info := range infos {
		name := path.Base(info.Path)
		n, ok := parseChunkName(name)
		if !ok {
			continue
		}
		id := uint64(0)
		if sess.ID > 0 {
			id = uint64(sess.ID)*uint64(maxChunks+1) + uint64(n) //nolint:gosec // bounded chunk index
		}
		mt := u.now()
		out = append(out, &webdav.Entry{
			Path:        "/" + tid + "/" + name,
			IsDir:       false,
			Size:        info.Size,
			ModTime:     mt,
			NumericID:   id,
			Permissions: webdav.PermAll,
			ContentType: "application/octet-stream",
			ETag:        webdav.ComputeETag(info.Size, mt, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (u *Uploads) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	ent, err := u.Stat(ctx, user, p)
	if err != nil {
		return nil, nil, err
	}
	if ent.IsDir {
		return nil, nil, webdav.ErrIsDir
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return nil, nil, err
	}
	key, err := chunkStorageKey(user, tid, rest)
	if err != nil {
		return nil, nil, err
	}
	rc, err := u.Storage.Open(ctx, key)
	if err != nil {
		return nil, nil, mapStorage(err)
	}
	return rc, ent, nil
}

func (u *Uploads) Write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time) (*webdav.Entry, bool, error) {
	usr, err := u.resolveUser(ctx, user)
	if err != nil {
		return nil, false, err
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return nil, false, err
	}
	if rest == "" || rest == ".file" {
		return nil, false, webdav.ErrForbidden
	}
	if _, ok := parseChunkName(rest); !ok {
		return nil, false, webdav.ErrForbidden
	}
	if _, err := u.Sessions.Get(ctx, usr.ID, tid); err != nil {
		return nil, false, mapMeta(err)
	}
	key, err := chunkStorageKey(user, tid, rest)
	if err != nil {
		return nil, false, err
	}
	_, statErr := u.Storage.Stat(ctx, key)
	created := errors.Is(statErr, storage.ErrNotFound)
	if statErr != nil && !created {
		return nil, false, mapStorage(statErr)
	}
	wc, err := u.Storage.Create(ctx, key, 0)
	if err != nil {
		return nil, false, mapStorage(err)
	}
	if _, err := io.Copy(wc, r); err != nil {
		if cerr := wc.Close(); cerr != nil {
			return nil, false, errors.Join(err, cerr)
		}
		return nil, false, err
	}
	if err := wc.Close(); err != nil {
		return nil, false, err
	}
	ent, err := u.Stat(ctx, user, p)
	if err != nil {
		return nil, false, err
	}
	if mtime != nil {
		ent.ModTime = mtime.UTC()
	}
	return ent, created, nil
}

func (u *Uploads) Mkdir(ctx context.Context, user, p string) (*webdav.Entry, error) {
	return u.MkdirMeta(ctx, user, p, webdav.CollectionMeta{})
}

func (u *Uploads) MkdirMeta(ctx context.Context, user, p string, meta webdav.CollectionMeta) (*webdav.Entry, error) {
	usr, err := u.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return nil, err
	}
	if tid == "" || rest != "" {
		return nil, webdav.ErrForbidden
	}
	if !ValidTransferID(tid) {
		return nil, webdav.ErrForbidden
	}
	key, err := chunkStorageKey(user, tid, "")
	if err != nil {
		return nil, err
	}
	if err := u.ensureUploadRoot(ctx, user); err != nil {
		return nil, err
	}
	if err := u.Storage.Mkdir(ctx, key); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, mapStorage(err)
	}
	sess := &UploadSession{
		UserID:      usr.ID,
		TransferID:  tid,
		Destination: meta.Destination,
		TotalLength: meta.TotalLength,
		Created:     u.now(),
	}
	if err := u.Sessions.Create(ctx, sess); err != nil {
		return nil, mapMeta(err)
	}
	return u.sessionEntry(sess, "/"+tid), nil
}

func (u *Uploads) ensureUploadRoot(ctx context.Context, uid string) error {
	if err := u.Storage.Mkdir(ctx, "uploads"); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	if err := u.Storage.Mkdir(ctx, "uploads/"+uid); err != nil && !errors.Is(err, storage.ErrExists) {
		return mapStorage(err)
	}
	return nil
}

func (u *Uploads) Remove(ctx context.Context, user, p string) error {
	usr, err := u.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	tid, rest, err := splitUploadPath(p)
	if err != nil {
		return err
	}
	if tid == "" {
		return webdav.ErrForbidden
	}
	if _, err := u.Sessions.Get(ctx, usr.ID, tid); err != nil {
		return mapMeta(err)
	}
	if rest != "" {
		key, err := chunkStorageKey(user, tid, rest)
		if err != nil {
			return err
		}
		if err := u.Storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return mapStorage(err)
		}
		return nil
	}
	dirKey, err := chunkStorageKey(user, tid, "")
	if err != nil {
		return err
	}
	infos, err := u.Storage.List(ctx, dirKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return mapStorage(err)
	}
	for _, info := range infos {
		if err := u.Storage.Delete(ctx, info.Path); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return mapStorage(err)
		}
	}
	if err := u.Storage.Delete(ctx, dirKey); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrNotEmpty) {
		return mapStorage(err)
	}
	if err := u.Sessions.Delete(ctx, usr.ID, tid); err != nil && !errors.Is(err, ErrNotFound) {
		return mapMeta(err)
	}
	return nil
}

func (u *Uploads) Move(context.Context, string, string, string, string, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

func (u *Uploads) Copy(context.Context, string, string, string, string, bool, bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrForbidden
}

// Assemble concatenates chunks into Files and removes the staging folder.
func (u *Uploads) Assemble(ctx context.Context, srcUser, transferID, destUser, destPath string, overwrite bool, mtime *time.Time, checksum, ifHeader string) (*webdav.Entry, bool, error) {
	if srcUser != destUser {
		return nil, false, webdav.ErrForbidden
	}
	if u.Files == nil {
		return nil, false, webdav.ErrForbidden
	}
	usr, err := u.resolveUser(ctx, srcUser)
	if err != nil {
		return nil, false, err
	}
	if !ValidTransferID(transferID) {
		return nil, false, webdav.ErrForbidden
	}
	sess, err := u.Sessions.Get(ctx, usr.ID, transferID)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	dest := destPath
	if dest == "" {
		dest = sess.Destination
	}
	np, err := NormalizePath(dest)
	if err != nil || np == "/" {
		return nil, false, webdav.ErrBadRequest
	}
	dest = np
	if err := u.Files.CheckLock(ctx, destUser, dest, ifHeader); err != nil {
		return nil, false, err
	}
	if dest != sess.Destination {
		if err := u.Sessions.UpdateDest(ctx, usr.ID, transferID, dest, sess.TotalLength); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, false, mapMeta(err)
		}
	}

	// The Stat preserves the 409-on-existing semantics for overwrite=false;
	// the If: etag itself is enforced atomically by WriteIf (ADR-0094).
	_, statErr := u.Files.Stat(ctx, destUser, dest)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, webdav.ErrNotFound) {
		return nil, false, statErr
	}
	if exists && !overwrite {
		return nil, false, webdav.ErrExists
	}

	chunks, err := u.orderedChunks(ctx, srcUser, transferID)
	if err != nil {
		return nil, false, err
	}
	if len(chunks) == 0 {
		return nil, false, webdav.ErrBadRequest
	}
	var total int64
	var readers []io.Reader
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			if err := c.Close(); err != nil {
				continue
			}
		}
	}()
	for i, ch := range chunks {
		if ch.index != int64(i+1) {
			return nil, false, webdav.ErrBadRequest
		}
		key, err := chunkStorageKey(srcUser, transferID, ch.name)
		if err != nil {
			return nil, false, err
		}
		rc, err := u.Storage.Open(ctx, key)
		if err != nil {
			return nil, false, mapStorage(err)
		}
		closers = append(closers, rc)
		readers = append(readers, rc)
		total += ch.size
	}
	if sess.TotalLength > 0 && total != sess.TotalLength {
		return nil, false, webdav.ErrBadRequest
	}
	body := io.MultiReader(readers...)
	var hasher hash.Hash
	algo, wantHex, hasSum := splitChecksum(checksum)
	if hasSum {
		hasher = checksumHash(algo)
		if hasher == nil {
			return nil, false, webdav.ErrBadRequest
		}
		body = io.TeeReader(body, hasher)
	}
	entry, created, err := u.Files.WriteIf(ctx, destUser, dest, body, mtime, &webdav.WriteCond{IfETags: webdav.ParseIfETags(ifHeader)})
	if err != nil {
		return nil, false, err
	}
	if hasSum {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, wantHex) {
			var rb error
			if u.Files.Versions != nil {
				rb = u.Files.Versions.RollbackLatest(ctx, destUser, dest)
				if errors.Is(rb, ErrNotFound) {
					rb = u.Files.Purge(ctx, destUser, dest)
				}
			} else {
				rb = u.Files.Purge(ctx, destUser, dest)
			}
			if rb != nil {
				return nil, false, errors.Join(webdav.ErrBadRequest, rb)
			}
			return nil, false, webdav.ErrBadRequest
		}
	}
	if err := u.Remove(ctx, srcUser, "/"+transferID); err != nil {
		return entry, created, err
	}
	return entry, created, nil
}

type chunkInfo struct {
	index int64
	name  string
	size  int64
}

func (u *Uploads) orderedChunks(ctx context.Context, user, tid string) ([]chunkInfo, error) {
	ents, err := u.List(ctx, user, "/"+tid)
	if err != nil {
		return nil, err
	}
	out := make([]chunkInfo, 0, len(ents))
	for _, e := range ents {
		name := path.Base(e.Path)
		n, ok := parseChunkName(name)
		if !ok {
			continue
		}
		out = append(out, chunkInfo{index: n, name: name, size: e.Size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out, nil
}

func splitChecksum(v string) (algo, hexPart string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", "", false
	}
	algo, hexPart, found := strings.Cut(v, ":")
	if !found || algo == "" || hexPart == "" {
		return "", "", false
	}
	return strings.ToUpper(algo), strings.ToLower(hexPart), true
}

func checksumHash(algo string) hash.Hash {
	switch strings.ToUpper(algo) {
	case "SHA256":
		return sha256.New()
	case "SHA1":
		return sha1.New() //nolint:gosec // OC-Checksum SHA1
	case "MD5":
		return md5.New() //nolint:gosec // OC-Checksum MD5
	default:
		return nil
	}
}

var (
	_ webdav.FS          = (*Uploads)(nil)
	_ webdav.MetaMkdirFS = (*Uploads)(nil)
)
