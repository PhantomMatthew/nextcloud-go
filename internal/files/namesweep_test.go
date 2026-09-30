package files_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// The phase-4 sweep tests (ADR-0104): per-user transactional conversion of
// existing plaintext trees to NCGOFN1 ciphertext and back.

// newSweepFixture builds the sweep's input state on a keyShareEnv (the
// flag-off wiring — plaintext tree, v3-sealed file content, raw satellite
// stores): nested folders, files, a lock, a version (overwrite snapshot), a
// trashed file, a user share of /docs to bob, and an in-flight upload
// session. Returns the env plus the pre-sweep trash location id.
func newSweepFixture(t *testing.T) (env *keyShareEnv, trashLoc string) {
	t.Helper()
	env = newKeyShareEnv(t, "alice", "bob")
	ctx := context.Background()
	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.write(t, "/docs/sub/hello.txt", "hello world")
	env.write(t, "/a.txt", "alpha v1")
	env.write(t, "/a.txt", "alpha v2") // the overwrite snapshots a version
	env.write(t, "/docs/note.txt", "note")
	if err := files.NewSQLLockStore(env.db).Insert(ctx, &files.FileLock{
		UserID: env.ids["alice"], Path: "/a.txt", Token: "sweep-lock-token", Owner: "alice", TimeoutMs: 3600000,
	}); err != nil {
		t.Fatal(err)
	}
	// A favorite mark — file_properties is a path-keyed satellite too.
	if err := files.NewSQLPropertyStore(env.db).Set(ctx, &files.FileProperty{
		UserID: env.ids["alice"], Path: "/a.txt", NS: files.PropNSOwnCloud, Name: files.PropFavorite, Value: "1",
	}); err != nil {
		t.Fatal(err)
	}
	// Raw trash store: plaintext original_path and a plaintext-basename
	// location id — the pre-sweep state.
	if err := env.dav.Trash.MoveToTrash(ctx, "alice", "/docs/note.txt", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT location_id FROM trash_items WHERE user_id = ?`, env.ids["alice"]).Scan(&trashLoc); err != nil {
		t.Fatal(err)
	}
	// Raw share insert — grant hooks bypassed, the pre-mode state.
	if err := env.shares.Insert(ctx, &files.Share{
		OwnerUserID: env.ids["alice"], ShareType: files.ShareTypeUser, Path: "/docs",
		ItemType: "folder", Token: "sweep-share-token", Permissions: 1, ShareWith: "bob",
	}); err != nil {
		t.Fatal(err)
	}
	// In-flight chunked-upload session (plaintext destination).
	if err := files.NewSQLUploadStore(env.db).Create(ctx, &files.UploadSession{
		UserID: env.ids["alice"], TransferID: "sweep-transfer", Destination: "/docs/upload.bin", TotalLength: 10,
	}); err != nil {
		t.Fatal(err)
	}
	return env, trashLoc
}

func newSweep(env *keyShareEnv) *files.NameSweep {
	return &files.NameSweep{
		DB:      env.db,
		Store:   env.meta,
		Keys:    env.res,
		Users:   env.users,
		Storage: env.dav.Storage,
	}
}

func drySweep(env *keyShareEnv) *files.NameSweep {
	s := newSweep(env)
	s.DryRun = true
	return s
}

// sweepTranslator answers post-sweep reads through the translating stack.
func sweepTranslator(env *keyShareEnv) (*files.NameTranslator, *files.TranslatingStore) {
	xl := files.NewNameTranslator(env.meta, env.res, env.users)
	return xl, files.NewTranslatingStore(env.meta, xl)
}

func schemeOf(t *testing.T, env *keyShareEnv) int {
	t.Helper()
	scheme, err := env.users.UserNameScheme(context.Background(), env.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	return scheme
}

func countTable(t *testing.T, env *keyShareEnv, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := env.db.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNameSweepEncryptEndToEnd pins the phase-4 cutover: one tx rewrites
// every table, the trash object follows its location id, the share row's
// sealed fields open, bob holds wraps of the subtree directory keys, and the
// translated stack reads the tree plaintext.
func TestNameSweepEncryptEndToEnd(t *testing.T) {
	ctx := context.Background()
	env, oldTrashLoc := newSweepFixture(t)
	aliceID := env.ids["alice"]
	bobID := env.ids["bob"]

	// Dry-run: the full report with zero writes — no DK mints, no tokens, no
	// scheme flip, trash object unmoved.
	wrapsBefore := countTable(t, env, `SELECT COUNT(*) FROM file_keys`)
	dry, err := drySweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	wantStats := files.NameSweepStats{
		FilesRows: 5, FoldersKeyed: 3, LocksRows: 1, VersionsRows: 1, PropsRows: 1,
		TrashRows: 1, TrashMoved: 1, SharesRows: 1, ShareWraps: 2, SkippedRows: 0,
	}
	if dry != wantStats {
		t.Fatalf("dry-run stats = %+v, want %+v", dry, wantStats)
	}
	if got := schemeOf(t, env); got != 0 {
		t.Fatalf("dry-run flipped name_scheme to %d", got)
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM file_keys`); got != wrapsBefore {
		t.Fatal("dry-run minted directory key wraps")
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM files WHERE name_scheme = 1`); got != 0 {
		t.Fatal("dry-run tokenized files rows")
	}
	if _, err := env.dav.Storage.Stat(ctx, "trash/alice/"+oldTrashLoc); err != nil {
		t.Fatal("dry-run moved the trash object")
	}

	stats, err := newSweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stats != wantStats {
		t.Fatalf("sweep stats = %+v, want %+v", stats, wantStats)
	}
	if got := schemeOf(t, env); got != 1 {
		t.Fatalf("users.name_scheme = %d, want 1", got)
	}

	// Raw SQL: every files row is scheme 1 with ciphertext names; the root
	// stays "/"/""; folders (root included) carry key_uuids; files keep
	// their FKs.
	rows, err := env.db.Query(ctx, `
SELECT id, parent_id, name, path, is_dir, key_uuid, name_scheme FROM files WHERE user_id = ? ORDER BY id`, aliceID)
	if err != nil {
		t.Fatal(err)
	}
	type rawRow struct {
		id         int64
		hasParent  bool
		parentID   int64
		name, path string
		isDir      bool
		keyUUID    []byte
		scheme     int
	}
	var raw []rawRow
	for rows.Next() {
		var r rawRow
		var parent any
		var isDir int
		if err := rows.Scan(&r.id, &parent, &r.name, &r.path, &isDir, &r.keyUUID, &r.scheme); err != nil {
			t.Fatal(err)
		}
		if pid, ok := parent.(int64); ok {
			r.hasParent, r.parentID = true, pid
		}
		r.isDir = isDir != 0
		raw = append(raw, r)
	}
	_ = rows.Close()
	if len(raw) != 5 {
		t.Fatalf("files rows = %d, want 5", len(raw))
	}
	var rootID int64
	ctATxt, ctDocs := "", ""
	for _, r := range raw {
		if r.scheme != 1 {
			t.Errorf("row %d name_scheme = %d", r.id, r.scheme)
		}
		if len(r.keyUUID) != 16 {
			t.Errorf("row %d (%q, dir=%v) key_uuid = %d bytes, want 16", r.id, r.path, r.isDir, len(r.keyUUID))
		}
		for _, leak := range []string{"docs", "sub", "hello", "a.txt", "note"} {
			if strings.Contains(r.name, leak) || strings.Contains(r.path, leak) {
				t.Errorf("row %d leaks plaintext %q: name=%q path=%q", r.id, leak, r.name, r.path)
			}
		}
		if !r.hasParent {
			rootID = r.id
			if r.path != "/" || r.name != "" {
				t.Errorf("root = (%q, %q), want (/, )", r.path, r.name)
			}
		}
		if r.hasParent && !r.isDir {
			ctATxt = r.path // /a.txt is the only file with a scheme-1 lock/version
		}
		if r.hasParent && r.isDir && r.parentID == rootID {
			ctDocs = r.path // /docs is the only top-level folder
		}
	}

	// Locks, versions and the favorite carry the /a.txt ciphertext path
	// (deterministic tokens — identical to the files row's).
	var lockPath, versionPath, propPath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_locks WHERE user_id = ?`, aliceID).Scan(&lockPath); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions WHERE user_id = ?`, aliceID).Scan(&versionPath); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_properties WHERE user_id = ?`, aliceID).Scan(&propPath); err != nil {
		t.Fatal(err)
	}
	if lockPath != ctATxt || versionPath != ctATxt || propPath != ctATxt || ctATxt == "" {
		t.Errorf("lock=%q version=%q prop=%q files=%q — all four must be the /a.txt ciphertext path", lockPath, versionPath, propPath, ctATxt)
	}

	// Trash: ciphertext original_path/name, token-based location id (the
	// ".d<ts>" suffix survives), object moved (old gone, new present).
	var trashOrig, trashName, trashLoc string
	if err := env.db.QueryRow(ctx, `SELECT original_path, name, location_id FROM trash_items WHERE user_id = ?`, aliceID).Scan(&trashOrig, &trashName, &trashLoc); err != nil {
		t.Fatal(err)
	}
	suffix := oldTrashLoc[strings.LastIndex(oldTrashLoc, ".d"):]
	if !strings.HasPrefix(trashLoc, trashName) || !strings.HasSuffix(trashLoc, suffix) {
		t.Errorf("location id %q must be <name token %q> + suffix %q", trashLoc, trashName, suffix)
	}
	for _, leak := range []string{"note", "docs"} {
		if strings.Contains(trashOrig, leak) || strings.Contains(trashName, leak) {
			t.Errorf("trash row leaks plaintext %q: original_path=%q name=%q", leak, trashOrig, trashName)
		}
	}
	if !strings.HasPrefix(trashOrig, ctDocs+"/") {
		t.Errorf("trash original_path %q must sit under the /docs ciphertext %q", trashOrig, ctDocs)
	}
	if _, err := env.dav.Storage.Stat(ctx, "trash/alice/"+trashLoc); err != nil {
		t.Errorf("trash object missing under its new id: %v", err)
	}
	if _, err := env.dav.Storage.Stat(ctx, "trash/alice/"+oldTrashLoc); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("old trash object should be gone, stat err = %v", err)
	}

	// The upload session is NOT converted (documented residual — finalize
	// fails loudly, the client retries cleanly).
	var dest string
	if err := env.db.QueryRow(ctx, `SELECT destination FROM uploads WHERE user_id = ?`, aliceID).Scan(&dest); err != nil {
		t.Fatal(err)
	}
	if dest != "/docs/upload.bin" {
		t.Errorf("uploads.destination = %q, want the untouched plaintext", dest)
	}

	// The share row: ciphertext file_path, sealed metadata copies open to
	// (/docs, docs); bob holds wraps of both subtree DKs.
	var sharePath, mountEnc, absEnc string
	if err := env.db.QueryRow(ctx, `SELECT file_path, mount_name_enc, abs_path_enc FROM shares WHERE owner_user_id = ?`, aliceID).Scan(&sharePath, &mountEnc, &absEnc); err != nil {
		t.Fatal(err)
	}
	if sharePath != ctDocs || mountEnc == "" || absEnc == "" {
		t.Errorf("share row = (%q, %q, %q), want file_path = the /docs ciphertext %q", sharePath, mountEnc, absEnc, ctDocs)
	}
	xl, tmeta := sweepTranslator(env)
	plainAbs, mountName, err := xl.OpenShareMeta(ctx, aliceID, &files.Share{Path: sharePath, MountNameEnc: mountEnc, AbsPathEnc: absEnc})
	if err != nil {
		t.Fatal(err)
	}
	if plainAbs != "/docs" || mountName != "docs" {
		t.Errorf("OpenShareMeta = (%q, %q), want (/docs, docs)", plainAbs, mountName)
	}
	if got := countTable(t, env, `
SELECT COUNT(*) FROM file_keys k JOIN files f ON f.key_uuid = k.key_uuid
WHERE k.user_id = ? AND f.is_dir = 1`, bobID); got != 2 {
		t.Errorf("bob's directory-key wraps = %d, want 2 (/docs and /docs/sub)", got)
	}

	// The translated stack reads the tree plaintext.
	hello, err := tmeta.GetByPath(ctx, aliceID, "/docs/sub/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if hello.Name != "hello.txt" || hello.Path != "/docs/sub/hello.txt" {
		t.Errorf("translated row = (%q, %q)", hello.Name, hello.Path)
	}
	sub, err := tmeta.GetByPath(ctx, aliceID, "/docs/sub")
	if err != nil {
		t.Fatal(err)
	}
	kids, err := tmeta.ListChildren(ctx, aliceID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 1 || kids[0].Name != "hello.txt" {
		t.Errorf("translated listing = %+v", kids)
	}

	// Idempotent re-run: a skip; nothing changes.
	again, err := newSweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !again.Skipped || again.FilesRows != 0 {
		t.Fatalf("second run = %+v, want a bare skip", again)
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM files WHERE user_id = ?`, aliceID); got != 5 {
		t.Fatalf("files rows after re-run = %d", got)
	}

	// Unknown user: an error, never an invented conversion.
	if _, err := newSweep(env).EncryptUser(ctx, "nobody"); err == nil {
		t.Fatal("unknown user must error")
	}
}

// TestNameSweepDecryptRoundTrip pins the decommission path: decrypt-names
// restores plaintext everywhere, strips folder key_uuids and their wrap rows
// (file FKs stay — content still reads), and moves trash objects back.
func TestNameSweepDecryptRoundTrip(t *testing.T) {
	ctx := context.Background()
	env, oldTrashLoc := newSweepFixture(t)
	aliceID := env.ids["alice"]

	if _, err := newSweep(env).EncryptUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	wrapsEncrypted := countTable(t, env, `SELECT COUNT(*) FROM file_keys`)

	stats, err := newSweep(env).DecryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := files.NameSweepStats{
		FilesRows: 5, LocksRows: 1, VersionsRows: 1, PropsRows: 1,
		TrashRows: 1, TrashMoved: 1, SharesRows: 1, SkippedRows: 0,
	}
	if stats != want {
		t.Fatalf("decrypt stats = %+v, want %+v", stats, want)
	}
	if got := schemeOf(t, env); got != 0 {
		t.Fatalf("users.name_scheme = %d, want 0", got)
	}

	// Everything is plaintext again; folders carry no key_uuid; files keep
	// their FKs.
	rows, err := env.db.Query(ctx, `
SELECT name, path, is_dir, key_uuid, name_scheme FROM files WHERE user_id = ? ORDER BY id`, aliceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var name, p string
		var isDir, scheme int
		var keyUUID []byte
		if err := rows.Scan(&name, &p, &isDir, &keyUUID, &scheme); err != nil {
			t.Fatal(err)
		}
		if scheme != 0 {
			t.Errorf("row %q name_scheme = %d", p, scheme)
		}
		if isDir != 0 && len(keyUUID) != 0 {
			t.Errorf("folder %q still carries a directory key uuid", p)
		}
		if isDir == 0 && len(keyUUID) != 16 {
			t.Errorf("file %q lost its file key uuid", p)
		}
		paths = append(paths, p)
	}
	wantPaths := []string{"/", "/docs", "/docs/sub", "/docs/sub/hello.txt", "/a.txt"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Errorf("paths = %v, want %v", paths, wantPaths)
	}

	// Folder DK wrap rows are gone (3 owner + 2 sharee); file FK wraps stay.
	wrapsDecrypted := countTable(t, env, `SELECT COUNT(*) FROM file_keys`)
	if wrapsEncrypted-wrapsDecrypted != 5 {
		t.Errorf("wrap rows %d → %d, want exactly the 5 folder-DK wraps deleted", wrapsEncrypted, wrapsDecrypted)
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM file_keys WHERE user_id = ?`, env.ids["bob"]); got != 0 {
		t.Errorf("bob still holds %d wraps", got)
	}

	// Locks, versions, props, trash, shares are plaintext; the upload session
	// is untouched as ever.
	var lockPath, versionPath, propPath, trashOrig, trashLoc, sharePath, mountEnc, absEnc, dest string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_locks WHERE user_id = ?`, aliceID).Scan(&lockPath); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions WHERE user_id = ?`, aliceID).Scan(&versionPath); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_properties WHERE user_id = ?`, aliceID).Scan(&propPath); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT original_path, location_id FROM trash_items WHERE user_id = ?`, aliceID).Scan(&trashOrig, &trashLoc); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path, mount_name_enc, abs_path_enc FROM shares WHERE owner_user_id = ?`, aliceID).Scan(&sharePath, &mountEnc, &absEnc); err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT destination FROM uploads WHERE user_id = ?`, aliceID).Scan(&dest); err != nil {
		t.Fatal(err)
	}
	if lockPath != "/a.txt" || versionPath != "/a.txt" || propPath != "/a.txt" {
		t.Errorf("lock=%q version=%q prop=%q, want /a.txt", lockPath, versionPath, propPath)
	}
	if trashOrig != "/docs/note.txt" || trashLoc != oldTrashLoc {
		t.Errorf("trash = (%q, %q), want (/docs/note.txt, %q)", trashOrig, trashLoc, oldTrashLoc)
	}
	if sharePath != "/docs" || mountEnc != "" || absEnc != "" {
		t.Errorf("share = (%q, %q, %q), want plaintext with cleared enc fields", sharePath, mountEnc, absEnc)
	}
	if dest != "/docs/upload.bin" {
		t.Errorf("uploads.destination = %q", dest)
	}

	// The trash object moved back; the token id is gone.
	if _, err := env.dav.Storage.Stat(ctx, "trash/alice/"+oldTrashLoc); err != nil {
		t.Errorf("trash object not back at its plaintext id: %v", err)
	}

	// File content still reads — files kept their FKs through the round-trip.
	rc, _, err := env.dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "alpha v2" {
		t.Errorf("content = %q, want the v2 bytes", body)
	}

	// Re-encrypting after a decrypt works (fresh DKs mint again), and a
	// scheme-0 user is a decrypt skip (idempotent) once back out.
	reStats, err := newSweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if reStats.Skipped || reStats.FoldersKeyed != 3 {
		t.Fatalf("re-encrypt = %+v", reStats)
	}
	if _, err := newSweep(env).DecryptUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	skipStats, err := newSweep(env).DecryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !skipStats.Skipped || skipStats.FilesRows != 0 {
		t.Fatalf("second decrypt = %+v, want a bare skip", skipStats)
	}
}

// TestNameSweepBudgetAborts pins the length-budget abort: an over-budget name
// fails the user with NOTHING written (the tx never opened), and the sweep
// succeeds after a rename.
func TestNameSweepBudgetAborts(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.mkdir(t, "/deep")
	longName := strings.Repeat("日", 255) // 255 runes — 765 bytes; the token path blows the 768 budget
	env.write(t, "/deep/"+longName, "x")

	_, err := newSweep(env).EncryptUser(ctx, "alice")
	if err == nil || !errors.Is(err, files.ErrNameBudget) {
		t.Fatalf("err = %v, want ErrNameBudget", err)
	}
	if !strings.Contains(err.Error(), longName) {
		t.Errorf("error must name the offending plaintext path: %v", err)
	}
	// Nothing written: scheme 0, rows plaintext, folders keyless (the tx
	// never ran — the key pass's minted wrap rows are the documented benign
	// orphans).
	if got := schemeOf(t, env); got != 0 {
		t.Fatalf("name_scheme = %d after abort", got)
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM files WHERE name_scheme <> 0`); got != 0 {
		t.Fatal("abort still tokenized rows")
	}
	if got := countTable(t, env, `SELECT COUNT(*) FROM files WHERE is_dir = 1 AND key_uuid IS NOT NULL`); got != 0 {
		t.Fatal("abort still claimed folder keys onto rows")
	}

	// Rename the offender (raw SQL — the files rows are what the sweep
	// reads) and the sweep succeeds.
	if _, err := env.db.Exec(ctx, `UPDATE files SET name = 'short.txt', path = '/deep/short.txt' WHERE name = ?`, longName); err != nil {
		t.Fatal(err)
	}
	stats, err := newSweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesRows != 3 || stats.FoldersKeyed != 2 {
		t.Fatalf("post-rename stats = %+v", stats)
	}
	if got := schemeOf(t, env); got != 1 {
		t.Fatalf("name_scheme = %d", got)
	}
}

// TestNameSweepSkipsOrphanTrash pins the documented residual: a trash row
// whose original_path no longer resolves against the live tree (an ancestor
// was deleted later) is skipped and counted — its row keeps plaintext fields
// and its object is NOT moved — while resolvable rows convert.
func TestNameSweepSkipsOrphanTrash(t *testing.T) {
	ctx := context.Background()
	env := newKeyShareEnv(t, "alice")
	env.mkdir(t, "/a")
	env.mkdir(t, "/a/b")
	env.write(t, "/a/b/c.txt", "child")
	if err := env.dav.Trash.MoveToTrash(ctx, "alice", "/a/b/c.txt", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := env.dav.Trash.MoveToTrash(ctx, "alice", "/a/b", "alice"); err != nil {
		t.Fatal(err)
	}
	// c.txt's row: ancestors /a (exists) and /a/b (now in trash) →
	// unresolvable. /a/b's own row resolves against /a.
	var orphanLoc string
	if err := env.db.QueryRow(ctx, `SELECT location_id FROM trash_items WHERE original_path = '/a/b/c.txt'`).Scan(&orphanLoc); err != nil {
		t.Fatal(err)
	}

	stats, err := newSweep(env).EncryptUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stats.TrashRows != 1 || stats.SkippedRows != 1 || stats.TrashMoved != 1 {
		t.Fatalf("stats = %+v, want one converted, one skipped, one moved", stats)
	}
	// The orphan row keeps its plaintext fields; its object did not move.
	var orig, loc string
	if err := env.db.QueryRow(ctx, `SELECT original_path, location_id FROM trash_items WHERE original_path LIKE '%c.txt'`).Scan(&orig, &loc); err != nil {
		t.Fatal(err)
	}
	if orig != "/a/b/c.txt" || loc != orphanLoc {
		t.Errorf("orphan row = (%q, %q), want untouched plaintext", orig, loc)
	}
	if _, err := env.dav.Storage.Stat(ctx, "trash/alice/"+orphanLoc); err != nil {
		t.Errorf("orphan trash object moved: %v", err)
	}
	// The resolvable row tokenized; /a's own subtree converted fine.
	if got := schemeOf(t, env); got != 1 {
		t.Fatalf("name_scheme = %d", got)
	}
	var folderOrig string
	if err := env.db.QueryRow(ctx, `SELECT original_path FROM trash_items WHERE location_id <> ?`, orphanLoc).Scan(&folderOrig); err != nil {
		t.Fatal(err)
	}
	// No plaintext SEGMENT survives (exact-segment compare — a substring check
	// false-positives whenever a random b64url token happens to start with
	// 'a'). Stronger still: the stored ciphertext round-trips through the real
	// translator back to the plaintext original path.
	for _, seg := range strings.Split(folderOrig, "/") {
		if seg == "a" || seg == "b" {
			t.Errorf("folder trash row leaks plaintext segment %q in %q", seg, folderOrig)
		}
	}
	xlate := files.NewNameTranslator(env.meta, env.res, env.users)
	back, err := xlate.PlainPath(ctx, env.ids["alice"], folderOrig)
	if err != nil {
		t.Fatalf("converted trash path does not decrypt: %v", err)
	}
	if back != "/a/b" {
		t.Errorf("PlainPath(%q) = %q, want /a/b", folderOrig, back)
	}
}

// TestNameSweepEnrolled pins constraint (1) of ADR-0104 phase 4: an enrolled
// user's tree can never be converted by a principal-less run (their DK/FK
// boxes open only with an unlocked session) — the login-hook ctx converts
// them, and decrypt offline fails with the unenroll-first hint.
func TestNameSweepEnrolled(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice")
	env.mkdir(t, "/docs")
	env.write(t, "/docs/hello.txt", "hello")
	// Enroll alice (password login runs the state machine) — her UK and wrap
	// rows become X25519 boxes.
	priv := env.login(t, "alice", "pw")
	sweep := &files.NameSweep{
		DB:      env.db,
		Store:   env.meta,
		Keys:    env.res,
		Users:   env.users,
		Storage: env.dav.Storage,
	}

	// Principal-less: loud abort naming the login path; nothing converted.
	_, err := sweep.EncryptUser(ctx, "alice")
	if err == nil || !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Fatalf("offline enrolled encrypt err = %v, want ErrKeyLocked", err)
	}
	if !strings.Contains(err.Error(), "password login") {
		t.Errorf("error must name the login path: %v", err)
	}
	if got := schemeOf(t, &env.keyShareEnv); got != 0 {
		t.Fatalf("name_scheme = %d after the refused sweep", got)
	}

	// The login-hook path: the ctx carries the unlocked key → converts.
	stats, err := sweep.EncryptUser(pctx("alice", priv), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped || stats.FilesRows != 3 {
		t.Fatalf("login-ctx sweep = %+v", stats)
	}
	if got := schemeOf(t, &env.keyShareEnv); got != 1 {
		t.Fatalf("name_scheme = %d", got)
	}
	// The enrolled owner reads their converted tree through the translating
	// stack with the unlocked session.
	xl := files.NewNameTranslator(env.meta, env.res, env.users)
	tmeta := files.NewTranslatingStore(env.meta, xl)
	hello, err := tmeta.GetByPath(pctx("alice", priv), env.ids["alice"], "/docs/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if hello.Name != "hello.txt" {
		t.Errorf("translated name = %q", hello.Name)
	}

	// Offline decrypt of the enrolled (now scheme-1) user: loud, with the
	// unenroll-first hint.
	_, err = sweep.DecryptUser(ctx, "alice")
	if err == nil || !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Fatalf("offline enrolled decrypt err = %v, want ErrKeyLocked", err)
	}
	if !strings.Contains(err.Error(), "unenroll first") {
		t.Errorf("error must carry the unenroll-first hint: %v", err)
	}
	// With the unlocked ctx the decrypt round-trips.
	if _, err := sweep.DecryptUser(pctx("alice", priv), "alice"); err != nil {
		t.Fatal(err)
	}
	if got := schemeOf(t, &env.keyShareEnv); got != 0 {
		t.Fatalf("name_scheme = %d after decrypt", got)
	}
	row, err := env.meta.GetByPath(ctx, env.ids["alice"], "/docs/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != "hello.txt" || row.NameScheme != 0 {
		t.Errorf("row after round-trip = (%q, scheme %d)", row.Name, row.NameScheme)
	}
}
