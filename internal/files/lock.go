package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	defaultLockTimeout = 1800 * time.Second
	maxLockTimeout     = 86400 * time.Second
)

// Lock implements webdav.LockFS for exclusive write locks.
func (d *DAV) Lock(ctx context.Context, user, p string, req webdav.LockRequest) (*webdav.LockInfo, error) {
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
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	if timeout > maxLockTimeout {
		timeout = maxLockTimeout
	}
	now := d.now()
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err == nil {
		if expired, err := d.expireLock(ctx, existing, now); err != nil {
			return nil, err
		} else if expired {
			existing = nil
		}
	}
	if existing != nil {
		if lockHeaderHasToken(req.Token, existing.Token) {
			expiry := now.Add(timeout).UnixMilli()
			if err := d.Locks.UpdateTimeout(ctx, existing.ID, expiry); err != nil {
				return nil, err
			}
			existing.TimeoutMs = expiry
			return lockInfoFromRow(existing, now), nil
		}
		return nil, webdav.ErrLocked
	}
	if req.Refresh {
		return nil, webdav.ErrPrecondition
	}
	token, err := d.newLockToken()
	if err != nil {
		return nil, err
	}
	row := &FileLock{
		UserID:    u.ID,
		Path:      np,
		Token:     token,
		Owner:     req.Owner,
		TimeoutMs: now.Add(timeout).UnixMilli(),
		CreatedMs: now.UnixMilli(),
	}
	if err := d.Locks.Insert(ctx, row); err != nil {
		if errors.Is(err, ErrExists) {
			return nil, webdav.ErrLocked
		}
		return nil, err
	}
	return lockInfoFromRow(row, now), nil
}

// Unlock implements webdav.LockFS.
func (d *DAV) Unlock(ctx context.Context, user, p, token string) error {
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
	token = stripLockToken(token)
	if token == "" {
		return webdav.ErrBadRequest
	}
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return webdav.ErrConflict
		}
		return err
	}
	now := d.now()
	expired, err := d.expireLock(ctx, existing, now)
	if err != nil {
		return err
	}
	if expired {
		return webdav.ErrConflict
	}
	if !lockTokenMatches(existing.Token, token) {
		return webdav.ErrConflict
	}
	return d.Locks.Delete(ctx, existing.ID)
}

func (d *DAV) checkLockOwned(ctx context.Context, user, p, ifHeader string) error {
	if d.Locks == nil {
		return nil
	}
	u, err := d.resolveUser(ctx, user)
	if err != nil {
		return err
	}
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	existing, err := d.Locks.GetByPath(ctx, u.ID, np)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	now := d.now()
	expired, err := d.expireLock(ctx, existing, now)
	if err != nil {
		return err
	}
	if expired {
		return nil
	}
	if lockHeaderHasToken(ifHeader, existing.Token) {
		return nil
	}
	return webdav.ErrLocked
}

func (d *DAV) expireLock(ctx context.Context, l *FileLock, now time.Time) (bool, error) {
	if l == nil {
		return false, nil
	}
	if l.TimeoutMs > now.UnixMilli() {
		return false, nil
	}
	if err := d.Locks.Delete(ctx, l.ID); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DAV) newLockToken() (string, error) {
	if d.NewToken != nil {
		return d.NewToken(), nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "opaquelocktoken:" + hex.EncodeToString(b[:]), nil
}

func (d *DAV) applyLock(ctx context.Context, userID int64, p string, e *webdav.Entry) {
	if d == nil || d.Locks == nil || e == nil {
		return
	}
	lk, err := d.Locks.GetByPath(ctx, userID, p)
	if err != nil {
		return
	}
	now := d.now()
	expired, err := d.expireLock(ctx, lk, now)
	if err != nil || expired {
		return
	}
	e.LockToken = lk.Token
	e.LockOwner = lk.Owner
	rem := time.UnixMilli(lk.TimeoutMs).UTC().Sub(now)
	if rem < 0 {
		rem = 0
	}
	e.LockTimeout = rem
}

func lockInfoFromRow(l *FileLock, now time.Time) *webdav.LockInfo {
	rem := time.UnixMilli(l.TimeoutMs).UTC().Sub(now)
	if rem < 0 {
		rem = 0
	}
	return &webdav.LockInfo{
		Token:   l.Token,
		Owner:   l.Owner,
		Timeout: rem,
		Path:    l.Path,
	}
}

func lockTokenMatches(stored, provided string) bool {
	return stored != "" && stored == stripLockToken(provided)
}

func stripLockToken(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "<")
	v = strings.TrimSuffix(v, ">")
	return strings.TrimSpace(v)
}

func extractLockTokens(header string) []string {
	var out []string
	for {
		start := strings.Index(header, "<")
		if start < 0 {
			break
		}
		end := strings.Index(header[start:], ">")
		if end < 0 {
			break
		}
		end += start
		tok := strings.TrimSpace(header[start+1 : end])
		header = header[end+1:]
		if strings.HasPrefix(tok, "opaquelocktoken:") {
			out = append(out, tok)
		}
	}
	return out
}

func lockHeaderHasToken(header, token string) bool {
	token = stripLockToken(token)
	if token == "" {
		return false
	}
	for _, t := range extractLockTokens(header) {
		if t == token {
			return true
		}
	}
	return stripLockToken(header) == token
}
