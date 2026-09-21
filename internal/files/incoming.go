package files

import (
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func (d *DAV) lookupIncoming(ctx context.Context, user, p string) (*IncomingMount, string, error) {
	if d == nil || d.Incoming == nil {
		return nil, "", webdav.ErrNotFound
	}
	np, err := NormalizePath(p)
	if err != nil {
		return nil, "", mapMeta(err)
	}
	mounts, err := d.Incoming.ListIncoming(ctx, user)
	if err != nil {
		return nil, "", err
	}
	for i := range mounts {
		m := mounts[i]
		if np == m.Mount {
			return &m, m.OwnerPath, nil
		}
		if m.ItemType == "folder" && strings.HasPrefix(np, m.Mount+"/") {
			rel := strings.TrimPrefix(np, m.Mount)
			return &m, m.OwnerPath + rel, nil
		}
	}
	return nil, "", webdav.ErrNotFound
}

func incomingEntry(e *webdav.Entry, requestPath string, perms int) *webdav.Entry {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Path = requestPath
	cp.Permissions = perms
	cp.Shareable = false
	cp.Shared = true
	return &cp
}

func remoteEntry(m *IncomingMount, requestPath string) *webdav.Entry {
	if m == nil {
		return nil
	}
	return applyRemoteMeta(m, requestPath, &webdav.Entry{
		IsDir: m.ItemType == "folder",
		Size:  0,
		ETag:  "ocm-remote",
	})
}

func applyRemoteMeta(m *IncomingMount, requestPath string, e *webdav.Entry) *webdav.Entry {
	if m == nil {
		return nil
	}
	if e == nil {
		e = &webdav.Entry{}
	}
	out := *e
	out.Path = requestPath
	out.Permissions = m.Permissions
	out.Shareable = false
	out.Mounted = true
	out.Shared = true
	if out.ETag == "" {
		out.ETag = "ocm-remote"
	}
	return &out
}

func remoteRel(m *IncomingMount, np string) (string, bool) {
	if m == nil {
		return "", false
	}
	if m.ItemType == "file" {
		if np != m.Mount {
			return "", false
		}
		return "/", true
	}
	if np == m.Mount {
		return "/", true
	}
	if !strings.HasPrefix(np, m.Mount+"/") {
		return "", false
	}
	return strings.TrimPrefix(np, m.Mount), true
}

func skipRemoteSelf(e *webdav.Entry) bool {
	if e == nil {
		return true
	}
	p := strings.Trim(e.Path, "/")
	return p == ""
}

func (d *DAV) statOwned(ctx context.Context, user, p string) (*webdav.Entry, error) {
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

func (d *DAV) listOwned(ctx context.Context, user, p string) ([]*webdav.Entry, error) {
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

func (d *DAV) mergeIncomingRoot(ctx context.Context, user string, own []*webdav.Entry) ([]*webdav.Entry, error) {
	if d.Incoming == nil {
		return own, nil
	}
	mounts, err := d.Incoming.ListIncoming(ctx, user)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(own)+len(mounts))
	for _, e := range own {
		if e != nil {
			seen[path.Base(e.Path)] = struct{}{}
		}
	}
	out := own
	for i := range mounts {
		m := mounts[i]
		name := path.Base(m.Mount)
		if name == "" || name == "/" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if m.Remote {
			out = append(out, remoteEntry(&m, m.Mount))
			seen[name] = struct{}{}
			continue
		}
		ent, err := d.statOwned(ctx, m.OwnerUID, m.OwnerPath)
		if err != nil {
			if errors.Is(err, webdav.ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, incomingEntry(ent, m.Mount, m.Permissions))
		seen[name] = struct{}{}
	}
	return out, nil
}

func (d *DAV) CheckLock(ctx context.Context, user, p, ifHeader string) error {
	if m, ownerPath, err := d.lookupIncoming(ctx, user, p); err == nil {
		if m.Remote {
			return nil
		}
		return d.checkLockOwned(ctx, m.OwnerUID, ownerPath, ifHeader)
	}
	return d.checkLockOwned(ctx, user, p, ifHeader)
}

func (d *DAV) writeMaybeIncoming(ctx context.Context, user, p string, r io.Reader, mtime *time.Time, snapshot bool) (*webdav.Entry, bool, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, false, mapMeta(err)
	}
	if _, err := d.statOwned(ctx, user, np); err == nil {
		return d.write(ctx, user, np, r, mtime, snapshot)
	} else if err != nil && !errors.Is(err, webdav.ErrNotFound) {
		return nil, false, err
	}
	if m, ownerPath, err := d.lookupIncoming(ctx, user, np); err == nil {
		if m.Remote {
			return d.writeRemote(ctx, np, m, r)
		}
		need := webdav.PermUpdate
		if _, serr := d.statOwned(ctx, m.OwnerUID, ownerPath); errors.Is(serr, webdav.ErrNotFound) {
			need = webdav.PermCreate
		} else if serr != nil {
			return nil, false, serr
		}
		if m.Permissions&need == 0 {
			return nil, false, webdav.ErrForbidden
		}
		e, created, werr := d.write(ctx, m.OwnerUID, ownerPath, r, mtime, snapshot)
		return incomingEntry(e, np, m.Permissions), created, werr
	}
	return d.write(ctx, user, np, r, mtime, snapshot)
}

func (d *DAV) mkdirMaybeIncoming(ctx context.Context, user, p string) (*webdav.Entry, error) {
	np, err := NormalizePath(p)
	if err != nil {
		return nil, mapMeta(err)
	}
	if m, ownerPath, err := d.lookupIncoming(ctx, user, np); err == nil {
		if m.Remote {
			return d.mkdirRemote(ctx, np, m)
		}
		if m.Permissions&webdav.PermCreate == 0 {
			return nil, webdav.ErrForbidden
		}
		e, merr := d.mkdirOwned(ctx, m.OwnerUID, ownerPath)
		return incomingEntry(e, np, m.Permissions), merr
	}
	return d.mkdirOwned(ctx, user, np)
}

func (d *DAV) removeMaybeIncoming(ctx context.Context, user, p string) error {
	np, err := NormalizePath(p)
	if err != nil {
		return mapMeta(err)
	}
	if _, err := d.statOwned(ctx, user, np); err == nil {
		return d.removeOwned(ctx, user, np)
	} else if err != nil && !errors.Is(err, webdav.ErrNotFound) {
		return err
	}
	if m, ownerPath, err := d.lookupIncoming(ctx, user, np); err == nil {
		if m.Remote {
			return d.removeRemote(ctx, np, m)
		}
		if m.Permissions&webdav.PermDelete == 0 {
			return webdav.ErrForbidden
		}
		return d.removeOwned(ctx, m.OwnerUID, ownerPath)
	}
	return d.removeOwned(ctx, user, np)
}

func (d *DAV) writeRemote(ctx context.Context, np string, m *IncomingMount, r io.Reader) (*webdav.Entry, bool, error) {
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return nil, false, webdav.ErrNotImplemented
	}
	rel, ok := remoteRel(m, np)
	if !ok || (m.ItemType == "folder" && np == m.Mount) {
		return nil, false, webdav.ErrIsDir
	}
	need := webdav.PermUpdate
	_, serr := d.statRemote(ctx, np, m)
	created := false
	if errors.Is(serr, webdav.ErrNotFound) {
		need = webdav.PermCreate
		created = true
	} else if serr != nil && !errors.Is(serr, webdav.ErrNotImplemented) {
		return nil, false, serr
	}
	if m.Permissions&need == 0 {
		return nil, false, webdav.ErrForbidden
	}
	ent, err := d.Remote.Put(ctx, m.RemoteOrigin, m.RemoteToken, rel, r)
	if err != nil {
		return nil, false, err
	}
	out := applyRemoteMeta(m, np, ent)
	if out.ModTime.IsZero() && d.Clock != nil {
		out.ModTime = d.Clock()
	}
	return out, created, nil
}

func (d *DAV) mkdirRemote(ctx context.Context, np string, m *IncomingMount) (*webdav.Entry, error) {
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return nil, webdav.ErrNotImplemented
	}
	if np == m.Mount {
		return nil, webdav.ErrForbidden
	}
	rel, ok := remoteRel(m, np)
	if !ok {
		return nil, webdav.ErrNotFound
	}
	if m.Permissions&webdav.PermCreate == 0 {
		return nil, webdav.ErrForbidden
	}
	if err := d.Remote.Mkcol(ctx, m.RemoteOrigin, m.RemoteToken, rel); err != nil {
		return nil, err
	}
	out := applyRemoteMeta(m, np, &webdav.Entry{IsDir: true})
	if out.ModTime.IsZero() && d.Clock != nil {
		out.ModTime = d.Clock()
	}
	return out, nil
}

func (d *DAV) removeRemote(ctx context.Context, np string, m *IncomingMount) error {
	if d.Remote == nil || m.RemoteOrigin == "" || m.RemoteToken == "" {
		return webdav.ErrNotImplemented
	}
	if m.ItemType == "folder" && np == m.Mount {
		return webdav.ErrForbidden
	}
	rel, ok := remoteRel(m, np)
	if !ok {
		return webdav.ErrNotFound
	}
	if m.Permissions&webdav.PermDelete == 0 {
		return webdav.ErrForbidden
	}
	return d.Remote.Delete(ctx, m.RemoteOrigin, m.RemoteToken, rel)
}
