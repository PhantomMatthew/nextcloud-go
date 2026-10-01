package files

import (
	"context"
	"errors"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// WOPI lock seams (ADR-0106): WOPI lock ids are client-chosen arbitrary
// strings, so the opaquelocktoken:-minting dav.Lock cannot serve the
// Collabora callbacks. These three exported methods mirror Lock/Unlock —
// including the incoming-share dispatch of CheckLock (incoming.go), so a
// sharee's WOPI lock lands in the OWNER's lock namespace and owner/sharee
// contend on one row — but store and compare the given token verbatim
// (no prefix minting, no angle-bracket stripping). Ciphertext mounts need
// no dedicated branch here: the owned path resolves through the
// translating lock store keyed by owner id, so an enrolled owner surfaces
// ErrKeyLocked (the ADR-0101 403 boundary) in the anonymous WOPI callback
// ctx, exactly like the content path.

// LockWithToken is Lock with a caller-chosen token. An existing non-expired
// lock with the same token is refreshed (idempotent re-LOCK); a different
// token is webdav.ErrLocked. timeout<=0 selects defaultLockTimeout, capped
// at maxLockTimeout — the same constants dav.Lock uses.
func (d *DAV) LockWithToken(ctx context.Context, user, p, token string, timeout time.Duration) (*webdav.LockInfo, error) {
	if token == "" {
		return nil, webdav.ErrBadRequest
	}
	owner, ownerPath, err := d.wopiLockTarget(ctx, user, p)
	if err != nil {
		return nil, err
	}
	return d.lockOwnedWithToken(ctx, owner, ownerPath, token, timeout)
}

// UnlockWithToken deletes the lock at p only when the stored token matches
// verbatim; no lock, an expired lock, or a mismatch is webdav.ErrConflict
// (mirroring dav.Unlock — the WOPI handler maps it to 409).
func (d *DAV) UnlockWithToken(ctx context.Context, user, p, token string) error {
	if token == "" {
		return webdav.ErrBadRequest
	}
	owner, ownerPath, err := d.wopiLockTarget(ctx, user, p)
	if err != nil {
		return err
	}
	return d.unlockOwnedWithToken(ctx, owner, ownerPath, token)
}

// LockTokenAt returns the current non-expired lock token at p, if any.
func (d *DAV) LockTokenAt(ctx context.Context, user, p string) (string, bool, error) {
	if d.Locks == nil {
		return "", false, nil
	}
	owner, ownerPath, err := d.wopiLockTarget(ctx, user, p)
	if err != nil {
		return "", false, err
	}
	u, err := d.resolveUser(ctx, owner)
	if err != nil {
		return "", false, err
	}
	np, err := NormalizePath(ownerPath)
	if err != nil {
		return "", false, mapMeta(err)
	}
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		return "", false, mapMeta(err) // lock paths encrypt under ADR-0104
	}
	expired, err := d.expireLock(ctx, existing, d.now())
	if err != nil {
		return "", false, err
	}
	if expired {
		return "", false, nil
	}
	return existing.Token, true, nil
}

// wopiLockTarget resolves (user, p) to the (ownerUID, ownerPath) the lock
// row keys on, mirroring CheckLock's dispatch. Remote mounts have no local
// lock store — locking one is not implemented (resolve never yields one in
// practice: a remote mount's owner path is no local filecache row).
func (d *DAV) wopiLockTarget(ctx context.Context, user, p string) (string, string, error) {
	if m, ownerPath, err := d.lookupIncoming(ctx, user, p); err == nil {
		if m.Remote {
			return "", "", webdav.ErrNotImplemented
		}
		return m.OwnerUID, ownerPath, nil
	} else if errors.Is(err, encrypt.ErrKeyLocked) {
		return "", "", err
	}
	return user, p, nil
}

func (d *DAV) lockOwnedWithToken(ctx context.Context, user, p, token string, timeout time.Duration) (*webdav.LockInfo, error) {
	if d.Locks == nil {
		return nil, webdav.ErrForbidden
	}
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return nil, err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return nil, mapMeta(err)
	}
	if _, err := d.Stat(ctx, user, np); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	if timeout > maxLockTimeout {
		timeout = maxLockTimeout
	}
	now := d.now()
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, mapMeta(err) // lock paths encrypt under ADR-0104
	}
	if err == nil {
		if expired, err := d.expireLock(ctx, existing, now); err != nil {
			return nil, err
		} else if expired {
			existing = nil
		}
	}
	if existing != nil {
		if existing.Token == token {
			expiry := now.Add(timeout).UnixMilli()
			if err := d.Locks.UpdateTimeout(ctx, existing.ID, expiry); err != nil {
				return nil, err
			}
			existing.TimeoutMs = expiry
			return lockInfoFromRow(existing, now), nil
		}
		return nil, webdav.ErrLocked
	}
	row := &FileLock{
		UserID:    u.ID,
		Path:      np,
		Token:     token,
		TimeoutMs: now.Add(timeout).UnixMilli(),
		CreatedMs: now.UnixMilli(),
	}
	if err := d.Locks.Insert(ctx, row); err != nil {
		if errors.Is(err, ErrExists) {
			return nil, webdav.ErrLocked
		}
		return nil, mapMeta(err) // budget/locked name translation errors map too
	}
	return lockInfoFromRow(row, now), nil
}

func (d *DAV) unlockOwnedWithToken(ctx context.Context, user, p, token string) error {
	if d.Locks == nil {
		return webdav.ErrForbidden
	}
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	if _, err := d.Stat(ctx, user, np); err != nil {
		return err
	}
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return webdav.ErrConflict
		}
		return mapMeta(err) // lock paths encrypt under ADR-0104
	}
	expired, err := d.expireLock(ctx, existing, d.now())
	if err != nil {
		return err
	}
	if expired {
		return webdav.ErrConflict
	}
	if existing.Token != token {
		return webdav.ErrConflict
	}
	return d.Locks.Delete(ctx, existing.ID)
}
