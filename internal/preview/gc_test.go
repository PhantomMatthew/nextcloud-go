package preview

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
)

const gcTestPrefix = "appdata_ocTestInstance/previews"

func newGCStorage(t *testing.T) (storage.Storage, string) {
	t.Helper()
	root := t.TempDir()
	st, err := localfs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return st, root
}

// writeGCEntry creates one cache entry through the storage API and returns
// its storage path.
func writeGCEntry(t *testing.T, st storage.Storage, name string) string {
	t.Helper()
	p := gcTestPrefix + "/" + name
	wc, err := st.Create(context.Background(), p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = wc.Close()
	return p
}

// ageGCEntry backdates one cache entry 31 days, past the default TTL.
func ageGCEntry(t *testing.T, root, p string) {
	t.Helper()
	old := time.Now().Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(p)), old, old); err != nil {
		t.Fatal(err)
	}
}

func requireGone(t *testing.T, st storage.Storage, p string) {
	t.Helper()
	_, err := st.Stat(context.Background(), p)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat(%s) err = %v, want ErrNotFound", p, err)
	}
}

func requirePresent(t *testing.T, st storage.Storage, p string) {
	t.Helper()
	if _, err := st.Stat(context.Background(), p); err != nil {
		t.Errorf("Stat(%s) err = %v, want present", p, err)
	}
}

func TestGCJobSweep(t *testing.T) {
	st, root := newGCStorage(t)
	oldPath := writeGCEntry(t, st, "old.jpg")
	freshPath := writeGCEntry(t, st, "fresh.jpg")
	if err := st.Mkdir(context.Background(), gcTestPrefix+"/subdir"); err != nil {
		t.Fatal(err)
	}
	// Age the old entry and the subdirectory past the default TTL: the
	// subdir must survive via the IsDir skip, not the modtime one.
	ageGCEntry(t, root, oldPath)
	ageGCEntry(t, root, gcTestPrefix+"/subdir")

	job := NewGCJob(st, gcTestPrefix, 0, nil, slog.New(slog.DiscardHandler))
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	requireGone(t, st, oldPath)
	requirePresent(t, st, freshPath)
	requirePresent(t, st, gcTestPrefix+"/subdir")
}

func TestGCJobMissingPrefix(t *testing.T) {
	st, root := newGCStorage(t)
	now := time.Now().UTC()
	job := NewGCJob(st, gcTestPrefix, 0, func() time.Time { return now }, nil)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run on missing prefix: %v", err)
	}
	// The missing-prefix pass counts as a completed sweep: an aged entry
	// created right after must survive the throttled immediate rerun...
	oldPath := writeGCEntry(t, st, "old.jpg")
	ageGCEntry(t, root, oldPath)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("throttled Run: %v", err)
	}
	requirePresent(t, st, oldPath)
	// ...and a later run past the throttle interval stays a nil no-op.
	now = now.Add(25 * time.Hour)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run after clock advance: %v", err)
	}
	requireGone(t, st, oldPath)
}

func TestGCJobThrottle(t *testing.T) {
	st, root := newGCStorage(t)
	now := time.Now().UTC()
	job := NewGCJob(st, gcTestPrefix, 0, func() time.Time { return now }, nil)

	first := writeGCEntry(t, st, "first.jpg")
	ageGCEntry(t, root, first)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	requireGone(t, st, first)

	// The periodic runner re-enqueues every poll interval: an immediate
	// rerun must not sweep again.
	second := writeGCEntry(t, st, "second.jpg")
	ageGCEntry(t, root, second)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("throttled Run: %v", err)
	}
	requirePresent(t, st, second)

	now = now.Add(25 * time.Hour)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run after clock advance: %v", err)
	}
	requireGone(t, st, second)
}

// stubGCStorage embeds storage.Storage and overrides List/Delete so the
// job's error contract can be tested without a real backend.
type stubGCStorage struct {
	storage.Storage
	infos     []*storage.FileInfo
	listErr   error
	failPaths map[string]bool
	deleted   []string
}

func (s *stubGCStorage) List(_ context.Context, _ string) ([]*storage.FileInfo, error) {
	return s.infos, s.listErr
}

func (s *stubGCStorage) Delete(_ context.Context, p string) error {
	if s.failPaths[p] {
		return errors.New("delete refused")
	}
	s.deleted = append(s.deleted, p)
	return nil
}

func oldGCInfo(p string) *storage.FileInfo {
	return &storage.FileInfo{Path: p, Size: 1, ModTime: time.Now().Add(-31 * 24 * time.Hour)}
}

func TestGCJobDeleteFailureDoesNotFailSweep(t *testing.T) {
	const okPath = gcTestPrefix + "/ok.jpg"
	const badPath = gcTestPrefix + "/bad.jpg"
	st := &stubGCStorage{
		infos:     []*storage.FileInfo{oldGCInfo(okPath), oldGCInfo(badPath)},
		failPaths: map[string]bool{badPath: true},
	}
	job := NewGCJob(st, gcTestPrefix, 0, nil, slog.New(slog.DiscardHandler))
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run with one failing delete: %v", err)
	}
	if !slices.Contains(st.deleted, okPath) {
		t.Errorf("Delete(%s) not attempted; deleted = %v", okPath, st.deleted)
	}
}

func TestGCJobListError(t *testing.T) {
	errBoom := errors.New("list exploded")
	st := &stubGCStorage{listErr: errBoom}
	job := NewGCJob(st, gcTestPrefix, 0, nil, nil)
	if err := job.Run(context.Background(), nil); !errors.Is(err, errBoom) {
		t.Fatalf("Run err = %v, want %v", err, errBoom)
	}
	// A failed List must not set lastRun: the runner's retry is immediate.
	st.listErr = nil
	st.infos = []*storage.FileInfo{oldGCInfo(gcTestPrefix + "/a.jpg")}
	if err := job.Run(context.Background(), nil); err != nil {
		t.Fatalf("retry Run: %v", err)
	}
	if !slices.Contains(st.deleted, gcTestPrefix+"/a.jpg") {
		t.Errorf("retry did not sweep; deleted = %v", st.deleted)
	}
}

func TestGCJobGuards(t *testing.T) {
	if job := NewGCJob(nil, gcTestPrefix, 0, nil, nil); job.Name() != jobs.JobPreviewGC {
		t.Errorf("Name() = %q, want %q", job.Name(), jobs.JobPreviewGC)
	} else if err := job.Run(context.Background(), nil); err != nil {
		t.Errorf("nil cache Run: %v", err)
	}
	st, _ := newGCStorage(t)
	job := NewGCJob(st, "", 0, nil, nil)
	if err := job.Run(context.Background(), nil); err != nil {
		t.Errorf("empty prefix Run: %v", err)
	}
}
