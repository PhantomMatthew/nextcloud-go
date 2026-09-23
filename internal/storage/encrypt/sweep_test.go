package encrypt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// mixedTree writes a fixture tree with plaintext files (nested dirs, an
// empty file, a sub-magic-length file) on the raw backend plus two files
// already sealed through the encryption layer, returning the plaintext
// content expected at every path.
func mixedTree(t *testing.T, fs *FS, inner storage.Storage) map[string][]byte {
	t.Helper()
	plain := map[string][]byte{
		"alice/docs/report.txt": []byte("quarterly report contents"),
		"alice/photos/cat.jpg":  bytes.Repeat([]byte{0xAB}, 1000),
		"alice/empty.txt":       {},
		"alice/tiny.bin":        []byte("abcd"),
		"bob/notes.md":          []byte("# notes\n"),
	}
	for p, data := range plain {
		writeAll(t, inner, p, data)
	}
	sealed := map[string][]byte{
		"alice/sealed.bin": []byte("already encrypted payload"),
		"bob/sealed.txt":   []byte("sealed notes"),
	}
	for p, data := range sealed {
		writeAll(t, fs, p, data)
	}
	want := make(map[string][]byte, len(plain)+len(sealed))
	for p, data := range plain {
		want[p] = data
	}
	for p, data := range sealed {
		want[p] = data
	}
	return want
}

func hasMagic(t *testing.T, inner storage.Storage, p string) bool {
	t.Helper()
	raw := rawBytes(t, inner, p)
	return len(raw) >= len(magic) && string(raw[:len(magic)]) == magic
}

func sumBytes(m map[string][]byte) int64 {
	var n int64
	for _, data := range m {
		n += int64(len(data))
	}
	return n
}

func TestSweepSealMixedTree(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	want := mixedTree(t, fs, inner)

	var progressCalls int64
	var lastDone, lastTotal int64
	stats, err := Sweep(ctx, inner, fs, SweepOptions{
		Direction: SweepSeal,
		Progress: func(done, total int64) {
			progressCalls++
			lastDone, lastTotal = done, total
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 5 || stats.Skipped != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Bytes != sumBytes(mixedTreePlainOnly(want)) {
		t.Errorf("bytes = %d, want plaintext total of the 5 changed files", stats.Bytes)
	}
	if progressCalls != 7 || lastDone != 7 || lastTotal != 7 {
		t.Errorf("progress = %d calls, last %d/%d", progressCalls, lastDone, lastTotal)
	}

	for p, data := range want {
		if !hasMagic(t, inner, p) {
			t.Errorf("%s: not sealed after sweep", p)
		}
		if got := readAll(t, fs, p); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip = %d bytes, want %d", p, len(got), len(data))
		}
	}
	// The sealed empty file stores the header only.
	raw, err := inner.Stat(ctx, "alice/empty.txt")
	if err != nil {
		t.Fatal(err)
	}
	if raw.Size != int64(headerSize) {
		t.Errorf("sealed empty file = %d bytes, want header only (%d)", raw.Size, headerSize)
	}
}

// mixedTreePlainOnly drops the two paths that were sealed from the start.
func mixedTreePlainOnly(want map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(want))
	for p, data := range want {
		if p != "alice/sealed.bin" && p != "bob/sealed.txt" {
			out[p] = data
		}
	}
	return out
}

func TestSweepSealIdempotent(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	mixedTree(t, fs, inner)

	if _, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal}); err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 0 || stats.Skipped != 7 || stats.Failed != 0 || stats.Bytes != 0 {
		t.Fatalf("second sweep stats = %+v", stats)
	}
}

func TestSweepOpenSymmetric(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	want := mixedTree(t, fs, inner)

	if _, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal}); err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepOpen})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 7 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("open stats = %+v", stats)
	}
	if stats.Bytes != sumBytes(want) {
		t.Errorf("bytes = %d, want %d", stats.Bytes, sumBytes(want))
	}
	for p, data := range want {
		if hasMagic(t, inner, p) {
			t.Errorf("%s: still sealed after open sweep", p)
		}
		if got := rawBytes(t, inner, p); !bytes.Equal(got, data) {
			t.Errorf("%s: raw content = %q, want %q", p, got, data)
		}
	}

	// A second open sweep skips everything.
	stats, err = Sweep(ctx, inner, fs, SweepOptions{Direction: SweepOpen})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Changed != 0 || stats.Skipped != 7 {
		t.Fatalf("second open sweep stats = %+v", stats)
	}
}

func TestSweepPrefix(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	mixedTree(t, fs, inner)

	stats, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal, Prefix: "alice/"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 5 || stats.Changed != 4 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("prefix stats = %+v", stats)
	}
	for _, p := range []string{"alice/docs/report.txt", "alice/photos/cat.jpg", "alice/empty.txt", "alice/tiny.bin"} {
		if !hasMagic(t, inner, p) {
			t.Errorf("%s: not sealed", p)
		}
	}
	if hasMagic(t, inner, "bob/notes.md") {
		t.Error("bob/notes.md: outside the prefix, must stay plaintext")
	}

	// A missing subtree is an empty sweep, not an error.
	stats, err = Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal, Prefix: "carol/"})
	if err != nil {
		t.Fatal(err)
	}
	if stats != (SweepStats{}) {
		t.Fatalf("missing prefix stats = %+v", stats)
	}
}

func TestSweepDryRun(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	want := mixedTree(t, fs, inner)
	plain := mixedTreePlainOnly(want)

	stats, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 5 || stats.Skipped != 2 || stats.Failed != 0 {
		t.Fatalf("dry-run seal stats = %+v", stats)
	}
	if stats.Bytes != sumBytes(plain) {
		t.Errorf("dry-run bytes = %d, want %d", stats.Bytes, sumBytes(plain))
	}
	for _, p := range []string{"alice/docs/report.txt", "bob/notes.md", "alice/tiny.bin"} {
		if hasMagic(t, inner, p) {
			t.Errorf("%s: dry-run must not write", p)
		}
	}

	// Seal for real, then dry-run the open direction.
	if _, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal}); err != nil {
		t.Fatal(err)
	}
	stats, err = Sweep(ctx, inner, fs, SweepOptions{Direction: SweepOpen, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 7 || stats.Skipped != 0 {
		t.Fatalf("dry-run open stats = %+v", stats)
	}
	if stats.Bytes != sumBytes(want) {
		t.Errorf("dry-run open bytes = %d, want plaintext total %d", stats.Bytes, sumBytes(want))
	}
	for p := range want {
		if !hasMagic(t, inner, p) {
			t.Errorf("%s: dry-run open must not write", p)
		}
	}
}

func TestSweepOpenWrongKeyAborts(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	writeAll(t, fs, "s1.bin", []byte("first sealed file"))
	writeAll(t, fs, "s2.bin", []byte("second sealed file"))
	writeAll(t, inner, "z_plain.txt", []byte("plaintext after the sealed files"))

	other, err := New(testKey(t), inner)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(ctx, inner, other, SweepOptions{Direction: SweepOpen})
	if err == nil {
		t.Fatal("wrong-key open sweep must fail")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if !strings.Contains(err.Error(), "master key") {
		t.Fatalf("err = %v, want a key-mismatch hint", err)
	}
	// localfs lists in name order: s1.bin aborts before s2/z_plain are
	// touched.
	if stats.Scanned != 1 || stats.Changed != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want abort at the first sealed file", stats)
	}
	if !hasMagic(t, inner, "s2.bin") {
		t.Error("s2.bin must remain untouched after the abort")
	}
}

// failStorage wraps a backend, failing Open or Create on chosen paths to
// simulate per-file faults.
type failStorage struct {
	storage.Storage
	openErr   map[string]error
	createErr map[string]error
}

func (f failStorage) Open(ctx context.Context, p string) (io.ReadSeekCloser, error) {
	if err, ok := f.openErr[p]; ok {
		return nil, err
	}
	return f.Storage.Open(ctx, p)
}

func (f failStorage) Create(ctx context.Context, p string, size int64) (io.WriteCloser, error) {
	if err, ok := f.createErr[p]; ok {
		return nil, err
	}
	return f.Storage.Create(ctx, p, size)
}

func TestSweepSingleFileErrorContinues(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	mixedTree(t, fs, inner)

	broken := errors.New("injected read fault")
	raw := failStorage{Storage: inner, openErr: map[string]error{"bob/notes.md": broken}}
	var failedPaths []string
	stats, err := Sweep(ctx, raw, fs, SweepOptions{
		Direction: SweepSeal,
		OnError: func(path string, err error) {
			failedPaths = append(failedPaths, fmt.Sprintf("%s: %v", path, err))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 4 || stats.Skipped != 2 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(failedPaths) != 1 || !strings.Contains(failedPaths[0], "bob/notes.md") {
		t.Fatalf("OnError records = %v", failedPaths)
	}
	if hasMagic(t, inner, "bob/notes.md") {
		t.Error("failed file must stay plaintext")
	}
	if !hasMagic(t, inner, "alice/docs/report.txt") {
		t.Error("other files must be sealed despite the failure")
	}

	// Seal the rest, then fail the plaintext write of one file during the
	// open sweep: also counted, not fatal.
	if _, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal}); err != nil {
		t.Fatal(err)
	}
	raw = failStorage{Storage: inner, createErr: map[string]error{"bob/notes.md": broken}}
	stats, err = Sweep(ctx, raw, fs, SweepOptions{Direction: SweepOpen})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 7 || stats.Changed != 6 || stats.Failed != 1 || stats.Skipped != 0 {
		t.Fatalf("create-failure stats = %+v", stats)
	}
	if !hasMagic(t, inner, "bob/notes.md") {
		t.Error("failed file must stay sealed")
	}
}

func TestSweepContextCancel(t *testing.T) {
	fs, inner := testFS(t)
	mixedTree(t, fs, inner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepSeal})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if stats.Scanned != 0 {
		t.Fatalf("scanned = %d after pre-cancelled context", stats.Scanned)
	}
}

func TestSweepValidation(t *testing.T) {
	fs, inner := testFS(t)
	ctx := context.Background()
	if _, err := Sweep(ctx, nil, fs, SweepOptions{Direction: SweepSeal}); err == nil {
		t.Fatal("nil raw storage must fail")
	}
	if _, err := Sweep(ctx, inner, nil, SweepOptions{Direction: SweepSeal}); err == nil {
		t.Fatal("nil encrypt FS must fail")
	}
	if _, err := Sweep(ctx, inner, fs, SweepOptions{Direction: SweepDirection(7)}); err == nil {
		t.Fatal("unknown direction must fail")
	}
}
