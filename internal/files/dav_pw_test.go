package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// pwEnv mirrors keyShareEnv with the resolver's ADR-0100 enrollment enabled,
// so tests can enroll users via UnlockForLogin and drive the identity read
// path through real DAV requests.
type pwEnv struct {
	keyShareEnv
	res *encrypt.SQLResolver
}

var pwTestKDF = encrypt.KeyDerivationParams{MemoryKB: 1024, Iterations: 1, Parallelism: 1}

func newPWEnv(t *testing.T, uids ...string) *pwEnv {
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
	res.PasswordWrapped = true
	res.KDF = pwTestKDF
	fs, err := encrypt.NewWithResolver(key, nil, inner, res)
	if err != nil {
		t.Fatal(err)
	}
	meta := files.NewSQLStore(db)
	dav := files.NewDAV(fs, meta, us)
	shareStore := sharing.NewSQLShareStore(db)
	dav.Shares = shareStore
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
	return &pwEnv{
		keyShareEnv: keyShareEnv{db: db, dav: dav, meta: meta, users: us, shares: shareStore, keys: ks, svc: svc, ids: ids},
		res:         res,
	}
}

func (e *pwEnv) login(t *testing.T, uid, password string) []byte {
	t.Helper()
	priv, err := e.res.UnlockForLogin(context.Background(), uid, password)
	if err != nil {
		t.Fatal(err)
	}
	if len(priv) != 32 {
		t.Fatalf("%s unlocked %d bytes, want 32", uid, len(priv))
	}
	return priv
}

// pctx carries uid's principal with an unlocked key, as the auth middleware
// attaches it (nil key = app-password/session-less request).
func pctx(uid string, key []byte) context.Context {
	return auth.WithUser(context.Background(), &auth.Principal{UID: uid, Enabled: true, UnlockedKey: key})
}

// stubIncomingFeed serves a fixed mount table (mirrors the internal
// incoming_test stub for this external test package).
type stubIncomingFeed struct {
	mounts []files.IncomingMount
}

func (s stubIncomingFeed) ListIncoming(context.Context, string) ([]files.IncomingMount, error) {
	return s.mounts, nil
}

func (e *pwEnv) readAs(t *testing.T, ctx context.Context, user string) (string, error) {
	t.Helper()
	rc, _, err := e.dav.Read(ctx, user, "/a.txt")
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TestIncomingOverwriteEnrolledOwnerCarriesWraps pins the ADR-0101 ordering
// fix: an enrolled recipient overwriting a file in an enrolled owner's tree
// must carry the wrap rows to the fresh key — the writer's ctx cannot
// Resolve the fresh owner-boxed key, so the write threads the FK from
// Allocate. Both parties keep reading afterwards.
func TestIncomingOverwriteEnrolledOwnerCarriesWraps(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice", "bob")
	env.write(t, "/a.txt", "v3 hello")
	oldUUID := env.keyUUID(t, "/a.txt")
	env.share(t, "/a.txt", files.ShareTypeUser, "bob")
	env.dav.Incoming = stubIncomingFeed{mounts: []files.IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/a.txt", Mount: "/a.txt",
		Permissions: webdav.PermRead | webdav.PermUpdate, ItemType: "file",
	}}}

	// Both enroll: alice's owner row and bob's recipient row become boxes.
	aliceKey := env.login(t, "alice", "alice-pw")
	bobKey := env.login(t, "bob", "bob-pw")
	if got, err := env.readAs(t, pctx("bob", bobKey), "bob"); got != "v3 hello" || err != nil {
		t.Fatalf("bob pre-overwrite read = %q, %v", got, err)
	}

	// bob overwrites through the incoming mount: the write remaps into
	// alice's tree, boxing the fresh FK for her (public key, no session) and
	// carrying bob's wrap across — via the threaded FK, not a Resolve.
	if _, _, err := env.dav.Write(pctx("bob", bobKey), "bob", "/a.txt", strings.NewReader("bye"), nil); err != nil {
		t.Fatal(err)
	}
	newUUID := env.keyUUID(t, "/a.txt")
	if bytes.Equal(oldUUID, newUUID) {
		t.Fatal("overwrite did not mint a fresh key UUID")
	}

	// Old rows carried: nothing remains under the superseded UUID, and the
	// fresh UUID holds scheme=1 rows for both.
	var oldRows int64
	if err := env.db.QueryRow(ctx, `SELECT COUNT(*) FROM file_keys WHERE key_uuid = ?`, oldUUID).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 {
		t.Errorf("old key uuid rows = %d, want 0 (carried and deleted)", oldRows)
	}
	for uid, want := range map[string]int64{"alice": 1, "bob": 1} {
		var n, scheme int64
		if err := env.db.QueryRow(ctx, `
SELECT COUNT(*), COALESCE(MAX(scheme), -1) FROM file_keys WHERE key_uuid = ? AND user_id = ?`,
			newUUID, env.ids[uid]).Scan(&n, &scheme); err != nil {
			t.Fatal(err)
		}
		if n != want || scheme != 1 {
			t.Errorf("%s: rows = %d scheme = %d, want 1 row at scheme 1", uid, n, scheme)
		}
	}

	// Both can still read — the pin's exact assertion.
	if got, err := env.readAs(t, pctx("alice", aliceKey), "alice"); err != nil || got != "bye" {
		t.Errorf("alice read after overwrite = %q, %v", got, err)
	}
	if got, err := env.readAs(t, pctx("bob", bobKey), "bob"); err != nil || got != "bye" {
		t.Errorf("bob read after overwrite = %q, %v", got, err)
	}
	// And without a key the file is locked, not corrupt.
	if _, err := env.readAs(t, pctx("bob", nil), "bob"); !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Errorf("keyless read err = %v, want ErrKeyLocked", err)
	}
}

// TestEnlockedWebDAVLockedGets403 pins the ADR-0101 WebDAV mapping at the
// HTTP boundary: an app-password principal (no unlocked key) GETting an
// enrolled owner's v3 file gets 403; the same GET with an unlocked session
// gets 200.
func TestEnrolledWebDAVLockedGets403(t *testing.T) {
	env := newPWEnv(t, "alice")
	env.write(t, "/a.txt", "v3 hello")
	aliceKey := env.login(t, "alice", "alice-pw")

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	get := func(principal *auth.Principal) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/remote.php/dav/files/alice/a.txt", nil)
		req = req.WithContext(auth.WithUser(req.Context(), principal))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// App-password principal: authenticated, but no unlocked key.
	rr := get(&auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodAppPassword})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("keyless GET = %d, want 403", rr.Code)
	}

	// Unlocked session: 200 with the content.
	rr = get(&auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: aliceKey})
	if rr.Code != http.StatusOK {
		t.Fatalf("unlocked GET = %d, want 200", rr.Code)
	}
	if got := rr.Body.String(); got != "v3 hello" {
		t.Errorf("unlocked GET body = %q", got)
	}
}

// TestPublicLinkEnrolledFileLocked pins the ADR-0100 accepted limitation: a
// public-link download of an enrolled owner's v3 file is a 403 lock (the
// dual-matched error), never corruption.
func TestPublicLinkEnrolledFileLocked(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice")
	env.write(t, "/a.txt", "v3 hello")
	env.login(t, "alice", "alice-pw")

	sh := &files.Share{ID: 1, OwnerUserID: env.ids["alice"], ShareType: files.ShareTypeLink, Path: "/a.txt", ItemType: "file", Permissions: webdav.PermRead}
	pub := &files.PublicDAV{
		Files: env.dav,
		Resolve: func(context.Context, string) (*files.Share, *users.User, error) {
			return sh, &users.User{ID: env.ids["alice"], UID: "alice", Enabled: true}, nil
		},
	}
	// Anonymous ctx (no principal — a link download carries no session).
	_, _, err := pub.Read(ctx, "token", "/")
	if !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("public read err = %v, want webdav.ErrForbidden (403)", err)
	}
	if !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Errorf("public read err = %v, want errors.Is ErrKeyLocked (not corruption)", err)
	}
}

// TestPublicLinkUnenrolledFileReads is the control: an unenrolled owner's
// link download keeps working anonymously (phases 1–3 behavior).
func TestPublicLinkUnenrolledFileReads(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "carol")
	if _, _, err := env.dav.Write(ctx, "carol", "/b.txt", strings.NewReader("carol hello"), nil); err != nil {
		t.Fatal(err)
	}
	sh := &files.Share{ID: 2, OwnerUserID: env.ids["carol"], ShareType: files.ShareTypeLink, Path: "/b.txt", ItemType: "file", Permissions: webdav.PermRead}
	pub := &files.PublicDAV{
		Files: env.dav,
		Resolve: func(context.Context, string) (*files.Share, *users.User, error) {
			return sh, &users.User{ID: env.ids["carol"], UID: "carol", Enabled: true}, nil
		},
	}
	rc, _, err := pub.Read(ctx, "token", "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if string(body) != "carol hello" {
		t.Errorf("unenrolled public read = %q", body)
	}
}
