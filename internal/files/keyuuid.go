package files

import (
	"context"
	"errors"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// KeyUUIDAt reports the filecache key_uuid of the file (uid, path) names,
// resolved exactly as Read resolves it: the own tree first, then incoming
// mounts — including ciphertext mounts anchored at the share root
// (ADR-0104 phase 3a). ok is false when the path names no v3-sealed file
// (absent row, null key_uuid, remote mount, directory). It satisfies
// preview.SourceKeys (ADR-0105 §2): a non-null key_uuid means the file's
// content is sealed under a per-user file key, and that UUID names it.
func (d *DAV) KeyUUIDAt(ctx context.Context, uid, p string) (keyUUID [16]byte, ok bool, err error) {
	if d == nil || d.Meta == nil || d.Users == nil {
		return keyUUID, false, nil
	}
	np, err := NormalizePath(p)
	if err != nil {
		return keyUUID, false, mapMeta(err)
	}
	u, err := d.resolveUser(ctx, uid)
	if err != nil {
		return keyUUID, false, err
	}
	f, err := d.Meta.GetByPath(ctx, u.ID, np)
	if err == nil {
		k, ok := rowKeyUUID(f)
		return k, ok, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return keyUUID, false, mapMeta(err)
	}
	m, ownerPath, ierr := d.lookupIncoming(ctx, uid, np)
	if ierr != nil {
		if errors.Is(ierr, webdav.ErrNotFound) {
			return keyUUID, false, nil
		}
		return keyUUID, false, ierr
	}
	if m.Remote {
		return keyUUID, false, nil
	}
	if m.OwnerCipherPath != "" {
		// Ciphertext mount: the target row is read raw by its anchored
		// ciphertext path, exactly as statCipherShare reads it.
		_, raw, err := d.cipherMountSeams()
		if err != nil {
			return keyUUID, false, err
		}
		ou, _, ctTarget, err := d.cipherShareTarget(ctx, m, np)
		if err != nil {
			return keyUUID, false, err
		}
		f, err = raw.GetByPath(ctx, ou.ID, ctTarget)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return keyUUID, false, nil
			}
			return keyUUID, false, mapMeta(err)
		}
		k, ok := rowKeyUUID(f)
		return k, ok, nil
	}
	ou, err := d.resolveUser(ctx, m.OwnerUID)
	if err != nil {
		return keyUUID, false, err
	}
	f, err = d.Meta.GetByPath(ctx, ou.ID, ownerPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return keyUUID, false, nil
		}
		return keyUUID, false, mapMeta(err)
	}
	k, ok := rowKeyUUID(f)
	return k, ok, nil
}

// rowKeyUUID extracts a row's file-key UUID; a directory, a nil row, or a
// null/malformed key_uuid means the content is not v3-sealed.
func rowKeyUUID(f *File) (keyUUID [16]byte, ok bool) {
	if f == nil || f.IsDir || len(f.KeyUUID) != 16 {
		return keyUUID, false
	}
	copy(keyUUID[:], f.KeyUUID)
	return keyUUID, true
}
