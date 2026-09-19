package files

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// PublicShareResolver looks up a valid (non-expired) public share by token.
type PublicShareResolver func(ctx context.Context, token string) (*Share, *users.User, error)

// PublicDAV jails WebDAV over the owner's files.DAV at the shared path.
type PublicDAV struct {
	Files   *DAV
	Resolve PublicShareResolver
}

func (p *PublicDAV) resolve(ctx context.Context, token string) (*Share, *users.User, error) {
	if p == nil || p.Files == nil || p.Resolve == nil || token == "" {
		return nil, nil, webdav.ErrNotFound
	}
	sh, owner, err := p.Resolve(ctx, token)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, webdav.ErrNotFound
		}
		return nil, nil, err
	}
	return sh, owner, nil
}

func jailPath(sh *Share, rel string) (string, error) {
	np, err := NormalizePath(rel)
	if err != nil {
		return "", mapMeta(err)
	}
	if sh.ItemType == "file" {
		if np != "/" {
			return "", webdav.ErrNotFound
		}
		return sh.Path, nil
	}
	if np == "/" {
		return sh.Path, nil
	}
	if sh.Path == "/" {
		return np, nil
	}
	return sh.Path + np, nil
}

func publicRel(sharePath, abs string) string {
	if abs == sharePath {
		return "/"
	}
	prefix := strings.TrimSuffix(sharePath, "/")
	rel := strings.TrimPrefix(abs, prefix)
	if rel == "" {
		return "/"
	}
	if !strings.HasPrefix(rel, "/") {
		rel = "/" + rel
	}
	return rel
}

func publicize(e *webdav.Entry, sh *Share) *webdav.Entry {
	if e == nil {
		return nil
	}
	out := *e
	out.Path = publicRel(sh.Path, e.Path)
	out.Shareable = false
	out.Shared = true
	out.Permissions = sh.Permissions
	return &out
}

func (p *PublicDAV) Stat(ctx context.Context, token, rel string) (*webdav.Entry, error) {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return nil, err
	}
	ent, err := p.Files.Stat(ctx, owner.UID, abs)
	if err != nil {
		return nil, err
	}
	return publicize(ent, sh), nil
}

func (p *PublicDAV) List(ctx context.Context, token, rel string) ([]*webdav.Entry, error) {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return nil, err
	}
	ents, err := p.Files.List(ctx, owner.UID, abs)
	if err != nil {
		return nil, err
	}
	out := make([]*webdav.Entry, 0, len(ents))
	for _, e := range ents {
		out = append(out, publicize(e, sh))
	}
	return out, nil
}

func (p *PublicDAV) Read(ctx context.Context, token, rel string) (io.ReadCloser, *webdav.Entry, error) {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return nil, nil, err
	}
	rc, ent, err := p.Files.Read(ctx, owner.UID, abs)
	if err != nil {
		return nil, nil, err
	}
	return rc, publicize(ent, sh), nil
}

func (p *PublicDAV) Write(ctx context.Context, token, rel string, r io.Reader, mtime *time.Time) (*webdav.Entry, bool, error) {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return nil, false, err
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return nil, false, err
	}
	_, statErr := p.Files.Stat(ctx, owner.UID, abs)
	need := webdav.PermUpdate
	if statErr != nil {
		need = webdav.PermCreate
	}
	if sh.Permissions&need == 0 {
		return nil, false, webdav.ErrForbidden
	}
	ent, created, err := p.Files.Write(ctx, owner.UID, abs, r, mtime)
	if err != nil {
		return nil, false, err
	}
	return publicize(ent, sh), created, nil
}

func (p *PublicDAV) Mkdir(ctx context.Context, token, rel string) (*webdav.Entry, error) {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return nil, err
	}
	if sh.Permissions&webdav.PermCreate == 0 {
		return nil, webdav.ErrForbidden
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return nil, err
	}
	ent, err := p.Files.Mkdir(ctx, owner.UID, abs)
	if err != nil {
		return nil, err
	}
	return publicize(ent, sh), nil
}

func (p *PublicDAV) Remove(ctx context.Context, token, rel string) error {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return err
	}
	if sh.Permissions&webdav.PermDelete == 0 {
		return webdav.ErrForbidden
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return err
	}
	if abs == sh.Path {
		return webdav.ErrForbidden
	}
	return p.Files.Remove(ctx, owner.UID, abs)
}

func (p *PublicDAV) Move(_ context.Context, _, _, _, _ string, _ bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (p *PublicDAV) Copy(_ context.Context, _, _, _, _ string, _, _ bool) (*webdav.Entry, bool, error) {
	return nil, false, webdav.ErrMethodNotAllowed
}

func (p *PublicDAV) Lock(_ context.Context, _, _ string, _ webdav.LockRequest) (*webdav.LockInfo, error) {
	return nil, webdav.ErrMethodNotAllowed
}

func (p *PublicDAV) Unlock(_ context.Context, _, _, _ string) error {
	return webdav.ErrMethodNotAllowed
}

func (p *PublicDAV) CheckLock(ctx context.Context, token, rel, ifHeader string) error {
	sh, owner, err := p.resolve(ctx, token)
	if err != nil {
		return err
	}
	abs, err := jailPath(sh, rel)
	if err != nil {
		return err
	}
	return p.Files.CheckLock(ctx, owner.UID, abs, ifHeader)
}

var (
	_ webdav.FS     = (*PublicDAV)(nil)
	_ webdav.LockFS = (*PublicDAV)(nil)
)
