package files_test

import (
	"context"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TestTranslatingPropsStore pins ciphertext file_path at rest for
// file_properties (ADR-0104 §6 — oc:favorite is the only persisted
// path-keyed property): writes tokenize, reads echo the caller's plaintext
// path back, and the path verbs translate both endpoints.
func TestTranslatingPropsStore(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	env.mkdir(t, "/docs")
	env.write(t, "/docs/a.txt", "alpha")
	aliceID := env.ids["alice"]

	props := env.dav.Props // the TranslatingPropsStore wired by upgradeNameCrypt
	wrapped, ok := props.(*files.TranslatingPropsStore)
	if !ok {
		t.Fatalf("dav.Props = %T, want the translating store", props)
	}
	if _, ok := wrapped.Raw().(*files.SQLPropertyStore); !ok {
		t.Fatalf("Raw() = %T, want the SQL store", wrapped.Raw())
	}

	// Set: the at-rest row carries the filecache row's exact ciphertext path.
	p := &files.FileProperty{UserID: aliceID, Path: "/docs/a.txt", NS: files.PropNSOwnCloud, Name: files.PropFavorite, Value: "1"}
	if err := props.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.Path != "/docs/a.txt" {
		t.Errorf("caller struct path = %q, want the plaintext echo", p.Path)
	}
	ctA := env.rawRow(t, "/docs/a.txt").Path
	var storedPath, storedVal string
	if err := env.db.QueryRow(ctx,
		`SELECT file_path, value FROM file_properties WHERE user_id = ?`, aliceID).
		Scan(&storedPath, &storedVal); err != nil {
		t.Fatal(err)
	}
	if storedPath != ctA || storedVal != "1" {
		t.Errorf("at-rest = (%q, %q), want (%q, 1)", storedPath, storedVal, ctA)
	}

	// Get/ListByPath echo the plaintext path back.
	got, err := props.Get(ctx, aliceID, "/docs/a.txt", files.PropNSOwnCloud, files.PropFavorite)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/docs/a.txt" || got.Value != "1" {
		t.Errorf("get = (%q, %q)", got.Path, got.Value)
	}
	items, err := props.ListByPath(ctx, aliceID, "/docs/a.txt")
	if err != nil || len(items) != 1 || items[0].Path != "/docs/a.txt" {
		t.Errorf("list = %+v %v", items, err)
	}

	// RenamePath translates both endpoints; the raw prefix rewrite is string
	// work on tokens.
	if err := props.RenamePath(ctx, aliceID, "/docs/a.txt", "/docs/b.txt"); err != nil {
		t.Fatal(err)
	}
	ctB, err := env.tmeta.CipherPath(ctx, aliceID, "/docs/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_properties WHERE user_id = ?`, aliceID).
		Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	if storedPath != ctB {
		t.Errorf("renamed at-rest = %q, want %q", storedPath, ctB)
	}

	// CopyPath duplicates the row at the translated destination.
	if err := props.CopyPath(ctx, aliceID, "/docs/b.txt", "/docs/c.txt"); err != nil {
		t.Fatal(err)
	}
	if got := countProps(t, env, aliceID); got != 2 {
		t.Fatalf("props after copy = %d, want 2", got)
	}

	// DeleteByPath is a translated prefix delete.
	if err := props.DeleteByPath(ctx, aliceID, "/docs"); err != nil {
		t.Fatal(err)
	}
	if got := countProps(t, env, aliceID); got != 0 {
		t.Errorf("props after delete = %d, want 0", got)
	}

	// Scheme 0 passes through bit-identically (plaintext at rest).
	env0 := newNameE2EEnv(t, "carol")
	if _, err := env0.dav.Mkdir(ctx, "carol", "/plain"); err != nil {
		t.Fatal(err)
	}
	p0 := &files.FileProperty{UserID: env0.ids["carol"], Path: "/plain", NS: files.PropNSOwnCloud, Name: files.PropFavorite, Value: "1"}
	if err := env0.dav.Props.Set(ctx, p0); err != nil {
		t.Fatal(err)
	}
	var s0 string
	if err := env0.db.QueryRow(ctx, `SELECT file_path FROM file_properties WHERE user_id = ?`, env0.ids["carol"]).
		Scan(&s0); err != nil {
		t.Fatal(err)
	}
	if s0 != "/plain" {
		t.Errorf("scheme-0 at-rest = %q, want /plain", s0)
	}
}

func countProps(t *testing.T, env *nameE2EEnv, userID int64) int64 {
	t.Helper()
	var n int64
	if err := env.db.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM file_properties WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNameCryptFavoriteLifecycle pins the DAV-level favorite flow on an
// encrypted user: PROPPATCH persists the mark under the ciphertext path, the
// wire keeps showing it, a rename moves the row, trash+restore to the same
// path keeps it (the deterministic parent-DK token reproduces), and purge
// clears it.
func TestNameCryptFavoriteLifecycle(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	env.mkdir(t, "/docs")
	env.write(t, "/docs/a.txt", "alpha")
	aliceID := env.ids["alice"]

	res, err := env.dav.PatchProps(ctx, "alice", "/docs/a.txt", []webdav.PropPatchOp{
		{Space: files.PropNSOwnCloud, Name: files.PropFavorite, Value: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != 200 {
		t.Fatalf("PROPPATCH = %+v, want one 200", res)
	}
	ent, err := env.dav.Stat(ctx, "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.Favorite != 1 {
		t.Errorf("stat favorite = %d, want 1", ent.Favorite)
	}
	if got := countProps(t, env, aliceID); got != 1 {
		t.Fatalf("at-rest props = %d, want 1", got)
	}

	// Rename: the row follows the file's new ciphertext path.
	if _, _, err := env.dav.Move(ctx, "alice", "/docs/a.txt", "alice", "/docs/moved.txt", false); err != nil {
		t.Fatal(err)
	}
	var storedPath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_properties WHERE user_id = ?`, aliceID).
		Scan(&storedPath); err != nil {
		t.Fatal(err)
	}
	if want := env.rawRow(t, "/docs/moved.txt").Path; storedPath != want {
		t.Errorf("post-move at-rest = %q, want %q", storedPath, want)
	}
	ent, err = env.dav.Stat(ctx, "alice", "/docs/moved.txt")
	if err != nil || ent.Favorite != 1 {
		t.Errorf("post-move stat favorite = %d %v", ent.Favorite, err)
	}

	// Trash + restore to the same path: the favorite survives (the parent
	// DK is untouched, so the restored leaf reproduces its token).
	if err := env.dav.Remove(ctx, "alice", "/docs/moved.txt"); err != nil {
		t.Fatal(err)
	}
	items, err := env.dav.Trash.Sessions.List(ctx, aliceID)
	if err != nil || len(items) != 1 {
		t.Fatalf("trash items = %+v %v", items, err)
	}
	if _, _, err := env.dav.Trash.Restore(ctx, "alice", items[0].LocationID, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	ent, err = env.dav.Stat(ctx, "alice", "/docs/moved.txt")
	if err != nil || ent.Favorite != 1 {
		t.Errorf("post-restore stat favorite = %d %v", ent.Favorite, err)
	}

	// Trash + purge: the row goes with the bytes.
	if err := env.dav.Remove(ctx, "alice", "/docs/moved.txt"); err != nil {
		t.Fatal(err)
	}
	items, err = env.dav.Trash.Sessions.List(ctx, aliceID)
	if err != nil || len(items) != 1 {
		t.Fatalf("trash items = %+v %v", items, err)
	}
	if err := env.dav.Trash.PurgeLocation(ctx, "alice", items[0].LocationID); err != nil {
		t.Fatal(err)
	}
	if got := countProps(t, env, aliceID); got != 0 {
		t.Errorf("post-purge props = %d, want 0", got)
	}
	if !strings.Contains(storedPath, "/") {
		t.Error("sanity: the stored path shape broke mid-test")
	}
}
