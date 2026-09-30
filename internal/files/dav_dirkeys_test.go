package files_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
)

// stubDirKeyMinter counts AllocateForUser calls and returns distinct key
// UUIDs without touching the database — the flag-on wiring check without
// the resolver's side effects.
type stubDirKeyMinter struct {
	calls int
	uids  []string
}

func (s *stubDirKeyMinter) AllocateForUser(_ context.Context, uid string) ([16]byte, []byte, error) {
	s.calls++
	s.uids = append(s.uids, uid)
	var uuid [16]byte
	binary.BigEndian.PutUint64(uuid[8:], uint64(s.calls))
	return uuid, make([]byte, 32), nil
}

// folderKeyUUID returns the folder row's 16-byte key UUID.
func (e *keyShareEnv) folderKeyUUID(t *testing.T, p string) []byte {
	t.Helper()
	f, err := e.meta.GetByPath(context.Background(), e.ids["alice"], p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.KeyUUID) != 16 {
		t.Fatalf("%s: key_uuid = %d bytes, want 16 (folder DK minted)", p, len(f.KeyUUID))
	}
	return f.KeyUUID
}

func (e *keyShareEnv) folderKeyUUIDNil(t *testing.T, p string) {
	t.Helper()
	f, err := e.meta.GetByPath(context.Background(), e.ids["alice"], p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.KeyUUID) != 0 {
		t.Errorf("%s: key_uuid = %x, want NULL (flag off)", p, f.KeyUUID)
	}
}

// TestDirKeysFlagOffBitIdentical pins the ADR-0104 rollback carve-out: with
// no DirKeyMinter wired (the flag off), no directory row ever carries a key
// UUID and no directory key wrap is minted — writes stay bit-identical.
func TestDirKeysFlagOffBitIdentical(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	if env.dav.DirKeys != nil {
		t.Fatal("DirKeys must be nil by default")
	}
	wraps := func() int64 {
		t.Helper()
		var n int64
		if err := env.db.QueryRow(ctx, `SELECT COUNT(*) FROM file_keys`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	if _, _, err := env.dav.Copy(ctx, "alice", "/docs", "alice", "/copy", false, true); err != nil {
		t.Fatal(err)
	}
	// Trash restore funnels through ingestFromStorage: covered too.
	if err := env.dav.Remove(ctx, "alice", "/copy/sub"); err != nil {
		t.Fatal(err)
	}
	ents, err := env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	loc := strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "", false); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/", "/docs", "/docs/sub", "/copy", "/copy/sub"} {
		env.folderKeyUUIDNil(t, p)
	}
	// Per-user keys are on in this env: file writes wrap FKs, but not a
	// single directory key was minted.
	if n := wraps(); n != 0 {
		t.Errorf("file_keys rows = %d, want 0 (no file written, no DK minted)", n)
	}
}

// TestDirKeysMkdirMintsAndResolves is the end-to-end pin over a real
// SQLResolver: an MKCOL with the flag on writes the folder row with its
// key_uuid, the wrap row exists, and Resolve round-trips the DK. The user's
// root gains its DK lazily at the first request.
func TestDirKeysMkdirMintsAndResolves(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.dav.DirKeys = env.res

	env.mkdir(t, "/docs")
	dkUUID := env.folderKeyUUID(t, "/docs")
	if got := env.wrapRowsForUUID(t, dkUUID, "alice"); got != 1 {
		t.Fatalf("owner wrap rows for the folder DK = %d, want 1", got)
	}
	dk, err := env.res.Resolve(ctx, [16]byte(dkUUID))
	if err != nil {
		t.Fatal(err)
	}
	if len(dk) != 32 {
		t.Errorf("resolved DK = %d bytes, want 32", len(dk))
	}

	// resolveUser ran for the mkdir: the root carries its lazily-minted DK.
	rootUUID := env.folderKeyUUID(t, "/")
	if got := env.wrapRowsForUUID(t, rootUUID, "alice"); got != 1 {
		t.Errorf("owner wrap rows for the root DK = %d, want 1", got)
	}
	if _, err := env.res.Resolve(ctx, [16]byte(rootUUID)); err != nil {
		t.Errorf("root DK does not resolve: %v", err)
	}
}

// TestDirKeysMintCallSites pins the mint call sites with a counting stub:
// the root lazily (once), each created folder exactly once — MKCOL, and
// Copy's per-folder inserts (copyOne routes through Mkdir).
func TestDirKeysMintCallSites(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	stub := &stubDirKeyMinter{}
	env.dav.DirKeys = stub

	env.mkdir(t, "/docs")     // root (lazy) + /docs
	env.mkdir(t, "/docs/sub") // root already claimed
	if _, _, err := env.dav.Copy(ctx, "alice", "/docs", "alice", "/copy", false, true); err != nil {
		t.Fatal(err)
	} // /copy + /copy/sub
	if stub.calls != 5 {
		t.Fatalf("mint calls = %d, want 5 (root, /docs, /docs/sub, /copy, /copy/sub)", stub.calls)
	}
	for _, uid := range stub.uids {
		if uid != "alice" {
			t.Errorf("minted for uid %q, want alice", uid)
		}
	}
}

// TestDirKeysCopyMintsFreshKeys pins the copy rule (ADR-0104 §5): a copied
// folder mints a fresh DK rather than carrying the source's.
func TestDirKeysCopyMintsFreshKeys(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.dav.DirKeys = env.res

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	srcUUID := env.folderKeyUUID(t, "/docs/sub")
	if _, _, err := env.dav.Copy(ctx, "alice", "/docs", "alice", "/copy", false, true); err != nil {
		t.Fatal(err)
	}
	dstUUID := env.folderKeyUUID(t, "/copy/sub")
	if bytes.Equal(srcUUID, dstUUID) {
		t.Error("copied folder kept the source's key UUID, want a fresh DK")
	}
	if got := env.wrapRowsForUUID(t, dstUUID, "alice"); got != 1 {
		t.Errorf("owner wrap rows for the copied folder DK = %d, want 1", got)
	}
}

// TestDirKeysTrashRestoreMints pins the restore funnel: Trash.Restore runs
// through ingestFromStorage, so a restored folder row gains a fresh DK with
// no extra code in the trash path.
func TestDirKeysTrashRestoreMints(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.dav.DirKeys = env.res

	env.mkdir(t, "/docs")
	oldUUID := env.folderKeyUUID(t, "/docs")
	if err := env.dav.Remove(ctx, "alice", "/docs"); err != nil {
		t.Fatal(err)
	}
	ents, err := env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	loc := strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	newUUID := env.folderKeyUUID(t, "/docs")
	if bytes.Equal(oldUUID, newUUID) {
		t.Error("restored folder kept its pre-trash key UUID, want a fresh DK")
	}
	if got := env.wrapRowsForUUID(t, newUUID, "alice"); got != 1 {
		t.Errorf("owner wrap rows for the restored folder DK = %d, want 1", got)
	}
	if _, err := env.res.Resolve(ctx, [16]byte(newUUID)); err != nil {
		t.Errorf("restored folder DK does not resolve: %v", err)
	}
}

// TestDirKeysMkdirUnderShareWrapsSharee pins folder wrap-on-write (ADR-0104
// phase 1, moved from phase 3): a folder created under a covering share
// wraps its fresh DK for the sharee, closing the availability gap for names
// symmetrically with content.
func TestDirKeysMkdirUnderShareWrapsSharee(t *testing.T) {
	env := newKeyShareEnv(t, "alice", "bob")
	env.dav.DirKeys = env.res

	env.mkdir(t, "/docs")
	env.share(t, "/docs", files.ShareTypeUser, "bob")
	// The grant itself wraps the shared folder's DK (KeySharer folder
	// coverage): bob holds a wrap of /docs's DK.
	docsUUID := env.folderKeyUUID(t, "/docs")
	if got := env.wrapRowsForUUID(t, docsUUID, "bob"); got != 1 {
		t.Fatalf("sharee wrap rows for the shared folder DK = %d, want 1", got)
	}

	// A new folder under the share: wrap-on-write covers its DK too.
	env.mkdir(t, "/docs/sub")
	subUUID := env.folderKeyUUID(t, "/docs/sub")
	if got := env.wrapRowsForUUID(t, subUUID, "bob"); got != 1 {
		t.Errorf("sharee wrap rows for the new subfolder DK = %d, want 1", got)
	}
}

// TestDirKeysRestoreUnderShareWrapsSharee pins folder wrap-on-restore
// (ADR-0104 phase-2 gap closed): a folder restored under a covering share
// wraps its freshly minted DK for the sharee — without the hook the
// restored tree's names stayed unresolvable to them.
func TestDirKeysRestoreUnderShareWrapsSharee(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice", "bob")
	env.dav.DirKeys = env.res

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.share(t, "/docs", files.ShareTypeUser, "bob")

	if err := env.dav.Remove(ctx, "alice", "/docs/sub"); err != nil {
		t.Fatal(err)
	}
	ents, err := env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	loc := strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	subUUID := env.folderKeyUUID(t, "/docs/sub")
	for uid, want := range map[string]int64{"alice": 1, "bob": 1} {
		if got := env.wrapRowsForUUID(t, subUUID, uid); got != want {
			t.Errorf("%s wrap rows for the restored subfolder DK = %d, want %d", uid, got, want)
		}
	}
}

// TestDirKeysEnrolledOwnerMkdir pins the enrolled path: the folder DK is a
// scheme=1 box under the owner's public key (no session, no symmetric UK),
// resolving through the reader's identity ctx like any enrolled key.
func TestDirKeysEnrolledOwnerMkdir(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice")
	env.dav.DirKeys = env.res
	aliceKey := env.login(t, "alice", "alice-pw")

	env.mkdir(t, "/docs")
	dkUUID := env.folderKeyUUID(t, "/docs")
	var scheme int64
	if err := env.db.QueryRow(ctx, `
SELECT scheme FROM file_keys WHERE key_uuid = ? AND user_id = ?`, dkUUID, env.ids["alice"]).Scan(&scheme); err != nil {
		t.Fatal(err)
	}
	if scheme != 1 {
		t.Errorf("folder DK wrap scheme = %d, want 1 (box)", scheme)
	}
	dk, err := env.res.Resolve(pctx("alice", aliceKey), [16]byte(dkUUID))
	if err != nil {
		t.Fatalf("resolve folder DK via identity ctx: %v", err)
	}
	if len(dk) != 32 {
		t.Errorf("resolved DK = %d bytes, want 32", len(dk))
	}
	// The lazily-minted root DK is a box row too.
	rootUUID := env.folderKeyUUID(t, "/")
	var rootScheme int64
	if err := env.db.QueryRow(ctx, `
SELECT scheme FROM file_keys WHERE key_uuid = ? AND user_id = ?`, rootUUID, env.ids["alice"]).Scan(&rootScheme); err != nil {
		t.Fatal(err)
	}
	if rootScheme != 1 {
		t.Errorf("root DK wrap scheme = %d, want 1 (box)", rootScheme)
	}
}
