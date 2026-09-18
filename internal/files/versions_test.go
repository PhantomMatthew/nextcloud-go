package files

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

func newVersions(t *testing.T) (*Versions, *DAV, int64) {
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
	ver := NewVersions(st, NewSQLVersionStore(db), dav, us)
	dav.Versions = ver
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	ver.Clock = func() time.Time { return freeze }
	return ver, dav, u.ID
}

func TestVersionsSnapshotRestoreRollback(t *testing.T) {
	ctx := t.Context()
	ver, dav, _ := newVersions(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("world"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	fid := strconv.FormatUint(st.NumericID, 10)
	ents, err := ver.List(ctx, "alice", "/versions/"+fid)
	if err != nil || len(ents) != 1 {
		t.Fatalf("list = %v %v", ents, err)
	}
	rc, _, err := ver.Read(ctx, "alice", "/versions/"+fid+"/1746100800")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "hello" {
		t.Fatalf("version body=%q err=%v", body, err)
	}

	got, _, err := ver.RestoreVersion(ctx, "alice", fid, "1746100800", "alice")
	if err != nil || got.Size != 5 {
		t.Fatalf("restore = %+v err=%v", got, err)
	}
	rc, _, err = dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "hello" {
		t.Fatalf("restored body=%q err=%v", body, err)
	}
}

func TestVersionsPurgeRemovesHistory(t *testing.T) {
	ctx := t.Context()
	ver, dav, uid := newVersions(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("world"), nil); err != nil {
		t.Fatal(err)
	}
	if err := dav.Purge(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	listed, err := ver.Meta.ListByPath(ctx, uid, "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("versions remain: %v", listed)
	}
}

func TestVersionsMoveRenamesPath(t *testing.T) {
	ctx := t.Context()
	ver, dav, _ := newVersions(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("world"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Move(ctx, "alice", "/a.txt", "alice", "/b.txt", true); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	ents, err := ver.List(ctx, "alice", "/versions/"+strconv.FormatUint(st.NumericID, 10))
	if err != nil || len(ents) != 1 {
		t.Fatalf("list after move = %v %v", ents, err)
	}
}

func TestVersionsRollbackLatest(t *testing.T) {
	ctx := t.Context()
	ver, dav, uid := newVersions(t)
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("bad"), nil); err != nil {
		t.Fatal(err)
	}
	if err := ver.RollbackLatest(ctx, "alice", "/a.txt"); err != nil {
		t.Fatal(err)
	}
	rc, _, err := dav.Read(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if cerr := rc.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || string(body) != "hello" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	listed, err := ver.Meta.ListByPath(ctx, uid, "/a.txt")
	if err != nil || len(listed) != 0 {
		t.Fatalf("rollback left versions: %v %v", listed, err)
	}
}

func TestVersionsLazyExpire(t *testing.T) {
	ctx := t.Context()
	ver, dav, _ := newVersions(t)
	ver.Retention = time.Second
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("world"), nil); err != nil {
		t.Fatal(err)
	}
	st, err := dav.Stat(ctx, "alice", "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	ver.Clock = func() time.Time {
		return time.Date(2025, 5, 1, 12, 0, 2, 0, time.UTC)
	}
	ents, err := ver.List(ctx, "alice", "/versions/"+strconv.FormatUint(st.NumericID, 10))
	if err != nil || len(ents) != 0 {
		t.Fatalf("expired still listed: %v %v", ents, err)
	}
}

func TestParseNumericFileID(t *testing.T) {
	t.Parallel()
	id, ok := parseNumericFileID("00000006ocdev00001")
	if !ok || id != 6 {
		t.Fatalf("got %d %v", id, ok)
	}
	if _, ok := parseNumericFileID("abc"); ok {
		t.Fatal("abc should fail")
	}
}
