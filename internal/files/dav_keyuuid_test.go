package files

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// newPerUserDAV wires a DAV over a v3-sealing encrypt FS (per-user keys,
// ADR-0097) backed by a real database, with trash and versions wired the
// way production does — version snapshots and upload parts live under
// namespaced storage keys the resolver must still attribute to the owner.
func newPerUserDAV(t *testing.T) (*DAV, *SQLStore) {
	t.Helper()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, encrypt.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	res, err := encrypt.NewSQLResolver(db, [][]byte{key})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := encrypt.NewWithResolver(key, nil, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	meta := NewSQLStore(db)
	dav := NewDAV(fs, meta, us)
	dav.Trash = NewTrash(fs, NewSQLTrashStore(db), dav, us)
	dav.Versions = NewVersions(fs, NewSQLVersionStore(db), dav, us)
	return dav, meta
}

func readDAV(t *testing.T, dav *DAV, p string) string {
	t.Helper()
	rc, _, err := dav.Read(context.Background(), "alice", p)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestDAVPerUserKeysRoundTrip(t *testing.T) {
	ctx := context.Background()
	dav, meta := newPerUserDAV(t)
	u, err := dav.Users.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := dav.Mkdir(ctx, "alice", "/docs"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("v3 hello")), nil); err != nil {
		t.Fatal(err)
	}
	f, err := meta.GetByPath(ctx, u.ID, "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.KeyUUID) != 16 {
		t.Fatalf("key_uuid = %d bytes, want 16", len(f.KeyUUID))
	}
	firstUUID := bytes.Clone(f.KeyUUID)
	if got := readDAV(t, dav, "/docs/a.txt"); got != "v3 hello" {
		t.Fatalf("read = %q", got)
	}

	// Overwrite (with a version snapshot on the way): a fresh key UUID, the
	// old content is gone, and the snapshot under versions/<uid>/<id> —
	// a namespaced storage key — sealed fine.
	if _, _, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("v3 hello again")), nil); err != nil {
		t.Fatal(err)
	}
	f2, err := meta.GetByPath(ctx, u.ID, "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(f2.KeyUUID) != 16 || bytes.Equal(f2.KeyUUID, firstUUID) {
		t.Fatalf("overwrite key_uuid = %x, want a fresh 16-byte uuid (first was %x)", f2.KeyUUID, firstUUID)
	}
	if got := readDAV(t, dav, "/docs/a.txt"); got != "v3 hello again" {
		t.Fatalf("read after overwrite = %q", got)
	}
	st, err := dav.Stat(ctx, "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := dav.Versions.List(ctx, "alice", "/versions/"+strconv.FormatUint(st.NumericID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1 (snapshot of the first write)", len(versions))
	}

	// Move keeps the object and its key UUID; reads still work.
	if _, _, err := dav.Move(ctx, "alice", "/docs/a.txt", "alice", "/docs/moved.txt", false); err != nil {
		t.Fatal(err)
	}
	fm, err := meta.GetByPath(ctx, u.ID, "/docs/moved.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fm.KeyUUID, f2.KeyUUID) {
		t.Errorf("moved key_uuid = %x, want unchanged %x", fm.KeyUUID, f2.KeyUUID)
	}
	if got := readDAV(t, dav, "/docs/moved.txt"); got != "v3 hello again" {
		t.Fatalf("read after move = %q", got)
	}

	// Copy Creates a fresh object: a distinct key UUID, both readable.
	if _, _, err := dav.Copy(ctx, "alice", "/docs/moved.txt", "alice", "/docs/copy.txt", false, true); err != nil {
		t.Fatal(err)
	}
	fc, err := meta.GetByPath(ctx, u.ID, "/docs/copy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.KeyUUID) != 16 || bytes.Equal(fc.KeyUUID, fm.KeyUUID) {
		t.Errorf("copied key_uuid = %x, want a fresh uuid (source %x)", fc.KeyUUID, fm.KeyUUID)
	}
	if got := readDAV(t, dav, "/docs/copy.txt"); got != "v3 hello again" {
		t.Fatalf("read copy = %q", got)
	}
}

func TestDAVPlainStorageKeepsKeyUUIDNil(t *testing.T) {
	ctx := context.Background()
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
	meta := NewSQLStore(db)
	dav := NewDAV(st, meta, us)
	if _, _, err := dav.Write(ctx, "alice", "/plain.txt", bytes.NewReader([]byte("plain")), nil); err != nil {
		t.Fatal(err)
	}
	f, err := meta.GetByPath(ctx, u.ID, "/plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.KeyUUID != nil {
		t.Errorf("plain storage key_uuid = %x, want nil", f.KeyUUID)
	}
}
