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
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
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

// hasV2ID1 reports whether the stored file carries the v2 magic and key-ID
// byte 1 (the current key of the two-key rings the rotate tests use).
func hasV2ID1(t *testing.T, inner storage.Storage, p string) bool {
	t.Helper()
	raw := rawBytes(t, inner, p)
	return len(raw) > len(magicV2) && string(raw[:len(magicV2)]) == magicV2 && raw[len(magicV2)] == 1
}

func TestSweepRotateBasic(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB := testKey(t), testKey(t)
	fsA, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{
		"a_old1.bin":  []byte("sealed under the retiring key"),
		"b_old2.bin":  bytes.Repeat([]byte{0xCD}, 1000),
		"c_plain.txt": []byte("legacy plaintext, not rotation's job"),
		"d_fresh.bin": []byte("already sealed under the current key"),
	}
	writeAll(t, fsA, "a_old1.bin", want["a_old1.bin"])
	writeAll(t, fsA, "b_old2.bin", want["b_old2.bin"])
	writeAll(t, inner, "c_plain.txt", want["c_plain.txt"])
	ring, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, ring, "d_fresh.bin", want["d_fresh.bin"])

	stats, err := Sweep(ctx, inner, ring, SweepOptions{Direction: SweepRotate})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 4 || stats.Changed != 2 || stats.Skipped != 2 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Bytes != int64(len(want["a_old1.bin"])+len(want["b_old2.bin"])) {
		t.Errorf("bytes = %d, want the plaintext total of the two rotated files", stats.Bytes)
	}
	for p, data := range want {
		if got := readAll(t, ring, p); !bytes.Equal(got, data) {
			t.Errorf("%s: round trip = %d bytes, want %d", p, len(got), len(data))
		}
	}
	if !hasV2ID1(t, inner, "a_old1.bin") || !hasV2ID1(t, inner, "b_old2.bin") {
		t.Error("old-key files must be re-sealed as v2 under key ID 1")
	}
	if !hasV2ID1(t, inner, "d_fresh.bin") {
		t.Error("current-key file must stay v2 ID 1")
	}
	if hasMagic(t, inner, "c_plain.txt") {
		t.Error("plaintext file must be skipped, not sealed (rotation is not encrypt-all)")
	}

	// Re-running rotates nothing.
	stats, err = Sweep(ctx, inner, ring, SweepOptions{Direction: SweepRotate})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 4 || stats.Changed != 0 || stats.Skipped != 4 || stats.Failed != 0 {
		t.Fatalf("second run stats = %+v", stats)
	}
}

// TestSweepRotateAppendOnlyRule pins ADR-0074's append-only keyring rule:
// after rotation the new key alone cannot read the rotated files — their
// v2 headers name key ID 1, which a single-key ring does not hold.
func TestSweepRotateAppendOnlyRule(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB := testKey(t), testKey(t)
	fsA, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, fsA, "rot.bin", []byte("rotated content"))
	ring, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sweep(ctx, inner, ring, SweepOptions{Direction: SweepRotate}); err != nil {
		t.Fatal(err)
	}
	if !hasV2ID1(t, inner, "rot.bin") {
		t.Fatal("file must be sealed as v2 ID 1 after rotation")
	}
	alone, err := New(keyB, inner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alone.Open(ctx, "rot.bin"); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("single-key ring open err = %v, want ErrUnknownKeyID", err)
	}
}

func TestSweepRotateDryRun(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB := testKey(t), testKey(t)
	fsA, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("dry-run rotation candidate")
	writeAll(t, fsA, "old.bin", content)
	writeAll(t, inner, "plain.txt", []byte("stays plaintext"))
	ring, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := Sweep(ctx, inner, ring, SweepOptions{Direction: SweepRotate, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned != 2 || stats.Changed != 1 || stats.Skipped != 1 || stats.Failed != 0 {
		t.Fatalf("dry-run stats = %+v", stats)
	}
	if stats.Bytes != int64(len(content)) {
		t.Errorf("dry-run bytes = %d, want %d", stats.Bytes, len(content))
	}
	if !hasMagic(t, inner, "old.bin") || hasV2ID1(t, inner, "old.bin") {
		t.Error("dry-run must not rewrite the old-key file")
	}
	if hasMagic(t, inner, "plain.txt") {
		t.Error("dry-run must not touch plaintext")
	}
}

func TestSweepRotateUnknownKeyIDAborts(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB, keyC := testKey(t), testKey(t), testKey(t)
	ring3, err := NewWithPrevious(keyC, [][]byte{keyA, keyB}, inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, ring3, "new.bin", []byte("sealed under key id 2"))
	fsA, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, fsA, "old.bin", []byte("v1 file listed after new.bin"))

	// The ring lost key ID 2 (previous_key_paths shrank): the sweep must
	// abort, not count-and-continue — every remaining old file would fail.
	ring2, err := NewWithPrevious(keyB, [][]byte{keyA}, inner)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(ctx, inner, ring2, SweepOptions{Direction: SweepRotate})
	if !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("err = %v, want ErrUnknownKeyID", err)
	}
	if errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v must not be ErrIntegrity", err)
	}
	// localfs lists in name order: new.bin aborts before old.bin is touched.
	if stats.Scanned != 1 || stats.Changed != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want abort at the first unknown-ID file", stats)
	}
	if !hasMagic(t, inner, "old.bin") || hasV2ID1(t, inner, "old.bin") {
		t.Error("old.bin must remain untouched after the abort")
	}
}

func TestSweepRotateIntegrityAborts(t *testing.T) {
	inner, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	keyA, keyB := testKey(t), testKey(t)
	fsA, err := New(keyA, inner)
	if err != nil {
		t.Fatal(err)
	}
	writeAll(t, fsA, "s1.bin", []byte("first sealed file"))
	writeAll(t, fsA, "s2.bin", []byte("second sealed file"))

	// The ring's key at ID 0 is not the key the files were sealed with:
	// same wrong-key semantics as SweepOpen, abort at the first file.
	ringWrong, err := NewWithPrevious(keyB, [][]byte{testKey(t)}, inner)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Sweep(ctx, inner, ringWrong, SweepOptions{Direction: SweepRotate})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("err = %v must not be ErrUnknownKeyID", err)
	}
	if stats.Scanned != 1 || stats.Changed != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want abort at the first sealed file", stats)
	}
	if !hasMagic(t, inner, "s2.bin") {
		t.Error("s2.bin must remain untouched after the abort")
	}
}

func TestSweepRotateSingleKeyRing(t *testing.T) {
	fs, inner := testFS(t)
	if _, err := Sweep(context.Background(), inner, fs, SweepOptions{Direction: SweepRotate}); err == nil ||
		!strings.Contains(err.Error(), "nothing to rotate") {
		t.Fatalf("single-key ring err = %v", err)
	}
}

// TestSweepOpenLockedSkipped pins the ADR-0100 sweep semantics: a
// principal-less decrypt-all over an enrolled user's v3 tree skips every
// file as locked — never a failure, never an OnError call, exit-clean.
func TestSweepOpenLockedSkipped(t *testing.T) {
	ctx := context.Background()
	db := resolverDB(t)
	seedResolverUser(t, db, "alice")
	fs, inner, res := sqlResolverFS(t, db, testKey(t))
	res.PasswordWrapped = true
	res.KDF = fastKDF

	writeV3(t, fs, "alice/a.txt", []byte("alpha"))
	writeV3(t, fs, "alice/b.txt", []byte("beta"))
	writeAll(t, inner, "alice/plain.txt", []byte("legacy"))
	if _, err := res.UnlockForLogin(ctx, "alice", "wonderland"); err != nil {
		t.Fatal(err)
	}

	var errPaths []string
	stats, err := Sweep(ctx, inner, fs, SweepOptions{
		Direction: SweepOpen,
		OnError:   func(p string, _ error) { errPaths = append(errPaths, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.LockedSkipped != 2 {
		t.Errorf("LockedSkipped = %d, want 2 (both enrolled v3 files)", stats.LockedSkipped)
	}
	if stats.Failed != 0 || len(errPaths) != 0 {
		t.Errorf("Failed = %d, OnError paths = %v, want none (a locked skip is never a failure)", stats.Failed, errPaths)
	}
	if stats.Changed != 0 || stats.Skipped != 1 {
		t.Errorf("Changed = %d, Skipped = %d, want 0/1 (the legacy plaintext file is already 'decrypted')",
			stats.Changed, stats.Skipped)
	}
	// The enrolled files are untouched on disk: still v3-sealed.
	for _, p := range []string{"alice/a.txt", "alice/b.txt"} {
		raw := rawBytes(t, inner, p)
		if !strings.HasPrefix(string(raw), magicV3) {
			t.Errorf("%s was rewritten despite the lock", p)
		}
	}
}
