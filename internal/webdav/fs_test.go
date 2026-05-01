package webdav

import (
	"context"
	"errors"
	"testing"
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
