package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

func writeTreeFile(t *testing.T, st storage.Storage, p, content string) {
	t.Helper()
	ctx := context.Background()
	w, err := st.Create(ctx, p, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTree(t *testing.T) {
	ctx := context.Background()
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTreeFile(t, st, "root/a.txt", "a")
	writeTreeFile(t, st, "root/sub/b.txt", "b")
	writeTreeFile(t, st, "root/sub/deep/c.txt", "c")
	writeTreeFile(t, st, "sibling/keep.txt", "keep")
	if err := deleteTree(ctx, st, "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Stat(ctx, "root"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("root survives: %v", err)
	}
	if _, err := st.Stat(ctx, "sibling/keep.txt"); err != nil {
		t.Fatalf("sibling lost: %v", err)
	}
}

func TestDeleteTreeMissingRoot(t *testing.T) {
	ctx := context.Background()
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteTree(ctx, st, "never/existed"); err != nil {
		t.Fatalf("missing root must be tolerated: %v", err)
	}
}

func TestDeleteTreeDepthCap(t *testing.T) {
	ctx := context.Background()
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// One level past the cap triggers the bound error, never silent
	// truncation.
	var b strings.Builder
	b.WriteString("root")
	for range maxStorageTreeDepth + 2 {
		b.WriteString("/d")
	}
	writeTreeFile(t, st, b.String()+"/leaf.txt", "x")
	err = deleteTree(ctx, st, "root")
	if err == nil || !strings.Contains(err.Error(), "depth cap") {
		t.Fatalf("err = %v", err)
	}
}
