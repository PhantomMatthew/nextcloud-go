package webdav

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestInMemoryFS_StatRoot(t *testing.T) {
	fs := NewInMemoryFS()
	entry, err := fs.Stat(context.Background(), "alice", "/")
	if err != nil {
		t.Fatalf("Stat / err = %v", err)
	}
	if !entry.IsDir {
		t.Errorf("root IsDir = false, want true")
	}
	if entry.Path != "/" {
		t.Errorf("root Path = %q, want /", entry.Path)
	}
	if entry.NumericID == 0 {
		t.Errorf("root NumericID = 0, want non-zero")
	}
	if entry.ContentType != "httpd/unix-directory" {
		t.Errorf("root ContentType = %q", entry.ContentType)
	}
}

func TestInMemoryFS_StatRootIdempotent(t *testing.T) {
	fs := NewInMemoryFS()
	a, _ := fs.Stat(context.Background(), "alice", "/")
	b, _ := fs.Stat(context.Background(), "alice", "/")
	if a.NumericID != b.NumericID {
		t.Errorf("repeat stat assigned different IDs: %d vs %d", a.NumericID, b.NumericID)
	}
}

func TestInMemoryFS_StatNotFound(t *testing.T) {
	fs := NewInMemoryFS()
	_, err := fs.Stat(context.Background(), "alice", "/nope.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryFS_ListRootEmpty(t *testing.T) {
	fs := NewInMemoryFS()
	entries, err := fs.List(context.Background(), "alice", "/")
	if err != nil {
		t.Fatalf("List / err = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("List / returned %d entries, want 0", len(entries))
	}
}

func TestInMemoryFS_DistinctUsersDistinctRoots(t *testing.T) {
	fs := NewInMemoryFS()
	a, _ := fs.Stat(context.Background(), "alice", "/")
	b, _ := fs.Stat(context.Background(), "bob", "/")
	if a.NumericID == b.NumericID {
		t.Errorf("alice and bob got same NumericID %d", a.NumericID)
	}
}

func TestInMemoryFS_WriteCreateThenRead(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	entry, created, err := fs.Write(ctx, "alice", "/foo.txt", strings.NewReader("hello"), nil)
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if !created {
		t.Errorf("created = false, want true")
	}
	if entry.Size != 5 {
		t.Errorf("Size = %d, want 5", entry.Size)
	}
	if entry.ETag == "" {
		t.Error("ETag empty")
	}
	if entry.IsDir {
		t.Error("IsDir = true, want false")
	}

	rc, got, err := fs.Read(ctx, "alice", "/foo.txt")
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("data = %q, want hello", data)
	}
	if got.NumericID != entry.NumericID {
		t.Errorf("NumericID mismatch: %d vs %d", got.NumericID, entry.NumericID)
	}
}

func TestInMemoryFS_WriteUpdatePreservesID(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	a, _, _ := fs.Write(ctx, "alice", "/foo.txt", strings.NewReader("v1"), nil)
	b, created, err := fs.Write(ctx, "alice", "/foo.txt", strings.NewReader("v2-longer"), nil)
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if created {
		t.Errorf("created = true on update, want false")
	}
	if a.NumericID != b.NumericID {
		t.Errorf("NumericID changed on update: %d -> %d", a.NumericID, b.NumericID)
	}
	if a.ETag == b.ETag {
		t.Errorf("ETag unchanged across update: %q", a.ETag)
	}
}

func TestInMemoryFS_WriteParentMissing(t *testing.T) {
	fs := NewInMemoryFS()
	_, _, err := fs.Write(context.Background(), "alice", "/nodir/foo.txt", strings.NewReader("x"), nil)
	if !errors.Is(err, ErrParentMissing) {
		t.Errorf("err = %v, want ErrParentMissing", err)
	}
}

func TestInMemoryFS_ReadNotFound(t *testing.T) {
	fs := NewInMemoryFS()
	_, _, err := fs.Read(context.Background(), "alice", "/missing.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryFS_ReadRootIsDir(t *testing.T) {
	fs := NewInMemoryFS()
	_, _, err := fs.Read(context.Background(), "alice", "/")
	if !errors.Is(err, ErrIsDir) {
		t.Errorf("err = %v, want ErrIsDir", err)
	}
}

func TestInMemoryFS_WriteRespectsMtime(t *testing.T) {
	fs := NewInMemoryFS()
	mt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	entry, _, err := fs.Write(context.Background(), "alice", "/foo.txt", strings.NewReader("x"), &mt)
	if err != nil {
		t.Fatalf("Write err = %v", err)
	}
	if !entry.ModTime.Equal(mt) {
		t.Errorf("ModTime = %v, want %v", entry.ModTime, mt)
	}
}

func TestInMemoryFS_ListShowsWritten(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/a.txt", strings.NewReader("a"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/b.txt", strings.NewReader("bb"), nil)
	entries, err := fs.List(ctx, "alice", "/")
	if err != nil {
		t.Fatalf("List err = %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("List returned %d entries, want 2", len(entries))
	}
}

func TestInMemoryFS_MkdirCreates(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	entry, err := fs.Mkdir(ctx, "alice", "/d1")
	if err != nil {
		t.Fatalf("Mkdir err = %v", err)
	}
	if !entry.IsDir {
		t.Error("IsDir = false, want true")
	}
	if entry.Path != "/d1" {
		t.Errorf("Path = %q, want /d1", entry.Path)
	}
	if entry.NumericID == 0 {
		t.Error("NumericID = 0")
	}
	if entry.ContentType != "httpd/unix-directory" {
		t.Errorf("ContentType = %q", entry.ContentType)
	}
	got, err := fs.Stat(ctx, "alice", "/d1")
	if err != nil || got.NumericID != entry.NumericID {
		t.Errorf("Stat after Mkdir mismatch: %v %+v", err, got)
	}
}

func TestInMemoryFS_MkdirExists(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d1")
	_, err := fs.Mkdir(ctx, "alice", "/d1")
	if !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
}

func TestInMemoryFS_MkdirRoot(t *testing.T) {
	fs := NewInMemoryFS()
	_, err := fs.Mkdir(context.Background(), "alice", "/")
	if !errors.Is(err, ErrExists) {
		t.Errorf("err = %v, want ErrExists", err)
	}
}

func TestInMemoryFS_MkdirParentMissing(t *testing.T) {
	fs := NewInMemoryFS()
	_, err := fs.Mkdir(context.Background(), "alice", "/no/d1")
	if !errors.Is(err, ErrParentMissing) {
		t.Errorf("err = %v, want ErrParentMissing", err)
	}
}

func TestInMemoryFS_MkdirParentNotDir(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/file.txt", strings.NewReader("x"), nil)
	_, err := fs.Mkdir(ctx, "alice", "/file.txt/sub")
	if !errors.Is(err, ErrNotDir) {
		t.Errorf("err = %v, want ErrNotDir", err)
	}
}

func TestInMemoryFS_MkdirNested(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, err := fs.Mkdir(ctx, "alice", "/a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fs.Mkdir(ctx, "alice", "/a/b")
	if err != nil {
		t.Fatalf("nested Mkdir err = %v", err)
	}
	_, _, err = fs.Write(ctx, "alice", "/a/b/x.txt", strings.NewReader("hi"), nil)
	if err != nil {
		t.Fatalf("Write under nested dir err = %v", err)
	}
	entries, err := fs.List(ctx, "alice", "/a/b")
	if err != nil {
		t.Fatalf("List nested err = %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/b/x.txt" {
		t.Errorf("List /a/b = %+v", entries)
	}
}

func TestInMemoryFS_ListSubdir(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/sub")
	_, _, _ = fs.Write(ctx, "alice", "/sub/a.txt", strings.NewReader("a"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/sub/b.txt", strings.NewReader("b"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/top.txt", strings.NewReader("t"), nil)
	entries, err := fs.List(ctx, "alice", "/sub")
	if err != nil {
		t.Fatalf("List /sub err = %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("List /sub returned %d, want 2", len(entries))
	}
}

func TestInMemoryFS_ListNotFound(t *testing.T) {
	fs := NewInMemoryFS()
	_, err := fs.List(context.Background(), "alice", "/nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryFS_ListNotDir(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/file.txt", strings.NewReader("x"), nil)
	_, err := fs.List(ctx, "alice", "/file.txt")
	if !errors.Is(err, ErrNotDir) {
		t.Errorf("err = %v, want ErrNotDir", err)
	}
}

func TestInMemoryFS_RemoveFile(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("x"), nil)
	if err := fs.Remove(ctx, "alice", "/x.txt"); err != nil {
		t.Fatalf("Remove err = %v", err)
	}
	_, err := fs.Stat(ctx, "alice", "/x.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat after Remove err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryFS_RemoveDirRecursive(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _ = fs.Mkdir(ctx, "alice", "/d/sub")
	_, _, _ = fs.Write(ctx, "alice", "/d/a.txt", strings.NewReader("a"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/d/sub/b.txt", strings.NewReader("b"), nil)
	if err := fs.Remove(ctx, "alice", "/d"); err != nil {
		t.Fatalf("Remove err = %v", err)
	}
	for _, p := range []string{"/d", "/d/a.txt", "/d/sub", "/d/sub/b.txt"} {
		if _, err := fs.Stat(ctx, "alice", p); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) err = %v, want ErrNotFound", p, err)
		}
	}
}

func TestInMemoryFS_RemoveNotFound(t *testing.T) {
	fs := NewInMemoryFS()
	err := fs.Remove(context.Background(), "alice", "/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryFS_RemoveRoot(t *testing.T) {
	fs := NewInMemoryFS()
	err := fs.Remove(context.Background(), "alice", "/")
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestInMemoryFS_MoveFile(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	orig, _, _ := fs.Write(ctx, "alice", "/x.txt", strings.NewReader("hello"), nil)
	moved, created, err := fs.Move(ctx, "alice", "/x.txt", "alice", "/y.txt", false)
	if err != nil {
		t.Fatalf("Move err = %v", err)
	}
	if !created {
		t.Errorf("created = false, want true (no overwrite)")
	}
	if moved.Path != "/y.txt" {
		t.Errorf("Path = %q, want /y.txt", moved.Path)
	}
	if moved.NumericID != orig.NumericID {
		t.Errorf("NumericID changed across move: %d -> %d", orig.NumericID, moved.NumericID)
	}
	if _, err := fs.Stat(ctx, "alice", "/x.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("source still exists: %v", err)
	}
	rc, _, err := fs.Read(ctx, "alice", "/y.txt")
	if err != nil {
		t.Fatalf("Read /y.txt err = %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("body = %q, want hello", data)
	}
}

func TestInMemoryFS_MoveOverwrite(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("new"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/y.txt", strings.NewReader("old"), nil)

	_, _, err := fs.Move(ctx, "alice", "/x.txt", "alice", "/y.txt", false)
	if !errors.Is(err, ErrExists) {
		t.Errorf("Move w/o overwrite err = %v, want ErrExists", err)
	}

	_, created, err := fs.Move(ctx, "alice", "/x.txt", "alice", "/y.txt", true)
	if err != nil {
		t.Fatalf("Move overwrite err = %v", err)
	}
	if created {
		t.Errorf("created = true, want false (overwrite)")
	}
}

func TestInMemoryFS_MoveDirRecursive(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _, _ = fs.Write(ctx, "alice", "/d/a.txt", strings.NewReader("a"), nil)
	_, _ = fs.Mkdir(ctx, "alice", "/d/sub")
	_, _, _ = fs.Write(ctx, "alice", "/d/sub/b.txt", strings.NewReader("b"), nil)
	if _, _, err := fs.Move(ctx, "alice", "/d", "alice", "/d2", false); err != nil {
		t.Fatalf("Move dir err = %v", err)
	}
	for _, p := range []string{"/d", "/d/a.txt", "/d/sub", "/d/sub/b.txt"} {
		if _, err := fs.Stat(ctx, "alice", p); !errors.Is(err, ErrNotFound) {
			t.Errorf("source %q still exists: %v", p, err)
		}
	}
	for _, p := range []string{"/d2", "/d2/a.txt", "/d2/sub", "/d2/sub/b.txt"} {
		if _, err := fs.Stat(ctx, "alice", p); err != nil {
			t.Errorf("dest %q missing: %v", p, err)
		}
	}
}

func TestInMemoryFS_MoveIntoSelf(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _, err := fs.Move(ctx, "alice", "/d", "alice", "/d/sub", false)
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestInMemoryFS_MoveCrossUser(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("x"), nil)
	_, _, err := fs.Move(ctx, "alice", "/x.txt", "bob", "/x.txt", false)
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestInMemoryFS_MoveParentMissing(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("x"), nil)
	_, _, err := fs.Move(ctx, "alice", "/x.txt", "alice", "/no/y.txt", false)
	if !errors.Is(err, ErrParentMissing) {
		t.Errorf("err = %v, want ErrParentMissing", err)
	}
}

func TestInMemoryFS_CopyFile(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	orig, _, _ := fs.Write(ctx, "alice", "/x.txt", strings.NewReader("hello"), nil)
	cp, created, err := fs.Copy(ctx, "alice", "/x.txt", "alice", "/y.txt", false, true)
	if err != nil {
		t.Fatalf("Copy err = %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	if cp.NumericID == orig.NumericID {
		t.Errorf("Copy reused NumericID %d", cp.NumericID)
	}
	if _, err := fs.Stat(ctx, "alice", "/x.txt"); err != nil {
		t.Errorf("source missing after copy: %v", err)
	}
	rc, _, err := fs.Read(ctx, "alice", "/y.txt")
	if err != nil {
		t.Fatalf("Read /y.txt err = %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("body = %q, want hello", data)
	}
}

func TestInMemoryFS_CopyOverwrite(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("new"), nil)
	_, _, _ = fs.Write(ctx, "alice", "/y.txt", strings.NewReader("old"), nil)
	_, _, err := fs.Copy(ctx, "alice", "/x.txt", "alice", "/y.txt", false, true)
	if !errors.Is(err, ErrExists) {
		t.Errorf("Copy w/o overwrite err = %v, want ErrExists", err)
	}
	_, created, err := fs.Copy(ctx, "alice", "/x.txt", "alice", "/y.txt", true, true)
	if err != nil {
		t.Fatalf("Copy overwrite err = %v", err)
	}
	if created {
		t.Error("created = true, want false (overwrite)")
	}
}

func TestInMemoryFS_CopyDirRecursive(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _, _ = fs.Write(ctx, "alice", "/d/a.txt", strings.NewReader("a"), nil)
	_, _ = fs.Mkdir(ctx, "alice", "/d/sub")
	_, _, _ = fs.Write(ctx, "alice", "/d/sub/b.txt", strings.NewReader("b"), nil)
	if _, _, err := fs.Copy(ctx, "alice", "/d", "alice", "/d2", false, true); err != nil {
		t.Fatalf("Copy dir err = %v", err)
	}
	for _, p := range []string{"/d", "/d/a.txt", "/d/sub", "/d/sub/b.txt"} {
		if _, err := fs.Stat(ctx, "alice", p); err != nil {
			t.Errorf("source %q gone: %v", p, err)
		}
	}
	for _, p := range []string{"/d2", "/d2/a.txt", "/d2/sub", "/d2/sub/b.txt"} {
		if _, err := fs.Stat(ctx, "alice", p); err != nil {
			t.Errorf("dest %q missing: %v", p, err)
		}
	}
}

func TestInMemoryFS_CopyDirShallow(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _, _ = fs.Write(ctx, "alice", "/d/a.txt", strings.NewReader("a"), nil)
	if _, _, err := fs.Copy(ctx, "alice", "/d", "alice", "/d2", false, false); err != nil {
		t.Fatalf("Copy dir shallow err = %v", err)
	}
	if _, err := fs.Stat(ctx, "alice", "/d2"); err != nil {
		t.Errorf("dest dir missing: %v", err)
	}
	if _, err := fs.Stat(ctx, "alice", "/d2/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("shallow copy leaked child: err = %v", err)
	}
}

func TestInMemoryFS_CopyIntoSelf(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _ = fs.Mkdir(ctx, "alice", "/d")
	_, _, err := fs.Copy(ctx, "alice", "/d", "alice", "/d/sub", false, true)
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestInMemoryFS_CopyCrossUser(t *testing.T) {
	fs := NewInMemoryFS()
	ctx := context.Background()
	_, _, _ = fs.Write(ctx, "alice", "/x.txt", strings.NewReader("x"), nil)
	_, _, err := fs.Copy(ctx, "alice", "/x.txt", "bob", "/x.txt", false, true)
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"":       "/",
		"/":      "/",
		"/a":     "/a",
		"a":      "/a",
		"/a/":    "/a",
		"/a/b/":  "/a/b",
		"//":     "/",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
