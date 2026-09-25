package files

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// writeLockTable serializes same-path writes in-process (ADR-0094): 256
// FNV-1a stripes keep unrelated paths parallel while giving every
// (user, path) key a single in-process writer. The zero value is usable.
type writeLockTable struct{ stripes [256]sync.Mutex }

// lock takes the stripe for key and returns its unlock function.
func (t *writeLockTable) lock(key string) func() {
	m := &t.stripes[fnv32a(key)%256]
	m.Lock()
	return m.Unlock
}

// fnv32a is FNV-1a over the key bytes (stdlib algorithm, inlined so the lock
// path carries no error return).
func fnv32a(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	h := uint32(offset32)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}

// WriteIf is Write with preconditions evaluated atomically against the
// in-lock filecache state (ADR-0094); a nil cond behaves exactly like Write.
func (d *DAV) WriteIf(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, cond *webdav.WriteCond) (*webdav.Entry, bool, error) {
	ent, created, err := d.writeConditional(ctx, user, p, r, mtime, true, cond)
	if err != nil {
		return nil, false, err
	}
	// Event emission stays outside the stripe lock.
	d.emitUploaded(ctx, user, ent, created)
	return ent, created, nil
}

// writeConditional resolves the write target — own tree, incoming share, or
// OCM-remote mount — and runs the write core under the per-path stripe lock
// (ADR-0094). Both Write and WriteIf go through it so plain writes serialize
// per path too: plugin and CLI writers race the same storage keys. Target
// resolution reads stay outside the lock: mount tables are stable within a
// request and a create-race converges to the same lock key.
func (d *DAV) writeConditional(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, snapshot bool, cond *webdav.WriteCond) (*webdav.Entry, bool, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	wUser, wPath := user, np
	var mount *IncomingMount
	_, serr := d.statOwned(ctx, user, np)
	switch {
	case serr != nil && !errors.Is(serr, webdav.ErrNotFound):
		return nil, false, serr
	case serr != nil:
		// Not an own file: try an incoming mount.
		if m, ownerPath, lerr := d.lookupIncoming(ctx, user, np); lerr == nil {
			if m.Remote {
				// Preconditions are not enforceable on OCM-remote mounts — the
				// remote server owns the state — so they pass through.
				return d.writeRemote(ctx, np, m, r)
			}
			need := webdav.PermUpdate
			if _, oerr := d.statOwned(ctx, m.OwnerUID, ownerPath); errors.Is(oerr, webdav.ErrNotFound) {
				need = webdav.PermCreate
			} else if oerr != nil {
				return nil, false, oerr
			}
			if m.Permissions&need == 0 {
				return nil, false, webdav.ErrForbidden
			}
			wUser, wPath, mount = m.OwnerUID, ownerPath, m
		}
	}
	unlock := d.writeLocks.lock(wUser + "\x00" + wPath)
	defer unlock()
	ent, created, werr := d.write(ctx, wUser, wPath, r, mtime, snapshot, cond)
	if mount != nil {
		return incomingEntry(ent, np, mount.Permissions), created, werr
	}
	return ent, created, werr
}

var _ webdav.CondWriteFS = (*DAV)(nil)
