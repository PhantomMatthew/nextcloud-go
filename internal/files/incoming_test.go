package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

type stubIncoming struct {
	mounts []IncomingMount
}

func (s stubIncoming) ListIncoming(context.Context, string) ([]IncomingMount, error) {
	return s.mounts, nil
}

func TestIncomingJail(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, _, err := dav.Write(ctx, "alice", "/hello.txt", strings.NewReader("hi"), nil); err != nil {
		t.Fatal(err)
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/hello.txt", Mount: "/hello.txt",
		Permissions: webdav.PermRead, ItemType: "file",
	}}}
	e, err := dav.Stat(ctx, "bob", "/hello.txt")
	if err != nil || !e.Shared || e.Shareable {
		t.Fatalf("stat incoming = %+v %v", e, err)
	}
	listed, err := dav.List(ctx, "bob", "/")
	if err != nil || len(listed) != 1 || listed[0].Path != "/hello.txt" {
		t.Fatalf("list = %+v %v", listed, err)
	}
	rc, _, err := dav.Read(ctx, "bob", "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if _, _, err := dav.Write(ctx, "bob", "/hello.txt", strings.NewReader("no"), nil); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("write without update = %v", err)
	}
	if _, _, err := dav.Move(ctx, "bob", "/hello.txt", "bob", "/moved.txt", false); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("move incoming = %v", err)
	}
}

func TestIncomingWriteWithUpdate(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, _, err := dav.Write(ctx, "alice", "/hello.txt", strings.NewReader("hi"), nil); err != nil {
		t.Fatal(err)
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/hello.txt", Mount: "/hello.txt",
		Permissions: webdav.PermRead | webdav.PermUpdate, ItemType: "file",
	}}}
	if _, _, err := dav.Write(ctx, "bob", "/hello.txt", strings.NewReader("bye"), nil); err != nil {
		t.Fatal(err)
	}
	rc, _, err := dav.Read(ctx, "alice", "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	n, _ := rc.Read(buf)
	_ = rc.Close()
	if string(buf[:n]) != "bye" {
		t.Fatalf("owner bytes = %q", buf[:n])
	}
}

func TestIncomingRemotePlaceholder(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		Mount: "/hello-remote.txt", Permissions: webdav.PermRead, ItemType: "file", Remote: true,
	}}}
	e, err := dav.Stat(ctx, "bob", "/hello-remote.txt")
	if err != nil || !e.Shared || !e.Mounted || e.Shareable {
		t.Fatalf("stat remote = %+v %v", e, err)
	}
	listed, err := dav.List(ctx, "bob", "/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range listed {
		if item.Path == "/hello-remote.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("list = %+v", listed)
	}
	if _, _, err := dav.Read(ctx, "bob", "/hello-remote.txt"); !errors.Is(err, webdav.ErrNotImplemented) {
		t.Fatalf("read remote = %v", err)
	}
}

type stubRemoteFile struct {
	body     string
	origin   string
	token    string
	rel      string
	children []*webdav.Entry
	putRel   string
	putBody  string
}

func (s *stubRemoteFile) Get(_ context.Context, origin, token, rel string) (io.ReadCloser, *webdav.Entry, error) {
	s.origin, s.token, s.rel = origin, token, rel
	b := []byte(s.body)
	return io.NopCloser(bytes.NewReader(b)), &webdav.Entry{
		Size: int64(len(b)), ContentType: "text/plain", ETag: "remote-etag",
	}, nil
}

func (s *stubRemoteFile) Propfind(_ context.Context, origin, token, rel string, depth int) ([]*webdav.Entry, error) {
	s.origin, s.token, s.rel = origin, token, rel
	self := &webdav.Entry{Path: "/", IsDir: true, ETag: "dir"}
	if depth == 0 && rel != "/" && rel != "" {
		name := path.Base(rel)
		for _, c := range s.children {
			if c != nil && path.Base(c.Path) == name {
				cp := *c
				return []*webdav.Entry{&cp}, nil
			}
		}
		return nil, webdav.ErrNotFound
	}
	out := []*webdav.Entry{self}
	if depth == 1 {
		out = append(out, s.children...)
	}
	return out, nil
}

func (s *stubRemoteFile) Put(_ context.Context, origin, token, rel string, body io.Reader) (*webdav.Entry, error) {
	s.origin, s.token, s.rel = origin, token, rel
	s.putRel = rel
	b, _ := io.ReadAll(body)
	s.putBody = string(b)
	return &webdav.Entry{ETag: "put-etag", Size: int64(len(b))}, nil
}

func (s *stubRemoteFile) Delete(_ context.Context, origin, token, rel string) error {
	s.origin, s.token, s.rel = origin, token, rel
	return nil
}

func (s *stubRemoteFile) Mkcol(_ context.Context, origin, token, rel string) error {
	s.origin, s.token, s.rel = origin, token, rel
	return nil
}

func TestIncomingRemoteRead(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	remote := &stubRemoteFile{body: "hello from remote\n"}
	dav.Remote = remote
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		Mount: "/hello-remote.txt", Permissions: webdav.PermRead, ItemType: "file", Remote: true,
		RemoteOrigin: "https://remote.example.com", RemoteToken: "ocmtok001",
	}}}
	rc, ent, err := dav.Read(ctx, "bob", "/hello-remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello from remote\n" {
		t.Fatalf("body = %q %v", body, err)
	}
	if ent == nil || !ent.Mounted || !ent.Shared || ent.Size != 18 || ent.ETag != "remote-etag" {
		t.Fatalf("entry = %+v", ent)
	}
	if remote.origin != "https://remote.example.com" || remote.token != "ocmtok001" || remote.rel != "/" {
		t.Fatalf("get args = %q %q %q", remote.origin, remote.token, remote.rel)
	}
}

func TestIncomingRemoteFolderGetIsDir(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		Mount: "/remote-dir", Permissions: webdav.PermRead, ItemType: "folder", Remote: true,
		RemoteOrigin: "https://remote.example.com", RemoteToken: "ocmtok001",
	}}}
	if _, _, err := dav.Read(ctx, "bob", "/remote-dir"); !errors.Is(err, webdav.ErrIsDir) {
		t.Fatalf("folder get = %v", err)
	}
}

func TestIncomingRemoteFolderListAndWrite(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	remote := &stubRemoteFile{
		body: "nested\n",
		children: []*webdav.Entry{{
			Path: "/child.txt", Size: 7, ETag: "child", ContentType: "text/plain",
		}},
	}
	dav.Remote = remote
	folder := IncomingMount{
		Mount: "/remote-dir", Permissions: webdav.PermRead | webdav.PermUpdate | webdav.PermCreate | webdav.PermDelete,
		ItemType: "folder", Remote: true,
		RemoteOrigin: "https://remote.example.com", RemoteToken: "ocmtok001",
	}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{folder}}
	listed, err := dav.List(ctx, "bob", "/remote-dir")
	if err != nil || len(listed) != 1 || listed[0].Path != "/child.txt" {
		t.Fatalf("list = %+v %v", listed, err)
	}
	stent, err := dav.Stat(ctx, "bob", "/remote-dir/child.txt")
	if err != nil || stent.IsDir || stent.Path != "/remote-dir/child.txt" {
		t.Fatalf("stat child = %+v %v", stent, err)
	}
	rc, _, err := dav.Read(ctx, "bob", "/remote-dir/child.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "nested\n" || remote.rel != "/child.txt" {
		t.Fatalf("read = %q rel=%q", body, remote.rel)
	}
	if _, _, err := dav.Write(ctx, "bob", "/remote-dir/child.txt", strings.NewReader("x"), nil); err != nil {
		t.Fatal(err)
	}
	if remote.putRel != "/child.txt" || remote.putBody != "x" {
		t.Fatalf("put = %q %q", remote.putRel, remote.putBody)
	}
	if _, err := dav.Mkdir(ctx, "bob", "/remote-dir/sub"); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "bob", "/remote-dir/child.txt"); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "bob", "/remote-dir"); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("delete mount = %v", err)
	}
}

func TestIncomingRemotePutForbidden(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	dav.Remote = &stubRemoteFile{body: "hello from remote\n"}
	dav.Incoming = stubIncoming{mounts: []IncomingMount{{
		Mount: "/hello-remote.txt", Permissions: webdav.PermRead, ItemType: "file", Remote: true,
		RemoteOrigin: "https://remote.example.com", RemoteToken: "ocmtok001",
	}}}
	if _, _, err := dav.Write(ctx, "bob", "/hello-remote.txt", strings.NewReader("no"), nil); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("readonly put = %v", err)
	}
}
