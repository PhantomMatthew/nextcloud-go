package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func newUploads(t *testing.T) (*Uploads, *DAV) {
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
	up := NewUploads(st, NewSQLUploadStore(db), dav, us)
	freeze := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	up.Clock = func() time.Time { return freeze }
	dav.Clock = func() time.Time { return freeze }
	return up, dav
}

func TestUploadsAssembleFiveAndSixDigitChunks(t *testing.T) {
	ctx := t.Context()
	up, dav := newUploads(t)
	if _, err := up.MkdirMeta(ctx, "alice", "/tid1", webdav.CollectionMeta{Destination: "/out.bin", TotalLength: 5}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := up.Write(ctx, "alice", "/tid1/00001", strings.NewReader("he"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := up.Write(ctx, "alice", "/tid1/000002", strings.NewReader("llo"), nil); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	wantSum := "SHA256:" + hex.EncodeToString(sum[:])
	ent, created, err := up.Assemble(ctx, "alice", "tid1", "alice", "/out.bin", true, nil, wantSum, "")
	if err != nil || !created || ent.Size != 5 {
		t.Fatalf("assemble = %+v created=%v err=%v", ent, created, err)
	}
	rc, _, err := dav.Read(ctx, "alice", "/out.bin")
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
	if _, err := up.Stat(ctx, "alice", "/tid1"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("staging still present: %v", err)
	}
}

func TestUploadsAssembleGapFails(t *testing.T) {
	ctx := t.Context()
	up, _ := newUploads(t)
	if _, err := up.Mkdir(ctx, "alice", "/tid2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := up.Write(ctx, "alice", "/tid2/00001", strings.NewReader("a"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := up.Write(ctx, "alice", "/tid2/00003", strings.NewReader("c"), nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := up.Assemble(ctx, "alice", "tid2", "alice", "/gap.bin", true, nil, "", "")
	if !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("err=%v want ErrBadRequest", err)
	}
}

func TestUploadsMkdirDuplicate(t *testing.T) {
	ctx := t.Context()
	up, _ := newUploads(t)
	if _, err := up.Mkdir(ctx, "alice", "/tid3"); err != nil {
		t.Fatal(err)
	}
	if _, err := up.Mkdir(ctx, "alice", "/tid3"); !errors.Is(err, webdav.ErrExists) {
		t.Fatalf("err=%v want ErrExists", err)
	}
}

func TestUploadsMoveCopyForbidden(t *testing.T) {
	ctx := t.Context()
	up, _ := newUploads(t)
	if _, _, err := up.Move(ctx, "alice", "/a", "alice", "/b", true); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("move %v", err)
	}
	if _, _, err := up.Copy(ctx, "alice", "/a", "alice", "/b", true, true); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("copy %v", err)
	}
}

func TestParseIfETag(t *testing.T) {
	t.Parallel()
	got := parseIfETag(`</remote.php/dav/files/alice/foo.bin> (["abc123etag"])`)
	if got != "abc123etag" {
		t.Fatalf("got %q", got)
	}
}

func TestUploadsWriteRequiresSession(t *testing.T) {
	ctx := t.Context()
	up, _ := newUploads(t)
	_, _, err := up.Write(ctx, "alice", "/missing/00001", bytes.NewReader([]byte("x")), nil)
	if !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
}
