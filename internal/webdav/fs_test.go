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
