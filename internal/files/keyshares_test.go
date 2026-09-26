package files_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// keyShareEnv wires the full ADR-0098 stack over a real database the way
// app.New does in per-user mode: a v3-sealing encrypt FS with the SQL
// resolver, the filecache, the share store, and one KeySharer feeding every
// hook (DAV write path, sharing service, users membership hook).
type keyShareEnv struct {
	db     database.DB
	dav    *files.DAV
	meta   *files.SQLStore
	users  *users.SQLStore
	shares *sharing.SQLShareStore
	keys   *files.KeySharer
	svc    *sharing.Service
	ids    map[string]int64
}

func newKeyShareEnv(t *testing.T, uids ...string) *keyShareEnv {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	us := users.NewSQLStore(db)
	ids := map[string]int64{}
	for _, uid := range uids {
		u := &users.User{UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true}
		if err := us.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		ids[uid] = u.ID
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
	meta := files.NewSQLStore(db)
	dav := files.NewDAV(fs, meta, us)
	shareStore := sharing.NewSQLShareStore(db)
	dav.Shares = shareStore
	// Trash and versions wired the way production does (ADR-0097's DAV
	// tests): version snapshots re-seal under their own fresh key UUIDs.
	dav.Trash = files.NewTrash(fs, files.NewSQLTrashStore(db), dav, us)
	dav.Versions = files.NewVersions(fs, files.NewSQLVersionStore(db), dav, us)
	ks := &files.KeySharer{Meta: meta, Wrapper: res, Shares: shareStore, Users: us, Logger: slog.New(slog.DiscardHandler)}
	dav.KeySharer = ks
	dav.Logger = slog.New(slog.DiscardHandler)
	us.MemberKeys = ks
	us.Logger = slog.New(slog.DiscardHandler)
	svc := &sharing.Service{
		Store:  shareStore,
		Files:  dav,
		Users:  us,
		Keys:   ks,
		Logger: slog.New(slog.DiscardHandler),
	}
	return &keyShareEnv{db: db, dav: dav, meta: meta, users: us, shares: shareStore, keys: ks, svc: svc, ids: ids}
}

func (e *keyShareEnv) write(t *testing.T, p, content string) {
	t.Helper()
	if _, _, err := e.dav.Write(context.Background(), "alice", p, bytes.NewReader([]byte(content)), nil); err != nil {
		t.Fatal(err)
	}
}

func (e *keyShareEnv) mkdir(t *testing.T, p string) {
	t.Helper()
	if _, err := e.dav.Mkdir(context.Background(), "alice", p); err != nil {
		t.Fatal(err)
	}
}

// wrapRows counts the recipient's file_keys rows for the file at path (or
// for a key UUID directly).
func (e *keyShareEnv) wrapRows(t *testing.T, path, recipientUID string) int64 {
	t.Helper()
	ctx := context.Background()
	f, err := e.meta.GetByPath(ctx, e.ids["alice"], path)
	if err != nil {
		t.Fatal(err)
	}
	return e.wrapRowsForUUID(t, f.KeyUUID, recipientUID)
}

func (e *keyShareEnv) wrapRowsForUUID(t *testing.T, keyUUID []byte, recipientUID string) int64 {
	t.Helper()
	ctx := context.Background()
	var n int64
	if len(keyUUID) == 0 {
		if err := e.db.QueryRow(ctx, `
SELECT COUNT(*) FROM file_keys WHERE key_uuid IS NULL AND user_id = ?`, e.ids[recipientUID]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := e.db.QueryRow(ctx, `
SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`, keyUUID, e.ids[recipientUID]).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *keyShareEnv) keyUUID(t *testing.T, path string) []byte {
	t.Helper()
	f, err := e.meta.GetByPath(context.Background(), e.ids["alice"], path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.KeyUUID) != 16 {
		t.Fatalf("%s: key_uuid = %d bytes, want 16 (per-user mode writes v3)", path, len(f.KeyUUID))
	}
	return f.KeyUUID
}

func (e *keyShareEnv) share(t *testing.T, path string, shareType int, shareWith string) *files.Share {
	t.Helper()
	sh, err := e.svc.Create(context.Background(), "alice", path, shareType, 0, shareWith, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestKeyShareUserShareWrapAndUnshare(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice", "bob")
	env.write(t, "/a.txt", "v3 hello")

	sh := env.share(t, "/a.txt", files.ShareTypeUser, "bob")
	if got := env.wrapRows(t, "/a.txt", "bob"); got != 1 {
		t.Fatalf("recipient rows after grant = %d, want 1", got)
	}
	if got := env.wrapRows(t, "/a.txt", "alice"); got != 1 {
		t.Fatalf("owner rows after grant = %d, want 1", got)
	}

	// A link share wraps nothing: no recipient user key exists.
	link := env.share(t, "/a.txt", files.ShareTypeLink, "")
	if got := env.wrapRows(t, "/a.txt", "bob"); got != 1 {
		t.Fatalf("recipient rows after link share = %d, want 1 (links never wrap)", got)
	}
	if err := env.svc.Delete(ctx, "alice", link.ID); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.wrapRows(t, "/a.txt", "bob"); got != 0 {
		t.Errorf("recipient rows after unshare = %d, want 0", got)
	}
	if got := env.wrapRows(t, "/a.txt", "alice"); got != 1 {
		t.Errorf("owner rows after unshare = %d, want 1 (owner row survives)", got)
	}
}

func TestKeyShareFolderShareWrapsSealedSubtree(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice", "bob")
	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.write(t, "/docs/a.txt", "one")
	env.write(t, "/docs/sub/b.txt", "two")
	env.write(t, "/docs/legacy.txt", "v1 payload")
	// Simulate a v1/v2 file: no v3 key UUID on the row.
	if _, err := env.db.Exec(ctx, `UPDATE files SET key_uuid = NULL WHERE path = '/docs/legacy.txt'`); err != nil {
		t.Fatal(err)
	}

	sh := env.share(t, "/docs", files.ShareTypeUser, "bob")
	for _, p := range []string{"/docs/a.txt", "/docs/sub/b.txt"} {
		if got := env.wrapRows(t, p, "bob"); got != 1 {
			t.Errorf("%s: recipient rows = %d, want 1", p, got)
		}
	}
	if got := env.wrapRowsForUUID(t, nil, "bob"); got != 0 {
		t.Errorf("legacy v1/v2 file wrapped: %d rows, want 0 (NULL key_uuid skipped)", got)
	}

	if err := env.svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/docs/a.txt", "/docs/sub/b.txt"} {
		if got := env.wrapRows(t, p, "bob"); got != 0 {
			t.Errorf("%s: recipient rows after unshare = %d, want 0", p, got)
		}
		if got := env.wrapRows(t, p, "alice"); got != 1 {
			t.Errorf("%s: owner rows after unshare = %d, want 1", p, got)
		}
	}
}

func TestKeyShareWriteIntoSharedFolderAutoWraps(t *testing.T) {
	env := newKeyShareEnv(t, "alice", "bob")
	env.mkdir(t, "/docs")
	env.share(t, "/docs", files.ShareTypeUser, "bob")

	// The write-path hook wraps the fresh key for the covering share's
	// recipient without any explicit grant call.
	env.write(t, "/docs/new.txt", "fresh")
	if got := env.wrapRows(t, "/docs/new.txt", "bob"); got != 1 {
		t.Fatalf("recipient rows for a write into a shared folder = %d, want 1", got)
	}
}

func TestKeyShareOverwriteCarriesRecipients(t *testing.T) {
	env := newKeyShareEnv(t, "alice", "bob")
	env.mkdir(t, "/docs")
	env.write(t, "/docs/a.txt", "old content")
	env.share(t, "/docs", files.ShareTypeUser, "bob")
	oldUUID := env.keyUUID(t, "/docs/a.txt")
	if got := env.wrapRowsForUUID(t, oldUUID, "bob"); got != 1 {
		t.Fatalf("recipient rows before overwrite = %d, want 1", got)
	}

	env.write(t, "/docs/a.txt", "new content")
	newUUID := env.keyUUID(t, "/docs/a.txt")
	if bytes.Equal(oldUUID, newUUID) {
		t.Fatal("overwrite must mint a fresh key UUID")
	}
	if got := env.wrapRowsForUUID(t, newUUID, "bob"); got != 1 {
		t.Errorf("recipient rows on the new key = %d, want 1 (carried)", got)
	}
	if got := env.wrapRowsForUUID(t, oldUUID, "bob"); got != 0 {
		t.Errorf("recipient rows on the old key = %d, want 0", got)
	}
	var oldTotal int64
	if err := env.db.QueryRow(context.Background(), `
SELECT COUNT(*) FROM file_keys WHERE key_uuid = ?`, oldUUID).Scan(&oldTotal); err != nil {
		t.Fatal(err)
	}
	if oldTotal != 0 {
		t.Errorf("old-key rows = %d, want 0 (owner row moved with the overwrite)", oldTotal)
	}
	if got := env.wrapRowsForUUID(t, newUUID, "alice"); got != 1 {
		t.Errorf("owner rows on the new key = %d, want 1", got)
	}

	// The version snapshot taken during the overwrite re-sealed under its
	// own fresh key UUID (copyToVersion goes through the encrypt FS), so
	// deleting the old key's rows strands nothing: the snapshot still
	// resolves and reads back.
	st, err := env.dav.Stat(context.Background(), "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := env.dav.Versions.List(context.Background(), "alice", "/versions/"+strconv.FormatUint(st.NumericID, 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d, want 1 (snapshot of the first write)", len(versions))
	}
	rc, _, err := env.dav.Versions.Read(context.Background(), "alice", "/versions/"+strconv.FormatUint(st.NumericID, 10)+versions[0].Path)
	if err != nil {
		t.Fatalf("version read after old-key row deletion: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "old content" {
		t.Errorf("version content = %q, want %q", body, "old content")
	}
}

func TestKeyShareGroupMembershipChurn(t *testing.T) {
	env := newKeyShareEnv(t, "alice", "bob", "carol", "dave")
	ctx := context.Background()
	if err := env.users.CreateGroup(ctx, &users.Group{GID: "g1", DisplayName: "Group 1"}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"bob", "carol"} {
		if err := env.users.AddGroupMember(ctx, "g1", uid); err != nil {
			t.Fatal(err)
		}
	}
	env.mkdir(t, "/docs")
	env.write(t, "/docs/a.txt", "team file")

	env.share(t, "/docs", files.ShareTypeGroup, "g1")
	for _, uid := range []string{"bob", "carol"} {
		if got := env.wrapRows(t, "/docs/a.txt", uid); got != 1 {
			t.Errorf("%s: rows after group grant = %d, want 1", uid, got)
		}
	}

	// Member join wraps; member leave unwraps; the rest are untouched.
	if err := env.users.AddGroupMember(ctx, "g1", "dave"); err != nil {
		t.Fatal(err)
	}
	if got := env.wrapRows(t, "/docs/a.txt", "dave"); got != 1 {
		t.Errorf("dave: rows after join = %d, want 1", got)
	}
	if err := env.users.RemoveGroupMember(ctx, "g1", "bob"); err != nil {
		t.Fatal(err)
	}
	if got := env.wrapRows(t, "/docs/a.txt", "bob"); got != 0 {
		t.Errorf("bob: rows after leave = %d, want 0", got)
	}
	for _, uid := range []string{"carol", "dave"} {
		if got := env.wrapRows(t, "/docs/a.txt", uid); got != 1 {
			t.Errorf("%s: rows after bob's leave = %d, want 1", uid, got)
		}
	}
}

func TestKeyShareExpireJobUnwraps(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice", "bob")
	env.write(t, "/a.txt", "ephemeral")
	uuid := env.keyUUID(t, "/a.txt")

	// An already-expired user share, wrapped as the grant would have.
	past := time.Now().UTC().Add(-time.Hour).UnixMilli()
	sh := &files.Share{
		OwnerUserID: env.ids["alice"], ShareType: files.ShareTypeUser, Path: "/a.txt",
		ItemType: "file", Token: "expirekeywrap01", Permissions: 1,
		ExpireMs: past, StimeMs: past, ShareWith: "bob",
	}
	if err := env.shares.Insert(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if err := env.keys.WrapForShare(ctx, sh); err != nil {
		t.Fatal(err)
	}
	if got := env.wrapRowsForUUID(t, uuid, "bob"); got != 1 {
		t.Fatalf("recipient rows before expire = %d, want 1", got)
	}

	job := sharing.NewExpireJob(env.shares, time.Now, nil, nil, env.keys)
	if err := job.Run(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := env.wrapRowsForUUID(t, uuid, "bob"); got != 0 {
		t.Errorf("recipient rows after expire sweep = %d, want 0", got)
	}
	if got := env.wrapRowsForUUID(t, uuid, "alice"); got != 1 {
		t.Errorf("owner rows after expire sweep = %d, want 1", got)
	}
}

// TestKeyShareWriteUnshareRace pins the ADR-0096 phase-2 concurrency
// requirement: a write into a shared folder racing the folder's unshare
// must leave no error on either side, and the file's recipient wrap row may
// exist if and only if the share still exists. The DAV write path holds the
// ADR-0094 stripe lock, but the unshare never takes it — WrapForWrite's
// reconciliation re-read is what keeps the row set consistent with the
// share table for any interleaving.
func TestKeyShareWriteUnshareRace(t *testing.T) {
	const iterations = 16
	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("unshare/%d", i), func(t *testing.T) {
			env := newKeyShareEnv(t, "alice", "bob")
			env.mkdir(t, "/docs")
			sh := env.share(t, "/docs", files.ShareTypeUser, "bob")

			var wg sync.WaitGroup
			errs := make(chan error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _, err := env.dav.Write(context.Background(), "alice", "/docs/race.txt", bytes.NewReader([]byte("racing")), nil)
				errs <- err
			}()
			go func() {
				defer wg.Done()
				errs <- env.svc.Delete(context.Background(), "alice", sh.ID)
			}()
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("racing op: %v", err)
				}
			}

			// The share is gone: the invariant leaves no recipient row.
			if _, err := env.shares.GetByID(context.Background(), sh.ID); err == nil {
				t.Fatal("share still present after unshare")
			}
			if got := env.wrapRows(t, "/docs/race.txt", "bob"); got != 0 {
				t.Errorf("recipient rows with the share gone = %d, want 0", got)
			}
		})
	}

	// The other direction: a concurrent unrelated unshare must not touch
	// the live share's wraps — the row exists because the share exists.
	t.Run("unrelated-unshare-keeps-row", func(t *testing.T) {
		env := newKeyShareEnv(t, "alice", "bob", "carol")
		env.mkdir(t, "/docs")
		env.mkdir(t, "/other")
		env.share(t, "/docs", files.ShareTypeUser, "bob")
		other := env.share(t, "/other", files.ShareTypeUser, "carol")

		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _, err := env.dav.Write(context.Background(), "alice", "/docs/keep.txt", bytes.NewReader([]byte("kept")), nil)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			errs <- env.svc.Delete(context.Background(), "alice", other.ID)
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("racing op: %v", err)
			}
		}
		if got := env.wrapRows(t, "/docs/keep.txt", "bob"); got != 1 {
			t.Errorf("recipient rows with the share live = %d, want 1", got)
		}
	})
}
