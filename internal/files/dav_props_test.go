package files

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func newPropsDAV(t *testing.T) *DAV {
	t.Helper()
	ctx := t.Context()
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
	dav := NewDAV(st, NewSQLStore(db), us)
	dav.Props = NewSQLPropertyStore(db)
	tr := NewTrash(st, NewSQLTrashStore(db), dav, us)
	dav.Trash = tr
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	tr.Clock = func() time.Time { return freeze }
	return dav
}

func TestDAVPatchPropsFavorite(t *testing.T) {
	ctx := t.Context()
	dav := newPropsDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	results, err := dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: PropNSOwnCloud,
		Name:  PropFavorite,
		Value: "1",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("patch = %v err=%v", results, err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("stat favorite = %+v err=%v", st, err)
	}

	results, err = dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: "DAV:",
		Name:  "getetag",
		Value: "nope",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusForbidden {
		t.Fatalf("protected = %v err=%v", results, err)
	}

	if _, err := dav.PatchProps(ctx, "alice", "/missing.txt", []webdav.PropPatchOp{{
		Space: PropNSOwnCloud,
		Name:  PropFavorite,
		Value: "1",
	}}); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestDAVFavoriteMoveCopyPurge(t *testing.T) {
	ctx := t.Context()
	dav := newPropsDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: PropNSOwnCloud, Name: PropFavorite, Value: "1",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Copy(ctx, "alice", "/a.txt", "alice", "/b.txt", false, true); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/b.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("copy favorite = %+v err=%v", st, err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/a.txt", "alice", "/c.txt", false); err != nil {
		t.Fatal(err)
	}
	st, err = dav.Stat(ctx, "alice", "/c.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("move favorite = %+v err=%v", st, err)
	}
	if _, err := dav.Stat(ctx, "alice", "/a.txt"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("moved source = %v", err)
	}
	if err := dav.Purge(ctx, "alice", "/c.txt"); err != nil {
		t.Fatal(err)
	}
	u, err := dav.Users.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Props.Get(ctx, u.ID, "/c.txt", PropNSOwnCloud, PropFavorite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged property still present: %v", err)
	}
}

func TestDAVFavoriteSurvivesTrashRestore(t *testing.T) {
	ctx := t.Context()
	dav := newPropsDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.PatchProps(ctx, "alice", "/a.txt", []webdav.PropPatchOp{{
		Space: PropNSOwnCloud, Name: PropFavorite, Value: "1",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := dav.Remove(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	ents, err := dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list = %v %v", ents, err)
	}
	loc := strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := dav.Trash.Restore(ctx, "alice", loc, "alice", "", true); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("restored favorite = %+v err=%v", st, err)
	}

	if err := dav.Remove(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	ents, err = dav.Trash.List(ctx, "alice", "/trash")
	if err != nil || len(ents) != 1 {
		t.Fatalf("trash list 2 = %v %v", ents, err)
	}
	loc = strings.TrimPrefix(ents[0].Path, "/")
	if _, _, err := dav.Trash.Restore(ctx, "alice", loc, "alice", "/restored.txt", true); err != nil {
		t.Fatal(err)
	}
	st, err = dav.Stat(ctx, "alice", "/restored.txt")
	if err != nil || st.Favorite != 1 {
		t.Fatalf("renamed restore favorite = %+v err=%v", st, err)
	}
}
