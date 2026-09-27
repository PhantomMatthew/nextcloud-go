package files_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TestKeyUUIDAt pins the ADR-0105 preview seam on *files.DAV: own v3 files
// report their filecache key_uuid; plaintext rows, directories, missing
// paths, and remote mounts report ok=false; incoming shares resolve to the
// owner's row.
func TestKeyUUIDAt(t *testing.T) {
	env := newKeyShareEnv(t, "alice", "bob")
	env.write(t, "/a.txt", "v3 hello")
	env.mkdir(t, "/docs")

	uuid, ok, err := env.dav.KeyUUIDAt(context.Background(), "alice", "/a.txt")
	if err != nil || !ok {
		t.Fatalf("own v3 file KeyUUIDAt = ok %v, err %v", ok, err)
	}
	if got := env.keyUUID(t, "/a.txt"); !bytes.Equal(uuid[:], got) {
		t.Errorf("key uuid = %x, want filecache row's %x", uuid, got)
	}

	if _, ok, err := env.dav.KeyUUIDAt(context.Background(), "alice", "/docs"); err != nil || ok {
		t.Errorf("directory KeyUUIDAt = ok %v, err %v", ok, err)
	}
	if _, ok, err := env.dav.KeyUUIDAt(context.Background(), "alice", "/nope.txt"); err != nil || ok {
		t.Errorf("missing path KeyUUIDAt = ok %v, err %v", ok, err)
	}

	// Incoming share: the sharee's path resolves to the owner's row UUID.
	env.share(t, "/a.txt", files.ShareTypeUser, "bob")
	env.dav.Incoming = stubIncomingFeed{mounts: []files.IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/a.txt", Mount: "/shared.txt",
		Permissions: webdav.PermRead, ItemType: "file",
	}}}
	uuid, ok, err = env.dav.KeyUUIDAt(context.Background(), "bob", "/shared.txt")
	if err != nil || !ok {
		t.Fatalf("sharee KeyUUIDAt = ok %v, err %v", ok, err)
	}
	if got := env.keyUUID(t, "/a.txt"); !bytes.Equal(uuid[:], got) {
		t.Errorf("sharee key uuid = %x, want owner's %x", uuid, got)
	}

	// Remote (OCM) mounts carry no local key.
	env.dav.Incoming = stubIncomingFeed{mounts: []files.IncomingMount{{
		Mount: "/remote", Remote: true, ItemType: "file", Permissions: webdav.PermRead,
	}}}
	if _, ok, err := env.dav.KeyUUIDAt(context.Background(), "bob", "/remote"); err != nil || ok {
		t.Errorf("remote mount KeyUUIDAt = ok %v, err %v", ok, err)
	}
}
