package files

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

type memShares struct {
	byID    map[int64]*Share
	byToken map[string]*Share
	next    int64
}

func newMemShares() *memShares {
	return &memShares{byID: map[int64]*Share{}, byToken: map[string]*Share{}, next: 1}
}

func (m *memShares) Insert(_ context.Context, s *Share) error {
	if _, ok := m.byToken[s.Token]; ok {
		return ErrExists
	}
	s.ID = m.next
	m.next++
	cp := *s
	m.byID[s.ID] = &cp
	m.byToken[s.Token] = &cp
	return nil
}

func (m *memShares) GetByID(_ context.Context, id int64) (*Share, error) {
	s, ok := m.byID[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *memShares) GetByToken(_ context.Context, token string) (*Share, error) {
	s, ok := m.byToken[token]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *memShares) ListByOwner(_ context.Context, ownerUserID int64, pathFilter string) ([]Share, error) {
	var out []Share
	for _, s := range m.byID {
		if s.OwnerUserID != ownerUserID {
			continue
		}
		if pathFilter != "" && s.Path != pathFilter {
			continue
		}
		out = append(out, *s)
	}
	return out, nil
}

func (m *memShares) Update(_ context.Context, s *Share) error {
	cur, ok := m.byID[s.ID]
	if !ok {
		return ErrNotFound
	}
	*cur = *s
	m.byToken[s.Token] = cur
	return nil
}

func (m *memShares) Delete(_ context.Context, id int64) error {
	s, ok := m.byID[id]
	if !ok {
		return nil
	}
	delete(m.byID, id)
	delete(m.byToken, s.Token)
	return nil
}

func (m *memShares) DeleteByPath(_ context.Context, ownerUserID int64, filePath string) error {
	for id, s := range m.byID {
		if s.OwnerUserID == ownerUserID && (s.Path == filePath || strings.HasPrefix(s.Path, filePath+"/")) {
			delete(m.byToken, s.Token)
			delete(m.byID, id)
		}
	}
	return nil
}

func (m *memShares) RenamePath(_ context.Context, ownerUserID int64, srcPath, dstPath string) error {
	for _, s := range m.byID {
		if s.OwnerUserID != ownerUserID {
			continue
		}
		if s.Path == srcPath {
			s.Path = dstPath
		} else if strings.HasPrefix(s.Path, srcPath+"/") {
			s.Path = dstPath + strings.TrimPrefix(s.Path, srcPath)
		}
	}
	return nil
}

func newPublicTestDAV(t *testing.T) (*DAV, *memShares, *users.User) {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	shares := newMemShares()
	dav.Shares = shares
	dav.Locks = NewSQLLockStore(db)
	tr := NewTrash(st, NewSQLTrashStore(db), dav, us)
	dav.Trash = tr
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	tr.Clock = func() time.Time { return freeze }
	return dav, shares, u
}

func TestPublicDAVJailAndPerms(t *testing.T) {
	ctx := t.Context()
	dav, shares, u := newPublicTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Mkdir(ctx, "alice", "/pub"); err != nil {
		t.Fatal(err)
	}
	sh := &Share{OwnerUserID: u.ID, ShareType: ShareTypeLink, Path: "/a.txt", ItemType: "file", Token: "tokfile", Permissions: webdav.PermRead}
	if err := shares.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	folder := &Share{OwnerUserID: u.ID, ShareType: ShareTypeLink, Path: "/pub", ItemType: "folder", Token: "tokdir", Permissions: webdav.PermRead | webdav.PermCreate | webdav.PermUpdate | webdav.PermDelete}
	if err := shares.Insert(ctx, folder); err != nil {
		t.Fatal(err)
	}
	pub := &PublicDAV{Files: dav, Resolve: func(ctx context.Context, token string) (*Share, *users.User, error) {
		got, err := shares.GetByToken(ctx, token)
		if err != nil {
			return nil, nil, err
		}
		return got, u, nil
	}}

	ent, err := pub.Stat(ctx, "tokfile", "/")
	if err != nil || ent.Shared != true || ent.Shareable || ent.Path != "/" {
		t.Fatalf("stat file = %+v %v", ent, err)
	}
	if _, _, err := pub.Write(ctx, "tokfile", "/", strings.NewReader("x"), nil); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("write file share = %v", err)
	}
	if _, _, err := pub.Move(ctx, "tokfile", "/", "tokfile", "/b", false); !errors.Is(err, webdav.ErrMethodNotAllowed) {
		t.Fatalf("move = %v", err)
	}
	if _, err := pub.Lock(ctx, "tokfile", "/", webdav.LockRequest{}); !errors.Is(err, webdav.ErrMethodNotAllowed) {
		t.Fatalf("lock = %v", err)
	}

	if _, _, err := pub.Write(ctx, "tokdir", "/n.txt", strings.NewReader("n"), nil); err != nil {
		t.Fatal(err)
	}
	rc, _, err := pub.Read(ctx, "tokdir", "/n.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "n" {
		t.Fatalf("read = %q", b)
	}

	if err := dav.CheckLock(ctx, "alice", "/a.txt", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Lock(ctx, "alice", "/a.txt", webdav.LockRequest{Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := pub.CheckLock(ctx, "tokfile", "/", ""); !errors.Is(err, webdav.ErrLocked) {
		t.Fatalf("public checklock = %v", err)
	}
}

func TestDAVMoveRenamesShares(t *testing.T) {
	ctx := t.Context()
	dav, shares, u := newPublicTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	sh := &Share{OwnerUserID: u.ID, Path: "/a.txt", ItemType: "file", Token: "t1", Permissions: 1}
	if err := shares.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/a.txt", "alice", "/b.txt", false); err != nil {
		t.Fatal(err)
	}
	got, err := shares.GetByToken(ctx, "t1")
	if err != nil || got.Path != "/b.txt" {
		t.Fatalf("renamed = %+v %v", got, err)
	}
}

func TestTrashDeletesShares(t *testing.T) {
	ctx := t.Context()
	dav, shares, u := newPublicTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	sh := &Share{OwnerUserID: u.ID, Path: "/a.txt", ItemType: "file", Token: "t1", Permissions: 1}
	if err := shares.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.GetByToken(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("share after trash = %v", err)
	}
}
