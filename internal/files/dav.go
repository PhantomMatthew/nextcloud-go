package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
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
	Locks    LockStore
	Shares   ShareStore
	Incoming IncomingLookup
	NewToken func() string
	Remote   RemoteFile
	// KeySharer, when set, keeps file_keys wrap rows in step with shares on
	// the write path (ADR-0098): covering shares wrap the fresh key, and an
	// overwrite carries recipient wraps across the key-UUID change. Nil
	// disables the feature (per-user keys off) with zero overhead.
	KeySharer *KeySharer
	// DirKeys, when set (filename encryption on, ADR-0104), mints a
	// directory key at every folder creation — mkdir, copy, ingest — so the
	// fresh row carries its key_uuid, and lazily claims one onto the user's
	// root at their first request. Nil means the flag is off and every write
	// stays bit-identical (the never-enable rollback carve-out).
	DirKeys DirKeyMinter
	// Names, when set (filename encryption on, ADR-0104 phase 3a), is the
	// translation core for share-root-anchored resolution of ciphertext
	// incoming mounts. Nil disables the ciphertext-mount branches (no
	// ciphertext share rows exist then — the sharing service seals none).
	Names *NameTranslator
	// RawMeta, when set with Names, is the raw filecache store behind the
	// translating decorator: ciphertext-mount operations build rows with
	// ciphertext Name/Path directly and must not be double-translated.
	RawMeta *SQLStore
	// ShareResealer, when set (filename encryption on, ADR-0104 phase 3a),
	// re-seals share rows' sealed metadata after a rename rewrote their
	// ciphertext file_path prefix. Best-effort like KeySharer: a failure is
	// Warn-logged, never fails the move. *sharing.Service satisfies it.
	ShareResealer ShareMetaResealer
	// Logger, when set, receives the best-effort key-share warnings.
	Logger *slog.Logger
	// LiveProps, when set, attaches plugin-provided custom properties to
	// Stat/List/Read entries and gets first refusal on PROPPATCH ops.
	LiveProps webdav.LivePropProvider
	// Events, when set, receives files.uploaded after a successful Write.
	Events *events.Bus
	// writeLocks serializes same-path writes in-process (ADR-0094).
	writeLocks writeLockTable
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

func (d *DAV) warn(ctx context.Context, msg string, args ...any) {
	if d.Logger != nil {
		d.Logger.WarnContext(ctx, msg, args...)
	}
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
	root, err := d.Meta.EnsureRoot(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	// ADR-0104 phase 1: with filename encryption on, the user's root lazily
	// gains its directory key at the first request — the one existing-row
	// mint site (folder creations mint before their insert).
	if d.DirKeys != nil && len(root.KeyUUID) == 0 {
		keyUUID, _, err := d.DirKeys.AllocateForUser(ctx, u.UID)
		if err != nil {
			return nil, err
		}
		claimed, err := d.Meta.SetKeyUUIDIfNull(ctx, root.ID, keyUUID[:])
		if err != nil {
			return nil, err
		}
		if !claimed {
			// A concurrent mint won the row; this one's wrap row is a
			// benign orphan (reconcile owns the cleanup).
			d.warn(ctx, "files: root directory key claim lost to a concurrent mint", slog.String("uid", u.UID))
		}
	}
	if err := d.ensureHome(ctx, uid); err != nil {
		return nil, err
	}
	return u, nil
}

// Usage returns the user's filecache usage in bytes (sum of file sizes,
// directories excluded) together with the configured quota (nil means
// unlimited). Plugin storage writes check the pair (ADR-0061); the core DAV
// write path itself does not enforce quotas.
func (d *DAV) Usage(ctx context.Context, user string) (int64, *int64, error) {
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return 0, nil, err
	}
	n, err := d.Meta.Usage(ctx, u.ID)
	if err != nil {
		return 0, nil, err
	}
	return n, u.QuotaBytes, nil
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
	e := &webdav.Entry{
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
	d.applyLock(ctx, userID, f.Path, e)
	return e
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

// attachLiveProps queries the optional live-prop provider for each entry and
// attaches the results as ExtraProps. Provider failures never surface here:
// the interface returns only props it could compute.
func (d *DAV) attachLiveProps(ctx context.Context, user string, entries ...*webdav.Entry) {
	if d.LiveProps == nil {
		return
	}
	for _, e := range entries {
		if e == nil {
			continue
		}
		if props := d.LiveProps.PropsFor(ctx, user, e.Path); len(props) > 0 {
			e.ExtraProps = props
		}
	}
}

// PatchProps applies PROPPATCH operations. Live (plugin-provided) props are
// offered to the LiveProps provider first; everything else falls through to
// the persisted oc:favorite logic.
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
		if d.LiveProps != nil {
			val := op.Value
			if op.Remove {
				val = "" // a remove is a set-empty for the provider
			}
			if handled, status := d.LiveProps.SetProp(ctx, user, np, op.Space, op.Name, val); handled {
				res.Status = status
				out = append(out, res)
				continue
			}
		}
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
	e, err := d.statMaybeIncoming(ctx, user, p)
	if err != nil {
		return nil, err
	}
	d.attachLiveProps(ctx, user, e)
	return e, nil
}

func (d *DAV) statMaybeIncoming(ctx context.Context, user, p string) (*webdav.Entry, error) {
	e, err := d.statOwned(ctx, user, p)
	if err == nil || !errors.Is(err, webdav.ErrNotFound) {
		return e, err
	}
	np, nerr := NormalizePath(p)
	if nerr != nil {
		return nil, mapMeta(nerr)
	}
	m, ownerPath, ierr := d.lookupIncoming(ctx, user, np)
	if ierr != nil {
		return nil, ierr
	}
	if m.Remote {
		return d.statRemote(ctx, np, m)
	}
	if m.OwnerCipherPath != "" {
		// Ciphertext mount (ADR-0104 phase 3a): anchor name resolution at
		// the share root — the sharee's wraps cover the in-subtree keys.
		return d.statCipherShare(ctx, np, m)
	}
	e, err = d.statOwned(ctx, m.OwnerUID, ownerPath)
	if err != nil {
		return nil, err
	}
	return incomingEntry(e, np, m.Permissions), nil
}

func (d *DAV) List(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	out, err := d.listMaybeIncoming(ctx, user, p)
	if err != nil {
		return nil, err
	}
	d.attachLiveProps(ctx, user, out...)
	return out, nil
}

func (d *DAV) listMaybeIncoming(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, mapMeta(err)
	}
	out, err := d.listOwned(ctx, user, np)
	if err == nil {
		if np == "/" {
			return d.mergeIncomingRoot(ctx, user, out)
		}
		return out, nil
	}
	if !errors.Is(err, webdav.ErrNotFound) {
		return nil, err
	}
	m, ownerPath, ierr := d.lookupIncoming(ctx, user, np)
	if ierr != nil {
		return nil, ierr
	}
	if m.Remote {
		return d.listRemote(ctx, np, m)
	}
	if m.OwnerCipherPath != "" {
		// Ciphertext mount (ADR-0104 phase 3a): anchored name resolution.
		return d.listCipherShare(ctx, np, m)
	}
	children, err := d.listOwned(ctx, m.OwnerUID, ownerPath)
	if err != nil {
		return nil, err
	}
	rewritten := make([]*webdav.Entry, 0, len(children))
	for _, c := range children {
		rel := strings.TrimPrefix(c.Path, m.OwnerPath)
		rewritten = append(rewritten, incomingEntry(c, path.Join(m.Mount, strings.TrimPrefix(rel, "/")), m.Permissions))
	}
	return rewritten, nil
}

func (d *DAV) Read(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	rc, e, err := d.readMaybeIncoming(ctx, user, p)
	if err != nil {
		return nil, nil, err
	}
	d.attachLiveProps(ctx, user, e)
	return rc, e, nil
}

func (d *DAV) readMaybeIncoming(ctx context.Context, user, p string) (io.ReadCloser, *webdav.Entry, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, nil, mapMeta(err)
	}
	if e, err := d.statOwned(ctx, user, np); err == nil {
		return d.readOwned(ctx, user, np, e)
	} else if err != nil && !errors.Is(err, webdav.ErrNotFound) {
		return nil, nil, err
	}
	m, ownerPath, ierr := d.lookupIncoming(ctx, user, np)
	if ierr != nil {
		return nil, nil, ierr
	}
	if m.Remote {
		return d.readRemote(ctx, np, m)
	}
	if m.Permissions&webdav.PermRead == 0 {
		return nil, nil, webdav.ErrForbidden
	}
	if m.OwnerCipherPath != "" {
		// Ciphertext mount (ADR-0104 phase 3a): the entry resolves anchored
		// at the share root; the storage key is the plaintext owner path
		// (object keys stay plaintext, §10).
		e, err := d.statCipherShare(ctx, np, m)
		if err != nil {
			return nil, nil, err
		}
		rc, _, err := d.readOwned(ctx, m.OwnerUID, ownerPath, e)
		if err != nil {
			return nil, nil, err
		}
		return rc, e, nil
	}
	e, err := d.statOwned(ctx, m.OwnerUID, ownerPath)
	if err != nil {
		return nil, nil, err
	}
	rc, _, err := d.readOwned(ctx, m.OwnerUID, ownerPath, e)
	if err != nil {
		return nil, nil, err
	}
	return rc, incomingEntry(e, np, m.Permissions), nil
}

func (d *DAV) readOwned(ctx context.Context, user, p string, e *webdav.Entry) (io.ReadCloser, *webdav.Entry, error) {
	if e.IsDir {
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
	return rc, e, nil
}

func (d *DAV) readRemote(ctx context.Context, np string, m *IncomingMount) (io.ReadCloser, *webdav.Entry, error) {
	if m == nil {
		return nil, nil, webdav.ErrNotFound
	}
	if m.ItemType == "folder" && np == m.Mount {
		return nil, nil, webdav.ErrIsDir
	}
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return nil, nil, webdav.ErrNotImplemented
	}
	rel, ok := remoteRel(m, np)
	if !ok {
		return nil, nil, webdav.ErrNotFound
	}
	if m.Permissions&webdav.PermRead == 0 {
		return nil, nil, webdav.ErrForbidden
	}
	rc, ent, err := d.Remote.Get(ctx, m.RemoteOrigin, m.RemoteToken, rel)
	if err != nil {
		return nil, nil, err
	}
	out := applyRemoteMeta(m, np, ent)
	if out.ModTime.IsZero() && d.Clock != nil {
		out.ModTime = d.Clock()
	}
	out.IsDir = false
	return rc, out, nil
}

func (d *DAV) statRemote(ctx context.Context, np string, m *IncomingMount) (*webdav.Entry, error) {
	if np == m.Mount {
		e := remoteEntry(m, "/")
		if e != nil && e.ModTime.IsZero() && d.Clock != nil {
			e.ModTime = d.Clock()
		}
		return e, nil
	}
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return nil, webdav.ErrNotImplemented
	}
	rel, ok := remoteRel(m, np)
	if !ok {
		return nil, webdav.ErrNotFound
	}
	if m.Permissions&webdav.PermRead == 0 {
		return nil, webdav.ErrForbidden
	}
	ents, err := d.Remote.Propfind(ctx, m.RemoteOrigin, m.RemoteToken, rel, 0)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e == nil {
			continue
		}
		statPath := np
		if e.IsDir {
			statPath = "/"
		}
		out := applyRemoteMeta(m, statPath, e)
		if out.ModTime.IsZero() && d.Clock != nil {
			out.ModTime = d.Clock()
		}
		return out, nil
	}
	return nil, webdav.ErrNotFound
}

func (d *DAV) listRemote(ctx context.Context, np string, m *IncomingMount) ([]*webdav.Entry, error) {
	if m.ItemType != "folder" {
		return nil, webdav.ErrNotDir
	}
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return nil, webdav.ErrNotImplemented
	}
	rel, ok := remoteRel(m, np)
	if !ok {
		return nil, webdav.ErrNotFound
	}
	if m.Permissions&webdav.PermRead == 0 {
		return nil, webdav.ErrForbidden
	}
	ents, err := d.Remote.Propfind(ctx, m.RemoteOrigin, m.RemoteToken, rel, 1)
	if err != nil {
		return nil, err
	}
	out := make([]*webdav.Entry, 0, len(ents))
	for _, e := range ents {
		if skipRemoteSelf(e) {
			continue
		}
		name := path.Base(e.Path)
		if name == "" || name == "." || name == "/" {
			continue
		}
		child := applyRemoteMeta(m, "/"+name, e)
		if child.ModTime.IsZero() && d.Clock != nil {
			child.ModTime = d.Clock()
		}
		out = append(out, child)
	}
	return out, nil
}

func (d *DAV) Write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time) (*webdav.Entry, bool, error) {
	ent, created, err := d.writeConditional(ctx, user, p, r, mtime, true, nil)
	if err != nil {
		return nil, false, err
	}
	d.emitUploaded(ctx, user, ent, created)
	return ent, created, nil
}

// EventFilesUploaded is the bus topic published after every successful
// Write; the payload is msgpack map{user, path, size, created}.
const EventFilesUploaded = "files.uploaded"

// emitUploaded publishes files.uploaded after a successful write; emission
// never fails the write.
func (d *DAV) emitUploaded(ctx context.Context, user string, ent *webdav.Entry, created bool) {
	if d.Events == nil {
		return
	}
	payload, err := msgpack.Marshal(map[string]any{
		"user":    user,
		"path":    ent.Path,
		"size":    ent.Size,
		"created": created,
	})
	if err != nil {
		return
	}
	d.Events.Publish(ctx, events.Event{Topic: EventFilesUploaded, Payload: payload, Source: "host", UserID: user})
}

// write is the locked write core: callers must hold the writeLocks stripe
// for (user, p) — see writeConditional. Inside the lock only Versions,
// Storage, and Meta calls happen, none of which re-enter write locking.
func (d *DAV) write(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, snapshot bool, cond *webdav.WriteCond) (*webdav.Entry, bool, error) {
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

	// Evaluate preconditions against the in-lock filecache state (ADR-0094);
	// a nil cond is a no-op guard.
	expectETag := ""
	if !created && existing != nil {
		expectETag = existing.ETag
	}
	if err := cond.Evaluate(!created, expectETag); err != nil {
		return nil, false, err
	}

	if !created && snapshot && d.Versions != nil {
		// mapKeyLocked: the snapshot reads the old file at the storage
		// boundary — an enrolled owner's v3 content the writer cannot
		// resolve is a 403 lock, not a 500 (ADR-0101).
		if err := d.Versions.Snapshot(ctx, user, existing); err != nil {
			return nil, false, mapKeyLocked(err)
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
	// A v3-sealing storage layer (per-user keys, ADR-0097) names the file
	// key it wrapped in the sealed header; record the UUID on the filecache
	// row so the key resolver finds the owner without parsing headers.
	// v1/v2-sealing layers do not implement the interface and keyUUID stays
	// nil — which also correctly clears the column when a rewrite seals a
	// former v3 file back to v1/v2. The same writer may also expose the
	// plaintext file key it minted (storage.FileKeyWriter): the key-share
	// hooks thread it so wraps across an overwrite never need a Resolve in
	// the writer's ctx (ADR-0101).
	var keyUUID []byte
	if kw, ok := wc.(storage.KeyUUIDWriter); ok {
		if uuid, reported := kw.SealedKeyUUID(); reported {
			keyUUID = uuid[:]
		}
	}
	var fileKey []byte
	if fw, ok := wc.(storage.FileKeyWriter); ok {
		if fk, reported := fw.PlainFileKey(); reported {
			fileKey = fk
		}
	}
	mt := d.now()
	if mtime != nil {
		mt = mtime.UTC()
	}
	checksum := "SHA256:" + hex.EncodeToString(sum.Sum(nil))
	var f *File
	var oldKeyUUID []byte
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
			KeyUUID:     keyUUID,
		}
		if err := d.Meta.Insert(ctx, f); err != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(err))
		}
	} else {
		oldKeyUUID = existing.KeyUUID
		existing.Size = n
		existing.Mtime = mt
		existing.Checksum = checksum
		existing.ETag = ComputeFileETag(existing.ID, mt, n)
		existing.KeyUUID = keyUUID
		// Conditional writes CAS the filecache row against the etag read
		// during evaluation; unconditional writes keep the plain update.
		var uerr error
		if cond == nil {
			uerr = d.Meta.UpdateMeta(ctx, existing)
		} else {
			uerr = d.Meta.UpdateMetaIfETag(ctx, existing, expectETag)
		}
		if uerr != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(uerr))
		}
		f = existing
	}
	// The file row (with its key UUID) is persisted: keep share wrap rows in
	// step (ADR-0098). Best-effort inside the stripe lock — a failure is
	// Warn-logged and never fails the write.
	d.shareFileKey(ctx, u.ID, np, oldKeyUUID, keyUUID, fileKey)
	if err := d.Meta.RecalcAncestors(ctx, u.ID, f.ParentID, mt); err != nil {
		return nil, false, err
	}
	got, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, got), created, nil
}

// shareFileKey runs the ADR-0098 key-share hooks after a write persisted
// its filecache row: an overwrite with a previous v3 key carries sharee
// wraps from the superseded key UUID to the fresh one, then the recipients
// of every share covering the path get a wrap of the new key. Best-effort:
// failures are Warn-logged, never fail the write. Callers hold the write
// stripe lock; WrapForWrite re-reads the covering set to stay consistent
// with a concurrent unshare (which never takes this lock). fileKey, when
// non-nil, is the plaintext key the storage layer minted for the write —
// threading it lets both hooks wrap without a Resolve in the writer's ctx,
// which an enrolled owner's key would not satisfy (ADR-0101).
func (d *DAV) shareFileKey(ctx context.Context, ownerUserID int64, np string, oldUUID, newUUID, fileKey []byte) {
	if d.KeySharer == nil {
		return
	}
	// ADR-0104 phase 3a: shares.file_path is ciphertext for scheme-1
	// owners — Covering's prefix match needs the translated path. The
	// wrap is best-effort, so a translation failure is Warn-logged too.
	keyPath, err := d.shareKeyPath(ctx, ownerUserID, np)
	if err != nil {
		d.warn(ctx, "files: share key wrap path translation failed", slog.String("path", np), slog.Any("err", err))
		return
	}
	d.shareFileKeyCipher(ctx, ownerUserID, keyPath, oldUUID, newUUID, fileKey)
}

// shareFileKeyCipher is shareFileKey with an already-share-table-formed path
// (ciphertext for scheme-1 owners): the ciphertext-mount write path hands it
// over directly — the translating decorator must not re-translate it.
func (d *DAV) shareFileKeyCipher(ctx context.Context, ownerUserID int64, keyPath string, oldUUID, newUUID, fileKey []byte) {
	ks := d.KeySharer
	if ks == nil {
		return
	}
	var oldK, newK [16]byte
	haveOld := len(oldUUID) == 16
	haveNew := len(newUUID) == 16
	if haveOld {
		copy(oldK[:], oldUUID)
	}
	if haveNew {
		copy(newK[:], newUUID)
	}
	if haveOld && haveNew {
		if err := ks.ReWrapForOverwrite(ctx, oldK, newK, fileKey); err != nil {
			d.warn(ctx, "files: share key carry on overwrite failed", slog.String("path", keyPath), slog.Any("err", err))
		}
	}
	if haveNew {
		if err := ks.WrapForWrite(ctx, ownerUserID, keyPath, newK, fileKey); err != nil {
			d.warn(ctx, "files: share key wrap on write failed", slog.String("path", keyPath), slog.Any("err", err))
		}
	}
}

func (d *DAV) Mkdir(ctx context.Context, user, p string) (*webdav.Entry, error) {
	return d.mkdirMaybeIncoming(ctx, user, p)
}

// checkNameBudget enforces the ADR-0104 §3 plaintext name budget before the
// storage backend sees a new path (its own name limits would surface as
// 500s). Only the translating store enforces a budget; the raw store fails
// the assertion and the call is a no-op, keeping flag-off bit-identical.
func (d *DAV) checkNameBudget(ctx context.Context, userID int64, p string) error {
	if bc, ok := d.Meta.(interface {
		CheckNameBudget(context.Context, int64, string) error
	}); ok {
		return mapMeta(bc.CheckNameBudget(ctx, userID, p))
	}
	return nil
}

// shareKeyPath maps the plaintext path the DAV boundary speaks to the form
// the shares table carries (ciphertext for scheme-1 users, ADR-0104 phase
// 3a), so Covering/RenamePath/DeleteByPath prefix matching hits the stored
// rows. Only the translating store translates; the raw store fails the
// assertion and np passes through, keeping flag-off bit-identical.
func (d *DAV) shareKeyPath(ctx context.Context, userID int64, np string) (string, error) {
	if cp, ok := d.Meta.(interface {
		CipherPath(context.Context, int64, string) (string, error)
	}); ok {
		return cp.CipherPath(ctx, userID, np)
	}
	return np, nil
}

func (d *DAV) mkdirOwned(ctx context.Context, user, p string) (*webdav.Entry, error) {
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
	if err := d.checkNameBudget(ctx, u.ID, np); err != nil {
		return nil, err
	}
	key, err := storageKey(user, np)
	if err != nil {
		return nil, err
	}
	if err := d.Storage.Mkdir(ctx, key); err != nil && !errors.Is(err, storage.ErrExists) {
		// In the mkdir context a missing storage entry can only be the
		// parent, and RFC 4918 §9.3.1 wants 409 for it, not 404 (ADR-0078).
		if errors.Is(err, storage.ErrNotFound) {
			return nil, webdav.ErrParentMissing
		}
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
	// ADR-0104 phase 1: with filename encryption on, mint the folder's
	// directory key before the insert so the row is written once, carrying
	// its key_uuid. UNIQUE(user_id, path) rejects a racing insert; the
	// loser's wrap row is a benign orphan (reconcile owns the cleanup).
	var dirUUID [16]byte
	var dirKey []byte
	if d.DirKeys != nil {
		var err error
		dirUUID, dirKey, err = d.DirKeys.AllocateForUser(ctx, u.UID)
		if err != nil {
			return nil, d.compensateDelete(ctx, key, err)
		}
		f.KeyUUID = dirUUID[:]
	}
	if err := d.Meta.Insert(ctx, f); err != nil {
		return nil, d.compensateDelete(ctx, key, mapMeta(err))
	}
	if err := d.Meta.RecalcAncestors(ctx, u.ID, f.ParentID, d.now()); err != nil {
		return nil, err
	}
	// ADR-0104 phase 1 (folder wrap-on-write, moved from phase 3): the
	// recipients of every share covering the new folder get a wrap of its
	// DK. Best-effort, same as the file write path's share hooks. Phase 3a:
	// Covering matches ciphertext share paths, so the path translates first.
	if d.DirKeys != nil && d.KeySharer != nil {
		keyPath, kerr := d.shareKeyPath(ctx, u.ID, np)
		if kerr != nil {
			d.warn(ctx, "files: share key wrap path translation failed", slog.String("path", np), slog.Any("err", kerr))
		} else if err := d.KeySharer.WrapForWrite(ctx, u.ID, keyPath, dirUUID, dirKey); err != nil {
			d.warn(ctx, "files: share key wrap on mkdir failed", slog.String("path", np), slog.Any("err", err))
		}
	}
	got, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err != nil {
		return nil, mapMeta(err)
	}
	return d.toEntry(ctx, u.ID, got), nil
}

func (d *DAV) Remove(ctx context.Context, user, p string) error {
	return d.removeMaybeIncoming(ctx, user, p)
}

func (d *DAV) removeOwned(ctx context.Context, user, p string) error {
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
			return mapKeyLocked(err) // name decryption can hit a locked key
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
	if d.Locks != nil {
		if err := d.Locks.DeleteByPath(ctx, u.ID, np); err != nil {
			return err
		}
	}
	if d.Shares != nil {
		// ADR-0104 phase 3a: shares.file_path is ciphertext for scheme-1
		// owners; translate the plaintext boundary path before the prefix
		// delete. Only the leaf row is gone by now, so translation still
		// resolves (the leaf token needs only its parent's key).
		ctPath, err := d.shareKeyPath(ctx, u.ID, np)
		if err != nil {
			return mapMeta(err)
		}
		if err := d.Shares.DeleteByPath(ctx, u.ID, ctPath); err != nil {
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
	if _, _, err := d.lookupIncoming(ctx, srcUser, srcPath); err == nil {
		return nil, false, webdav.ErrForbidden
	} else if errors.Is(err, encrypt.ErrKeyLocked) {
		return nil, false, err
	}
	if _, _, err := d.lookupIncoming(ctx, dstUser, dstPath); err == nil {
		return nil, false, webdav.ErrForbidden
	} else if errors.Is(err, encrypt.ErrKeyLocked) {
		return nil, false, err
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
	// The destination's name budget must reject before the storage rename
	// (backend name limits would surface as 500s).
	if err := d.checkNameBudget(ctx, u.ID, dst); err != nil {
		return nil, false, err
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
	if d.Locks != nil {
		if err := d.Locks.RenamePath(ctx, u.ID, src, dst); err != nil {
			return nil, false, err
		}
	}
	if d.Shares != nil {
		// ADR-0104 phase 3a: shares.file_path is ciphertext for scheme-1
		// owners — both endpoints translate (only the moved leaf changed
		// place, so both still resolve: leaf tokens need only the parent's
		// key, and the destination leaf exists after RenameSubtree).
		ctSrc, err := d.shareKeyPath(ctx, u.ID, src)
		if err != nil {
			return nil, false, mapMeta(err)
		}
		ctDst, err := d.shareKeyPath(ctx, u.ID, dst)
		if err != nil {
			return nil, false, mapMeta(err)
		}
		if err := d.Shares.RenamePath(ctx, u.ID, ctSrc, ctDst); err != nil {
			return nil, false, err
		}
		// The ciphertext prefix moved; the sealed share metadata copies
		// (abs_path_enc/mount_name_enc) still hold the old plaintext path.
		// Best-effort re-seal, mirroring the KeySharer hook idiom.
		if d.ShareResealer != nil {
			if err := d.ShareResealer.ResealShareMeta(ctx, u.ID, src, dst); err != nil {
				d.warn(ctx, "files: share meta reseal after rename failed", slog.String("src", src), slog.String("dst", dst), slog.Any("err", err))
			}
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
			// ADR-0104 phase 1: mint-then-insert, as in mkdirOwned. The
			// ErrExists race loser's wrap row is a benign orphan.
			if d.DirKeys != nil {
				dirUUID, _, err := d.DirKeys.AllocateForUser(ctx, u.UID)
				if err != nil {
					return err
				}
				f.KeyUUID = dirUUID[:]
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
	case errors.Is(err, ErrETagConflict):
		return webdav.ErrPrecondition
	case errors.Is(err, ErrNameBudget):
		// ADR-0104 §3: over-budget names/paths are a client error (400).
		return webdav.ErrBadRequest
	default:
		// ADR-0104 phase 2: name resolution runs inside store calls, so an
		// enrolled reader without an unlocked session surfaces ErrKeyLocked
		// from any of them — map it to the 403 lock like the content path.
		return mapKeyLocked(err)
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
		return mapKeyLocked(err)
	}
}

var _ webdav.FS = (*DAV)(nil)
