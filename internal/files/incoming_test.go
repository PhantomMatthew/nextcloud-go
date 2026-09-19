package files

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	body   string
	origin string
	token  string
	rel    string
}

func (s *stubRemoteFile) Get(_ context.Context, origin, token, rel string) (io.ReadCloser, *webdav.Entry, error) {
	s.origin, s.token, s.rel = origin, token, rel
	b := []byte(s.body)
	return io.NopCloser(bytes.NewReader(b)), &webdav.Entry{
		Size: int64(len(b)), ContentType: "text/plain", ETag: "remote-etag",
	}, nil
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
