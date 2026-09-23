package localfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

func TestLocalFSRoundTrip(t *testing.T) {
	root := t.TempDir()
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := fs.Mkdir(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
	w, err := fs.Create(ctx, "dir/file.txt", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "data"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := fs.Stat(ctx, "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 4 || st.IsDir {
		t.Fatalf("%+v", st)
	}
	r, err := fs.Open(ctx, "dir/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if string(body) != "data" {
		t.Fatalf("body %q", body)
	}
	ents, err := fs.List(ctx, "dir")
	if err != nil || len(ents) != 1 {
		t.Fatalf("list = %v %v", ents, err)
	}
	if err := fs.Rename(ctx, "dir/file.txt", "dir/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "dir/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalFSRejectsEscape(t *testing.T) {
	root := t.TempDir()
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []string{"../outside", "/etc/passwd", ".."} {
		if _, err := fs.Stat(ctx, p); !errors.Is(err, storage.ErrInvalidPath) {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestLocalFSRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fs.Open(ctx, "link"); !errors.Is(err, storage.ErrInvalidPath) {
		t.Fatalf("symlink open = %v", err)
	}
}

func TestLocalFSErrors(t *testing.T) {
	root := t.TempDir()
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := fs.Stat(ctx, "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stat missing = %v", err)
	}
	if err := fs.Mkdir(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Mkdir(ctx, "d"); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("mkdir exists = %v", err)
	}
	if err := fs.Mkdir(ctx, "missing/child"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("mkdir parent missing = %v, want ErrNotFound", err)
	}
	w, err := fs.Create(ctx, "d/a.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, "d"); !errors.Is(err, storage.ErrNotEmpty) {
		t.Fatalf("not empty = %v", err)
	}
	if _, err := fs.Open(ctx, "d"); !errors.Is(err, storage.ErrIsDir) {
		t.Fatalf("open dir = %v", err)
	}
	if _, err := fs.List(ctx, "d/a.txt"); !errors.Is(err, storage.ErrNotDir) {
		t.Fatalf("list file = %v", err)
	}
}
