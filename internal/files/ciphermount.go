package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// ADR-0104 phase 3a: ciphertext incoming mounts (OwnerCipherPath != "") —
// shares into a scheme-1 owner's tree. The mount basename's tree token lives
// under the share root's PARENT directory key (never wrapped for sharees) and
// an enrolled offline owner's ancestor chain is unresolvable to anyone, so
// the share row carries the grant-time copies (mount_name_enc/abs_path_enc)
// and every operation here anchors name resolution at the share root: the
// sharee's wrap rows cover exactly the in-subtree directory keys. Content
// operations derive storage keys from the PLAINTEXT owner path (opened from
// abs_path_enc at ListIncoming) — object keys stay plaintext per ADR-0104
// §10 — while filecache rows are read and written in ciphertext form via the
// raw store (the translating decorator would walk the unresolvable ancestor
// chain, or double-translate on insert).

// cipherMountSeams returns the anchored-translation handles a ciphertext
// mount needs. Absent wiring while a ciphertext mount exists is a
// misconfiguration — ciphertext share rows are only produced when the
// filename-encryption wiring is complete — so it is a loud error, never a
// plaintext fallback.
func (d *DAV) cipherMountSeams() (*NameTranslator, *SQLStore, error) {
	if d.Names == nil || d.RawMeta == nil {
		return nil, nil, fmt.Errorf("files: ciphertext share mount without the phase-3a name-translation wiring")
	}
	return d.Names, d.RawMeta, nil
}

// mountRel returns the plaintext path of np relative to the mount root: "/"
// for the mount itself, else "/a/b".
func mountRel(m *IncomingMount, np string) string {
	if np == m.Mount {
		return "/"
	}
	return strings.TrimPrefix(np, m.Mount)
}

// cipherShareTarget resolves np (a sharee-side path inside a ciphertext
// mount) to the anchor row and the ciphertext owner path of the target,
// anchored at the share root. The leaf need not exist (creates).
func (d *DAV) cipherShareTarget(ctx context.Context, m *IncomingMount, np string) (*users.User, *File, string, error) {
	names, _, err := d.cipherMountSeams()
	if err != nil {
		return nil, nil, "", err
	}
	u, err := d.resolveUser(ctx, m.OwnerUID)
	if err != nil {
		return nil, nil, "", err
	}
	anchor, err := d.Names.AnchorRow(ctx, u.ID, m.OwnerCipherPath)
	if err != nil {
		return nil, nil, "", mapMeta(err)
	}
	ctTarget, err := names.CipherPathUnder(ctx, anchor, m.OwnerCipherPath, mountRel(m, np))
	if err != nil {
		return nil, nil, "", mapMeta(err)
	}
	return u, anchor, ctTarget, nil
}

// statCipherShare is statOwned for a ciphertext mount: the target row is read
// raw by its anchored ciphertext path and its name chain decrypted relative
// to the share root (never the generic decryptRow — it walks ancestors to the
// root and 403s for enrolled owners). The entry speaks the plaintext owner
// path inward (favorites/locks stay owner-path keyed) and np outward.
func (d *DAV) statCipherShare(ctx context.Context, np string, m *IncomingMount) (*webdav.Entry, error) {
	names, raw, err := d.cipherMountSeams()
	if err != nil {
		return nil, err
	}
	u, anchor, ctTarget, err := d.cipherShareTarget(ctx, m, np)
	if err != nil {
		return nil, err
	}
	row, err := raw.GetByPath(ctx, u.ID, ctTarget)
	if err != nil {
		return nil, mapMeta(err)
	}
	dec, err := names.DecryptUnderAnchor(ctx, anchor, m.OwnerCipherPath, m.OwnerPath, []File{*row})
	if err != nil {
		return nil, mapMeta(err)
	}
	return incomingEntry(d.toEntry(ctx, u.ID, &dec[0]), np, m.Permissions), nil
}

// listCipherShare is listOwned for a ciphertext mount: raw children of the
// anchored ciphertext directory, decrypted relative to the share root and
// re-sorted by plaintext name (ORDER BY path over tokens is meaningless,
// ADR-0104 §4), with entries rewritten into the sharee's mount namespace.
func (d *DAV) listCipherShare(ctx context.Context, np string, m *IncomingMount) ([]*webdav.Entry, error) {
	names, raw, err := d.cipherMountSeams()
	if err != nil {
		return nil, err
	}
	u, anchor, ctTarget, err := d.cipherShareTarget(ctx, m, np)
	if err != nil {
		return nil, err
	}
	dir, err := raw.GetByPath(ctx, u.ID, ctTarget)
	if err != nil {
		return nil, mapMeta(err)
	}
	if !dir.IsDir {
		return nil, webdav.ErrNotDir
	}
	children, err := raw.ListChildren(ctx, u.ID, dir.ID)
	if err != nil {
		return nil, mapMeta(err)
	}
	dec, err := names.DecryptUnderAnchor(ctx, anchor, m.OwnerCipherPath, m.OwnerPath, children)
	if err != nil {
		return nil, mapKeyLocked(err) // name decryption can hit a locked key
	}
	sort.SliceStable(dec, func(i, j int) bool {
		if dec[i].Name != dec[j].Name {
			return dec[i].Name < dec[j].Name
		}
		return dec[i].ID < dec[j].ID
	})
	out := make([]*webdav.Entry, 0, len(dec))
	for i := range dec {
		e := d.toEntry(ctx, u.ID, &dec[i])
		rel := strings.TrimPrefix(e.Path, m.OwnerPath)
		out = append(out, incomingEntry(e, path.Join(m.Mount, strings.TrimPrefix(rel, "/")), m.Permissions))
	}
	return out, nil
}

// mkdirCipherMount is mkdirOwned for a ciphertext mount: the storage tree is
// name-agnostic (plaintext keys), while the filecache row is built directly
// in ciphertext form — anchored tokenization under the share root, raw
// insert, the fresh directory key minted for the OWNER (Allocate needs no
// session, ADR-0100) and wrapped for every covering-share recipient
// (WrapForWrite matches ciphertext share paths, phase 3a).
func (d *DAV) mkdirCipherMount(ctx context.Context, np string, m *IncomingMount) (*webdav.Entry, error) {
	_, raw, err := d.cipherMountSeams()
	if err != nil {
		return nil, err
	}
	if d.DirKeys == nil {
		return nil, fmt.Errorf("files: ciphertext share mount without the directory-key wiring")
	}
	rel := mountRel(m, np)
	if rel == "/" {
		return nil, webdav.ErrExists // the mount root exists
	}
	plainTarget := m.OwnerPath + rel
	// CipherPathUnder enforces the ADR-0104 §3 name budget (rune count per
	// segment, ciphertext path length) before the storage backend sees it.
	// A missing intermediate is a missing PARENT here — 409 like mkdirOwned
	// (RFC 4918 §9.3.1, ADR-0078), not 404.
	u, _, ctTarget, err := d.cipherShareTarget(ctx, m, np)
	if err != nil {
		if errors.Is(err, webdav.ErrNotFound) {
			return nil, webdav.ErrParentMissing
		}
		return nil, err
	}
	key, err := storageKey(m.OwnerUID, plainTarget)
	if err != nil {
		return nil, err
	}
	if err := d.Storage.Mkdir(ctx, key); err != nil && !errors.Is(err, storage.ErrExists) {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, webdav.ErrParentMissing
		}
		return nil, mapStorage(err)
	}
	// Mint-then-insert, mirroring mkdirOwned: the UNIQUE(user_id, path)
	// race loser's wrap row is a benign orphan (reconcile owns the cleanup).
	dirUUID, dirKey, err := d.DirKeys.AllocateForUser(ctx, m.OwnerUID)
	if err != nil {
		return nil, d.compensateDelete(ctx, key, err)
	}
	f := &File{
		UserID:      u.ID,
		Path:        ctTarget,
		IsDir:       true,
		Mtime:       d.now(),
		MIME:        "httpd/unix-directory",
		Permissions: webdav.PermAll,
		KeyUUID:     dirUUID[:],
		NameScheme:  encrypt.NameSchemeNCGOFN1,
	}
	if err := raw.Insert(ctx, f); err != nil {
		return nil, d.compensateDelete(ctx, key, mapMeta(err))
	}
	if err := raw.RecalcAncestors(ctx, u.ID, f.ParentID, d.now()); err != nil {
		return nil, err
	}
	if d.KeySharer != nil {
		if err := d.KeySharer.WrapForWrite(ctx, u.ID, ctTarget, dirUUID, dirKey); err != nil {
			d.warn(ctx, "files: share key wrap on mount mkdir failed", slog.String("path", np), slog.Any("err", err))
		}
	}
	got, err := raw.GetByPath(ctx, u.ID, ctTarget)
	if err != nil {
		return nil, mapMeta(err)
	}
	got.Name = path.Base(np)
	got.Path = plainTarget
	return incomingEntry(d.toEntry(ctx, u.ID, got), np, m.Permissions), nil
}

// writeCipherMount is the write() core for a ciphertext mount (ADR-0104
// phase 3a). Storage keys come from the plaintext owner path; filecache rows
// are read/written in ciphertext form through the raw store; the share-key
// hooks run with the ciphertext path (Covering matches it). Version
// snapshots pin the ciphertext owner path into file_versions via the raw
// version store. Callers hold the writeLocks stripe for (owner, plainTarget).
func (d *DAV) writeCipherMount(ctx context.Context, np string, m *IncomingMount, r io.Reader, mtime *time.Time, snapshot bool, cond *webdav.WriteCond) (*webdav.Entry, bool, error) {
	_, raw, err := d.cipherMountSeams()
	if err != nil {
		return nil, false, err
	}
	rel := mountRel(m, np)
	plainTarget := m.OwnerPath
	if rel != "/" {
		plainTarget += rel
	}
	u, _, ctTarget, err := d.cipherShareTarget(ctx, m, np)
	if err != nil {
		return nil, false, err
	}
	parentCt := path.Dir(ctTarget)
	if parentCt == "." {
		parentCt = "/"
	}
	parent, err := raw.GetByPath(ctx, u.ID, parentCt)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	if !parent.IsDir {
		return nil, false, webdav.ErrNotDir
	}

	existing, err := raw.GetByPath(ctx, u.ID, ctTarget)
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
		if err := d.snapshotCipherMount(ctx, u, ctTarget, plainTarget, existing); err != nil {
			return nil, false, mapKeyLocked(err)
		}
	}

	key, err := storageKey(m.OwnerUID, plainTarget)
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
	// See write() for the key-UUID/file-key threading contract (ADR-0097/0101).
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
			Path:        ctTarget,
			IsDir:       false,
			Size:        n,
			Mtime:       mt,
			Checksum:    checksum,
			MIME:        "application/octet-stream",
			Permissions: webdav.PermAll,
			KeyUUID:     keyUUID,
			NameScheme:  encrypt.NameSchemeNCGOFN1,
		}
		if err := raw.Insert(ctx, f); err != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(err))
		}
	} else {
		oldKeyUUID = existing.KeyUUID
		existing.Size = n
		existing.Mtime = mt
		existing.Checksum = checksum
		existing.ETag = ComputeFileETag(existing.ID, mt, n)
		existing.KeyUUID = keyUUID
		var uerr error
		if cond == nil {
			uerr = raw.UpdateMeta(ctx, existing)
		} else {
			uerr = raw.UpdateMetaIfETag(ctx, existing, expectETag)
		}
		if uerr != nil {
			return nil, false, d.compensateDelete(ctx, key, mapMeta(uerr))
		}
		f = existing
	}
	// The ciphertext path is the share-table form: the wrap hooks match it
	// directly (no translation — the translating decorator must not see it).
	d.shareFileKeyCipher(ctx, u.ID, ctTarget, oldKeyUUID, keyUUID, fileKey)
	if err := raw.RecalcAncestors(ctx, u.ID, f.ParentID, mt); err != nil {
		return nil, false, err
	}
	got, err := raw.GetByPath(ctx, u.ID, ctTarget)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	// The entry speaks the plaintext owner path inward (favorites/locks stay
	// owner-path keyed); the caller rewrites it into the mount namespace.
	got.Name = path.Base(np)
	got.Path = plainTarget
	return d.toEntry(ctx, u.ID, got), created, nil
}

// snapshotCipherMount is Versions.Snapshot for a ciphertext-mount overwrite:
// the version row carries the ciphertext owner path verbatim (the
// translating version store would re-translate it), while the bytes copy
// from the plaintext storage key. Same insert-then-copy with compensating
// delete as Snapshot.
func (d *DAV) snapshotCipherMount(ctx context.Context, owner *users.User, ctPath, plainTarget string, existing *File) error {
	rawer, ok := d.Versions.Meta.(interface{ Raw() VersionStore })
	if !ok {
		return fmt.Errorf("files: ciphertext mount snapshot without the raw version store wiring")
	}
	rawVersions := rawer.Raw()
	// uniqueRevision against the raw store, keyed by the ciphertext path.
	base := strconv.FormatInt(existing.Mtime.Unix(), 10)
	rev := ""
	for i := 0; i < 1000; i++ {
		cand := base
		if i > 0 {
			cand = base + "-" + strconv.Itoa(i)
		}
		_, err := rawVersions.Get(ctx, owner.ID, ctPath, cand)
		if errors.Is(err, ErrNotFound) {
			rev = cand
			break
		}
		if err != nil {
			return err
		}
	}
	if rev == "" {
		return webdav.ErrExists
	}
	item := &FileVersion{
		UserID:   owner.ID,
		Path:     ctPath,
		Revision: rev,
		Size:     existing.Size,
		Checksum: existing.Checksum,
		Created:  d.now(),
	}
	if err := rawVersions.Insert(ctx, item); err != nil {
		return mapMeta(err)
	}
	from, err := storageKey(owner.UID, plainTarget)
	if err != nil {
		return err
	}
	vs := &Versions{Storage: d.Versions.Storage, Meta: rawVersions, Users: d.Versions.Users, Clock: d.Versions.Clock}
	if err := vs.copyToVersion(ctx, owner.UID, item, from); err != nil {
		if delErr := rawVersions.Delete(ctx, owner.ID, ctPath, rev); delErr != nil && !errors.Is(delErr, ErrNotFound) {
			return errors.Join(err, delErr)
		}
		return err
	}
	return nil
}

// checkLockCipherMount is checkLockOwned for a ciphertext mount: lock rows
// are keyed by the owner's ciphertext paths, translated anchor-relative (the
// translating lock store would walk the owner's root, unresolvable for an
// enrolled owner in the sharee's ctx). The walk stops at the mount root:
// owner-side ancestors above the share root are outside the sharee's wraps
// by design (a lock there is invisible through the mount — accepted
// residual, mirroring the name-resolution boundary).
func (d *DAV) checkLockCipherMount(ctx context.Context, m *IncomingMount, p, ifHeader string) error {
	if d.Locks == nil {
		return nil
	}
	names, _, err := d.cipherMountSeams()
	if err != nil {
		return err
	}
	rawer, ok := d.Locks.(interface{ Raw() LockStore })
	if !ok {
		return fmt.Errorf("files: ciphertext mount lock check without the raw lock store wiring")
	}
	rawLocks := rawer.Raw()
	u, err := d.resolveUser(ctx, m.OwnerUID)
	if err != nil {
		return err
	}
	anchor, err := d.Names.AnchorRow(ctx, u.ID, m.OwnerCipherPath)
	if err != nil {
		return mapMeta(err)
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	now := d.now()
	for cur := np; ; cur = parentFilePath(cur) {
		ctCur, err := names.CipherPathUnder(ctx, anchor, m.OwnerCipherPath, mountRel(m, cur))
		if err != nil {
			return mapMeta(err)
		}
		existing, err := rawLocks.GetByPath(ctx, u.ID, ctCur)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return mapMeta(err)
		}
		if existing != nil {
			expired, err := d.expireLock(ctx, existing, now)
			if err != nil {
				return err
			}
			if !expired {
				if lockHeaderHasToken(ifHeader, existing.Token) {
					return nil
				}
				return webdav.ErrLocked
			}
		}
		if cur == m.Mount {
			return nil
		}
	}
}
