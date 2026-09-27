package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// nameE2EEnv is keyShareEnv upgraded the way app.New wires ADR-0104 phase 2
// with encryption.filename_encryption on: one NameTranslator over the raw
// store, TranslatingStore as DAV.Meta and KeySharer.Meta, and the lock,
// trash, and version satellite wrappers. Users are flipped to scheme 1
// individually by encryptUser (the server creation hook / phase-4 sweep do
// this in production).
type nameE2EEnv struct {
	*keyShareEnv
	tmeta *files.TranslatingStore
}

func upgradeNameCrypt(t *testing.T, env *keyShareEnv, res *encrypt.SQLResolver) *nameE2EEnv {
	t.Helper()
	xlate := files.NewNameTranslator(env.meta, res, env.users)
	tmeta := files.NewTranslatingStore(env.meta, xlate)
	env.dav.Meta = tmeta
	env.dav.DirKeys = res
	env.dav.Locks = files.NewTranslatingLockStore(files.NewSQLLockStore(env.db), xlate)
	env.keys.Meta = tmeta
	env.dav.Trash = files.NewTrash(env.dav.Storage, files.NewTranslatingTrashStore(files.NewSQLTrashStore(env.db), xlate), env.dav, env.users)
	env.dav.Versions = files.NewVersions(env.dav.Storage, files.NewTranslatingVersionStore(files.NewSQLVersionStore(env.db), xlate), env.dav, env.users)
	return &nameE2EEnv{keyShareEnv: env, tmeta: tmeta}
}

func newNameE2EEnv(t *testing.T, uids ...string) *nameE2EEnv {
	t.Helper()
	env := newKeyShareEnv(t, uids...)
	return upgradeNameCrypt(t, env, env.res)
}

func (e *nameE2EEnv) encryptUser(t *testing.T, uid string) {
	t.Helper()
	if err := e.users.SetNameScheme(context.Background(), uid, encrypt.NameSchemeNCGOFN1); err != nil {
		t.Fatal(err)
	}
}

// rawRow returns the ciphertext DB view of the row at a plaintext path of
// alice's, resolved with a principal-less background ctx (fine for
// master-wrapped users; enrolled users need rawRowAs with an unlocked ctx).
func (e *nameE2EEnv) rawRow(t *testing.T, plainPath string) *files.File {
	t.Helper()
	return e.rawRowAs(t, context.Background(), "alice", plainPath)
}

// rawRowAs is rawRow with the caller's ctx.
func (e *nameE2EEnv) rawRowAs(t *testing.T, ctx context.Context, uid, plainPath string) *files.File {
	t.Helper()
	f, err := e.dav.Meta.GetByPath(ctx, e.ids[uid], plainPath)
	if err != nil {
		t.Fatalf("lookup %s: %v", plainPath, err)
	}
	raw, err := e.meta.GetByID(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// decryptName opens a stored token under the given parent row's DK.
func (e *nameE2EEnv) decryptName(t *testing.T, parentRow *files.File, token string) string {
	t.Helper()
	if len(parentRow.KeyUUID) != 16 {
		t.Fatalf("parent %q key_uuid = %d bytes", parentRow.Path, len(parentRow.KeyUUID))
	}
	var uuid [16]byte
	copy(uuid[:], parentRow.KeyUUID)
	dk, err := e.res.Resolve(context.Background(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	nk, err := encrypt.DeriveNameKey(dk, uuid)
	if err != nil {
		t.Fatal(err)
	}
	name, err := encrypt.DecryptName(nk, uuid, token)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// TestNameCryptTreeLifecycle is the phase-2 end-to-end pin: a scheme-1 user's
// MKCOL/PUT land as ciphertext rows in the DB while every wire view (stat,
// list, read, PROPFIND) stays plaintext.
func TestNameCryptTreeLifecycle(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.write(t, "/docs/sub/hello.txt", "hello world")
	env.write(t, "/docs/a.txt", "aaa")

	// DB rows literally hold tokens.
	rows, err := env.db.Query(ctx, `
SELECT name, path, name_scheme, parent_id FROM files WHERE user_id = ? ORDER BY id`, env.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rowT struct {
		name, path string
		scheme     int
		hasParent  bool
	}
	var got []rowT
	for rows.Next() {
		var r rowT
		var parent any
		if err := rows.Scan(&r.name, &r.path, &r.scheme, &parent); err != nil {
			t.Fatal(err)
		}
		r.hasParent = parent != nil
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("rows = %+v, want root + 4", got)
	}
	if got[0].path != "/" || got[0].name != "" || got[0].scheme != 0 {
		t.Errorf("root row = %+v, want the literal never-encrypted root", got[0])
	}
	for i, r := range got[1:] {
		if r.scheme != encrypt.NameSchemeNCGOFN1 {
			t.Errorf("row %d scheme = %d, want NCGOFN1", i, r.scheme)
		}
		for _, plain := range []string{"docs", "sub", "hello", "a.txt"} {
			if strings.Contains(r.name, plain) || strings.Contains(r.path, plain) {
				t.Errorf("row %d leaks %q: name=%q path=%q", i, plain, r.name, r.path)
			}
		}
		if r.hasParent && !strings.HasPrefix(r.path, "/") {
			t.Errorf("row %d path = %q", i, r.path)
		}
	}

	// The wire speaks plaintext.
	st, err := env.dav.Stat(ctx, "alice", "/docs/sub/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Path != "/docs/sub/hello.txt" {
		t.Errorf("stat path = %q", st.Path)
	}
	ents, err := env.dav.List(ctx, "alice", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 || ents[0].Path != "/docs/a.txt" || ents[1].Path != "/docs/sub" {
		t.Fatalf("list = %+v", ents)
	}
	rc, _, err := env.dav.Read(ctx, "alice", "/docs/sub/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello world" {
		t.Fatalf("read = %q %v", body, err)
	}

	// PROPFIND at the HTTP boundary: plaintext hrefs, no token leakage.
	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, "PROPFIND", "/remote.php/dav/files/alice/", nil)
	req.Header.Set("Depth", "1")
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodBasic}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	bodyText := rr.Body.String()
	if !strings.Contains(bodyText, `<d:href>/remote.php/dav/files/alice/docs/</d:href>`) {
		t.Errorf("PROPFIND body missing the plaintext /docs href\n%s", bodyText)
	}
	// No stored token appears on the wire.
	for _, p := range []string{"/docs", "/docs/sub", "/docs/sub/hello.txt", "/docs/a.txt"} {
		row := env.rawRow(t, p)
		if strings.Contains(bodyText, row.Name) || strings.Contains(bodyText, strings.TrimPrefix(row.Path, "/")) {
			t.Errorf("PROPFIND body leaks token material of %s (%q)", p, row.Path)
		}
	}
}

// TestNameCryptMoveRenameCopy pins the token math over DAV (ADR-0104 §5):
// rename re-tokens the basename under the same parent key, move re-tokens
// under the destination parent's key, descendants keep their tokens, and copy
// re-keys AND re-tokens.
func TestNameCryptMoveRenameCopy(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/a")
	env.mkdir(t, "/a/b")
	env.write(t, "/a/b/f.txt", "data")
	env.mkdir(t, "/c")

	subBefore := env.rawRow(t, "/a/b")
	fBefore := env.rawRow(t, "/a/b/f.txt")

	// Same-parent rename: basename re-tokened under /a's (unchanged) DK; the
	// descendant token is untouched, its path string prefix-rewritten.
	if _, _, err := env.dav.Move(ctx, "alice", "/a/b", "alice", "/a/b2", false); err != nil {
		t.Fatal(err)
	}
	subRenamed := env.rawRow(t, "/a/b2")
	if subRenamed.ID != subBefore.ID {
		t.Fatal("rename changed the row id")
	}
	if subRenamed.Name == subBefore.Name {
		t.Error("rename kept the old token, want a re-tokened basename")
	}
	aRow := env.rawRow(t, "/a")
	if got := env.decryptName(t, aRow, subRenamed.Name); got != "b2" {
		t.Errorf("renamed token decrypts to %q, want b2", got)
	}
	fRenamed := env.rawRow(t, "/a/b2/f.txt")
	if fRenamed.Name != fBefore.Name {
		t.Errorf("descendant token changed on rename: %q → %q", fBefore.Name, fRenamed.Name)
	}
	if fRenamed.Path != subRenamed.Path+"/"+fBefore.Name {
		t.Errorf("descendant path = %q, want prefix rewrite", fRenamed.Path)
	}

	// Cross-parent move: re-tokened under /c's DK; descendant still stable.
	if _, _, err := env.dav.Move(ctx, "alice", "/a/b2", "alice", "/c/b2", false); err != nil {
		t.Fatal(err)
	}
	subMoved := env.rawRow(t, "/c/b2")
	if subMoved.Name == subRenamed.Name {
		t.Error("cross-parent move kept the token, want re-tokening under the destination parent")
	}
	cRow := env.rawRow(t, "/c")
	if got := env.decryptName(t, cRow, subMoved.Name); got != "b2" {
		t.Errorf("moved token decrypts to %q under the destination parent, want b2", got)
	}
	fMoved := env.rawRow(t, "/c/b2/f.txt")
	if fMoved.Name != fBefore.Name {
		t.Errorf("descendant token changed on move: %q → %q", fBefore.Name, fMoved.Name)
	}
	if fMoved.Path != subMoved.Path+"/"+fBefore.Name {
		t.Errorf("moved descendant path = %q", fMoved.Path)
	}
	if *fMoved.ParentID != subMoved.ID {
		t.Errorf("descendant parent_id = %v, want %d", fMoved.ParentID, subMoved.ID)
	}

	// Copy: fresh DKs (phase 1) AND fresh tokens (phase 2) — the copied
	// root's token is under the destination parent's DK.
	if _, _, err := env.dav.Copy(ctx, "alice", "/c/b2", "alice", "/a/bcopy", false, true); err != nil {
		t.Fatal(err)
	}
	copyRoot := env.rawRow(t, "/a/bcopy")
	if bytes.Equal(copyRoot.KeyUUID, subMoved.KeyUUID) {
		t.Error("copied folder kept the source DK, want a fresh one")
	}
	if got := env.decryptName(t, aRow, copyRoot.Name); got != "bcopy" {
		t.Errorf("copied root token decrypts to %q under the destination parent, want bcopy", got)
	}
	copySub := env.rawRow(t, "/a/bcopy/f.txt")
	if copySub.Name == fMoved.Name {
		t.Error("copied file kept the source token, want re-tokening under the fresh parent DK")
	}
	if got := env.decryptName(t, env.rawRow(t, "/a/bcopy"), copySub.Name); got != "f.txt" {
		t.Errorf("copied file token decrypts to %q, want f.txt", got)
	}
	rc, _, err := env.dav.Read(ctx, "alice", "/a/bcopy/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "data" {
		t.Errorf("copied content = %q %v", body, err)
	}
}

// TestNameCryptTrashRestore pins trash: ciphertext original_path/name at
// rest, plaintext on the wire, and restore re-tokening under the current
// parent DK.
func TestNameCryptTrashRestore(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.write(t, "/docs/victim.txt", "v1")
	before := env.rawRow(t, "/docs/victim.txt")

	if err := env.dav.Remove(ctx, "alice", "/docs/victim.txt"); err != nil {
		t.Fatal(err)
	}
	var storedPath, storedName string
	if err := env.db.QueryRow(ctx, `SELECT original_path, name FROM trash_items`).Scan(&storedPath, &storedName); err != nil {
		t.Fatal(err)
	}
	if storedPath != before.Path || storedName != before.Name {
		t.Errorf("trash row = %q %q, want the row's ciphertext %q %q", storedPath, storedName, before.Path, before.Name)
	}

	// The trash listing speaks plaintext.
	ents, err := env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	if ents[0].TrashOriginal != "docs/victim.txt" {
		t.Errorf("trash original = %q, want plaintext docs/victim.txt", ents[0].TrashOriginal)
	}

	// Default restore: same parent → the same deterministic token returns.
	loc := strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	restored := env.rawRow(t, "/docs/victim.txt")
	if restored.Name != before.Name {
		t.Errorf("restored token = %q, want %q (same parent DK)", restored.Name, before.Name)
	}

	// Restore to a different parent re-tokens under that parent's DK.
	if err := env.dav.Remove(ctx, "alice", "/docs/victim.txt"); err != nil {
		t.Fatal(err)
	}
	ents, err = env.dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list 2 = %v %v", ents, err)
	}
	loc = strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", loc, "alice", "/renamed.txt", false); err != nil {
		t.Fatal(err)
	}
	renamed := env.rawRow(t, "/renamed.txt")
	if renamed.Name == before.Name {
		t.Error("restore-to-elsewhere kept the token, want re-tokening under the new parent")
	}
	rootRow := env.rawRow(t, "/")
	if got := env.decryptName(t, rootRow, renamed.Name); got != "renamed.txt" {
		t.Errorf("restored token decrypts to %q, want renamed.txt", got)
	}
	rc, _, err := env.dav.Read(ctx, "alice", "/renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "v1" {
		t.Errorf("restored content = %q %v", body, err)
	}
}

// TestNameCryptVersions pins file_versions.file_path ciphertext at rest,
// plaintext at the boundary, and the rename pass rewriting the tokens.
func TestNameCryptVersions(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	env.write(t, "/v.txt", "one")
	env.write(t, "/v.txt", "two") // snapshots "one"

	st, err := env.dav.Stat(ctx, "alice", "/v.txt")
	if err != nil {
		t.Fatal(err)
	}
	versPath := "/versions/" + strconv.FormatUint(st.NumericID, 10)
	vers, err := env.dav.Versions.List(ctx, "alice", versPath)
	if err != nil || len(vers) != 1 {
		t.Fatalf("versions = %v %v", vers, err)
	}
	var storedPath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions`).Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	liveRow := env.rawRow(t, "/v.txt")
	if storedPath != liveRow.Path {
		t.Errorf("version row path = %q, want the file's ciphertext path %q", storedPath, liveRow.Path)
	}
	rc, _, err := env.dav.Versions.Read(ctx, "alice", versPath+vers[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "one" {
		t.Errorf("version content = %q %v", body, err)
	}

	// Rename propagates: the version path is rewritten to the new ciphertext.
	if _, _, err := env.dav.Move(ctx, "alice", "/v.txt", "alice", "/w.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions`).Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	movedRow := env.rawRow(t, "/w.txt")
	if storedPath != movedRow.Path {
		t.Errorf("version row after rename = %q, want %q", storedPath, movedRow.Path)
	}
	if vers, err := env.dav.Versions.List(ctx, "alice", versPath); err != nil || len(vers) != 1 {
		t.Errorf("versions after rename = %v %v", vers, err)
	}
}

// TestNameCryptLocks pins ciphertext file_locks.file_path with the plaintext
// echo in DAV lock responses, and lock paths following a rename.
func TestNameCryptLocks(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.write(t, "/docs/f.txt", "x")

	li, err := env.dav.Lock(ctx, "alice", "/docs/f.txt", webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if li.Path != "/docs/f.txt" {
		t.Errorf("lock info path = %q, want the plaintext request path", li.Path)
	}
	var storedPath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_locks`).Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	if want := env.rawRow(t, "/docs/f.txt").Path; storedPath != want {
		t.Errorf("lock row path = %q, want the ciphertext path %q", storedPath, want)
	}
	if _, err := env.dav.Lock(ctx, "alice", "/docs/f.txt", webdav.LockRequest{Owner: "bob", Timeout: 1800 * time.Second}); !errors.Is(err, webdav.ErrLocked) {
		t.Errorf("second lock = %v, want ErrLocked", err)
	}
	if err := env.dav.Unlock(ctx, "alice", "/docs/f.txt", li.Token); err != nil {
		t.Fatal(err)
	}

	// A lock follows the file's rename (both endpoints translated).
	li2, err := env.dav.Lock(ctx, "alice", "/docs/f.txt", webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Move(ctx, "alice", "/docs/f.txt", "alice", "/docs/g.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_locks`).Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	if want := env.rawRow(t, "/docs/g.txt").Path; storedPath != want {
		t.Errorf("lock row after rename = %q, want %q", storedPath, want)
	}
	if err := env.dav.Unlock(ctx, "alice", "/docs/g.txt", li2.Token); err != nil {
		t.Errorf("unlock at the renamed path: %v", err)
	}
}

// TestNameCryptKeySharerFolderGrant pins the KeySharer flowing through the
// translating store with PLAINTEXT share paths (no double-encryption — the
// shares table is phase-3 scope): a folder grant wraps both the folder DKs
// and the file FKs for the recipient.
func TestNameCryptKeySharerFolderGrant(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.write(t, "/docs/sub/f.txt", "content")

	sh := env.share(t, "/docs", files.ShareTypeUser, "bob")

	// shares.file_path stays plaintext until phase 3.
	var sharePath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM shares WHERE id = ?`, sh.ID).Scan(&sharePath); err != nil {
		t.Fatal(err)
	}
	if sharePath != "/docs" {
		t.Errorf("share path = %q, want plaintext /docs (no double-encryption)", sharePath)
	}

	// Bob holds wraps of both folder DKs and the file FK (counted via SQL).
	for _, p := range []string{"/docs", "/docs/sub", "/docs/sub/f.txt"} {
		row := env.rawRow(t, p)
		if got := env.wrapRowsForUUID(t, row.KeyUUID, "bob"); got != 1 {
			t.Errorf("%s: bob wrap rows = %d, want 1", p, got)
		}
	}

	// Wrap-on-write: a new folder under the share wraps its DK for bob too.
	env.mkdir(t, "/docs/newdir")
	newRow := env.rawRow(t, "/docs/newdir")
	if got := env.wrapRowsForUUID(t, newRow.KeyUUID, "bob"); got != 1 {
		t.Errorf("newdir DK: bob wrap rows = %d, want 1", got)
	}

	// The (master-wrapped) owner's share reads through for bob with
	// plaintext names.
	env.dav.Incoming = stubIncomingFeed{mounts: []files.IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/docs", Mount: "/docs",
		Permissions: webdav.PermRead, ItemType: "folder",
	}}}
	ents, err := env.dav.List(ctx, "bob", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Path)
	}
	if strings.Join(names, ",") != "/docs/newdir,/docs/sub" {
		t.Errorf("bob mount list = %v", ents)
	}
}

// TestNameCryptMixedFleet pins the per-user cutover: with the wrappers wired,
// a scheme-0 user's tree stays plaintext bit-for-bit while a scheme-1 user's
// tree encrypts in the same database.
func TestNameCryptMixedFleet(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice") // bob stays scheme 0

	env.write(t, "/plain.txt", "bob data")
	if _, _, err := env.dav.Write(ctx, "bob", "/plain.txt", bytes.NewReader([]byte("bob data")), nil); err != nil {
		t.Fatal(err)
	}
	bobRow, err := env.meta.GetByPath(ctx, env.ids["bob"], "/plain.txt")
	if err != nil {
		t.Fatal(err)
	}
	if bobRow.Name != "plain.txt" || bobRow.Path != "/plain.txt" || bobRow.NameScheme != 0 {
		t.Errorf("bob row = %+v, want plaintext marker 0", bobRow)
	}
	// Bob's folder rows carry no DK (his tree is plaintext) — mkdir works.
	if _, err := env.dav.Mkdir(ctx, "bob", "/bdir"); err != nil {
		t.Fatal(err)
	}
	bdir, err := env.meta.GetByPath(ctx, env.ids["bob"], "/bdir")
	if err != nil {
		t.Fatal(err)
	}
	if bdir.Name != "bdir" || bdir.NameScheme != 0 {
		t.Errorf("bob dir = %+v, want plaintext", bdir)
	}

	env.write(t, "/enc.txt", "alice data")
	aliceRow := env.rawRow(t, "/enc.txt")
	if aliceRow.Name == "enc.txt" || aliceRow.NameScheme != encrypt.NameSchemeNCGOFN1 {
		t.Errorf("alice row = %+v, want ciphertext marker 1", aliceRow)
	}
}

// TestNameCryptBudgetIs400 pins the ADR-0104 §3 budget at the DAV boundary:
// an over-255-rune name is ErrNameBudget → webdav.ErrBadRequest (400).
func TestNameCryptBudgetIs400(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")

	big := "/" + strings.Repeat("n", 256)
	if _, err := env.dav.Mkdir(ctx, "alice", big); !errors.Is(err, webdav.ErrBadRequest) {
		t.Errorf("mkdir 256-rune name = %v, want webdav.ErrBadRequest", err)
	}
	if _, _, err := env.dav.Write(ctx, "alice", big, strings.NewReader("x"), nil); !errors.Is(err, webdav.ErrBadRequest) {
		t.Errorf("write 256-rune name = %v, want webdav.ErrBadRequest", err)
	}
	env.write(t, "/a.txt", "a")
	if _, _, err := env.dav.Move(ctx, "alice", "/a.txt", "alice", big, false); !errors.Is(err, webdav.ErrBadRequest) {
		t.Errorf("move to 256-rune name = %v, want webdav.ErrBadRequest", err)
	}
	// At the HTTP boundary: 400, not 500.
	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, "MKCOL", "/remote.php/dav/files/alice/"+strings.Repeat("n", 256), nil)
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodBasic}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("MKCOL over-budget = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	// A 255-rune name fits the budget.
	if _, err := env.dav.Mkdir(ctx, "alice", "/"+strings.Repeat("n", 255)); err != nil {
		t.Errorf("255-rune mkdir: %v", err)
	}
}

// TestNameCryptEnrolledShareeLocked pins the ADR-0101/0104 boundary at name
// resolution: an enrolled owner's tree cannot be name-resolved by a reader
// without an unlocked session — PROPFIND of her share is ErrKeyLocked → 403
// (the keylocked mapping covers name resolution, not just content).
func TestNameCryptEnrolledShareeLocked(t *testing.T) {
	env := newPWEnv(t, "alice", "bob")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	e2e.encryptUser(t, "bob")
	aliceKey := env.login(t, "alice", "alice-pw")
	env.login(t, "bob", "bob-pw")

	// Alice builds her tree with her unlocked session ctx.
	actx := pctx("alice", aliceKey)
	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.xlsx", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	// The grant runs in alice's ctx: wrap target listing resolves names with
	// her unlocked session, and bob's wrap rows are boxes under his public
	// key.
	sh, err := env.svc.Create(actx, "alice", "/secret", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	secretRow := e2e.rawRowAs(t, actx, "alice", "/secret")
	if got := env.wrapRowsForUUID(t, secretRow.KeyUUID, "bob"); got != 1 {
		t.Fatalf("bob wrap rows on the shared folder DK = %d, want 1", got)
	}
	_ = sh
	env.dav.Incoming = stubIncomingFeed{mounts: []files.IncomingMount{{
		OwnerUID: "alice", OwnerPath: "/secret", Mount: "/secret",
		Permissions: webdav.PermRead, ItemType: "folder",
	}}}

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	propfind := func(p *auth.Principal) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), "PROPFIND", "/remote.php/dav/files/bob/secret", nil)
		req.Header.Set("Depth", "1")
		req = req.WithContext(auth.WithUser(req.Context(), p))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// A reader without an unlocked session: name resolution of the enrolled
	// owner's path is locked → 403, never a 500 and never a silent miss.
	rr := propfind(&auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodAppPassword})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("keyless sharee PROPFIND = %d, want 403: %s", rr.Code, rr.Body.String())
	}

	// Alice herself, unlocked, resolves her own tree fine.
	req := httptest.NewRequestWithContext(context.Background(), "PROPFIND", "/remote.php/dav/files/alice/secret", nil)
	req.Header.Set("Depth", "1")
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: aliceKey}))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("owner unlocked PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plans.xlsx") {
		t.Errorf("owner PROPFIND body missing the plaintext name:\n%s", rr.Body.String())
	}
	if row := e2e.rawRowAs(t, actx, "alice", "/secret/plans.xlsx"); strings.Contains(rr.Body.String(), row.Name) {
		t.Error("owner PROPFIND body leaks the name token")
	}
}
