package encrypt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// SweepDirection selects which way Sweep re-encodes file contents.
type SweepDirection int

const (
	// SweepSeal rewrites legacy plaintext files through the encryption
	// layer, so every file in the tree is sealed at rest.
	SweepSeal SweepDirection = iota
	// SweepOpen rewrites sealed files back to plaintext (decommissioning).
	SweepOpen
	// SweepRotate rewrites files sealed under a retired key ID so they are
	// sealed under the keyring's current key (ADR-0074). Plaintext files
	// are skipped: rotation is not encrypt-all.
	SweepRotate
)

// String names the direction for messages.
func (d SweepDirection) String() string {
	switch d {
	case SweepOpen:
		return "open"
	case SweepRotate:
		return "rotate"
	default:
		return "seal"
	}
}

// SweepOptions controls a Sweep run.
type SweepOptions struct {
	Direction SweepDirection
	// Prefix scopes the sweep to a subtree: "" covers the whole backend,
	// "<uid>/" one user's tree. The trailing slash is optional. A missing
	// subtree is an empty sweep, not an error.
	Prefix string
	// DryRun counts what would change without writing anything.
	DryRun bool
	// Progress, when set, is called after every processed file with the
	// files done so far and the total file count under Prefix (gathered in
	// a list-only pre-pass).
	Progress func(done, total int64)
	// OnError, when set, records every file (or subtree) that failed; the
	// sweep continues past such failures.
	OnError func(path string, err error)
}

// SweepStats summarizes a Sweep run. Bytes counts plaintext bytes rewritten
// (or, in a dry run, that would be rewritten).
type SweepStats struct {
	Scanned int64
	Changed int64
	Skipped int64
	Failed  int64
	Bytes   int64
}

// maxSweepDepth bounds the recursive tree walk: backends cannot nest
// legitimately deeper, and the cap stops symlink cycles on backends that
// follow them.
const maxSweepDepth = 64

// Sweep walks the raw backend tree under opts.Prefix and rewrites every
// file into the target encoding: SweepSeal seals legacy plaintext files in
// place, SweepOpen writes sealed files back as plaintext, SweepRotate
// re-seals files whose header names a retired key ID under the keyring's
// current key. Files already in the target encoding are skipped (empty
// files and files shorter than the magic count as plaintext; for
// SweepRotate plaintext itself is a skip — sealing it is encrypt-all's
// job, which lands on the current key for free), so a sweep is idempotent
// and safe to re-run after an interruption. The walk is serial and holds
// at most one file's content in memory; rewrites go through Create, which
// backends install atomically (localfs temp+rename, s3 put-on-close), so
// concurrent readers always see the old or the new encoding, never a
// partial file.
//
// A per-file failure is counted (and reported via OnError) and the sweep
// continues; a nonzero Failed count does not by itself fail the run. The
// exceptions: in SweepOpen direction an ErrIntegrity read means the master
// key does not match the sealed data (or the file is corrupt); in
// SweepRotate direction an ErrIntegrity read means the same for the ring
// key at the file's key ID, and an ErrUnknownKeyID read means the keyring
// is incomplete. Continuing past any of these would fail every remaining
// sealed file — the sweep aborts at the first such file with an error
// wrapping ErrIntegrity or ErrUnknownKeyID.
func Sweep(ctx context.Context, raw storage.Storage, enc *FS, opts SweepOptions) (SweepStats, error) {
	if raw == nil || enc == nil {
		return SweepStats{}, fmt.Errorf("encrypt: sweep: nil storage")
	}
	if opts.Direction != SweepSeal && opts.Direction != SweepOpen && opts.Direction != SweepRotate {
		return SweepStats{}, fmt.Errorf("encrypt: sweep: unknown direction %d", opts.Direction)
	}
	if opts.Direction == SweepRotate && enc.singleKey() {
		return SweepStats{}, fmt.Errorf("encrypt: sweep: rotate requires a keyring with a previous key (a single-key ring has nothing to rotate)")
	}
	s := sweeper{raw: raw, enc: enc, opts: opts}
	root := strings.TrimSuffix(strings.TrimSpace(opts.Prefix), "/")
	if root == "" {
		root = "."
	}
	if opts.Progress != nil {
		total, err := s.count(ctx, root, 0)
		if err != nil {
			return SweepStats{}, fmt.Errorf("encrypt: sweep: count %s: %w", root, err)
		}
		s.total = total
	}
	if err := s.walk(ctx, root, 0); err != nil {
		return s.stats, err
	}
	return s.stats, nil
}

type sweeper struct {
	raw   storage.Storage
	enc   *FS
	opts  SweepOptions
	total int64
	stats SweepStats
}

// count totals the regular files under dir for the progress denominator.
// It runs before any write, so a list error aborts rather than being
// counted.
func (s *sweeper) count(ctx context.Context, dir string, depth int) (int64, error) {
	if depth > maxSweepDepth {
		return 0, fmt.Errorf("encrypt: sweep: depth cap (%d) at %s", maxSweepDepth, dir)
	}
	infos, err := s.raw.List(ctx, dir)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var n int64
	for _, fi := range infos {
		if fi.IsDir {
			sub, err := s.count(ctx, fi.Path, depth+1)
			if err != nil {
				return 0, err
			}
			n += sub
		} else {
			n++
		}
	}
	return n, nil
}

// walk visits every regular file under dir, recursing one List level at a
// time. Directory-level problems (unlistable subtree, depth cap) are
// counted as one failure each and skipped; the sweep continues elsewhere.
func (s *sweeper) walk(ctx context.Context, dir string, depth int) error {
	if depth > maxSweepDepth {
		s.fail(dir, fmt.Errorf("depth cap (%d) exceeded", maxSweepDepth))
		return nil
	}
	infos, err := s.raw.List(ctx, dir)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.fail(dir, err)
		return nil
	}
	for _, fi := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fi.IsDir {
			if err := s.walk(ctx, fi.Path, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := s.process(ctx, fi); err != nil {
			return err
		}
	}
	return nil
}

// process rewrites one file into the target encoding when it is not
// already there. Per-file failures are counted and reported, not returned;
// the only returned errors are the SweepOpen ErrIntegrity abort and the
// SweepRotate ErrIntegrity/ErrUnknownKeyID aborts.
func (s *sweeper) process(ctx context.Context, fi *storage.FileInfo) error {
	s.stats.Scanned++
	defer s.progress()
	sealed, keyID, err := sniff(ctx, s.raw, fi.Path)
	if err != nil {
		s.fail(fi.Path, err)
		return nil
	}
	rotate := s.opts.Direction == SweepRotate
	skip := sealed == (s.opts.Direction == SweepSeal)
	if rotate {
		skip = !sealed || keyID == s.enc.currentKeyID()
	}
	if skip {
		s.stats.Skipped++
		return nil
	}
	if s.opts.DryRun {
		size := fi.Size
		if s.opts.Direction != SweepSeal {
			// The stored size overstates plaintext by the header and
			// per-chunk tags; Stat computes the real plaintext size.
			plain, err := s.enc.Stat(ctx, fi.Path)
			if err != nil {
				s.fail(fi.Path, err)
				return nil
			}
			size = plain.Size
		}
		s.stats.Changed++
		s.stats.Bytes += size
		return nil
	}
	var data []byte
	dst := storage.Storage(s.enc)
	switch s.opts.Direction {
	case SweepSeal:
		data, err = sweepRead(ctx, s.raw, fi.Path)
	case SweepOpen:
		data, err = sweepRead(ctx, s.enc, fi.Path)
		dst = s.raw
	case SweepRotate:
		// Reading through the encrypt FS auto-detects the header version
		// and picks the ring key; writing through it re-seals under the
		// current key.
		data, err = sweepRead(ctx, s.enc, fi.Path)
	}
	if err != nil {
		if s.opts.Direction == SweepOpen && errors.Is(err, ErrIntegrity) {
			s.stats.Failed++
			return fmt.Errorf("encrypt: sweep: %s: master key does not match the sealed data (or the file is corrupt): %w", fi.Path, err)
		}
		if rotate && errors.Is(err, ErrUnknownKeyID) {
			s.stats.Failed++
			return fmt.Errorf("encrypt: sweep: %s: keyring does not hold the key this file was sealed with (restore the missing previous key): %w", fi.Path, err)
		}
		if rotate && errors.Is(err, ErrIntegrity) {
			s.stats.Failed++
			return fmt.Errorf("encrypt: sweep: %s: the ring key at this file's key id does not match the sealed data (or the file is corrupt): %w", fi.Path, err)
		}
		s.fail(fi.Path, err)
		return nil
	}
	if err := sweepWrite(ctx, dst, fi.Path, data); err != nil {
		s.fail(fi.Path, err)
		return nil
	}
	s.stats.Changed++
	s.stats.Bytes += int64(len(data))
	return nil
}

func (s *sweeper) fail(path string, err error) {
	s.stats.Failed++
	if s.opts.OnError != nil {
		s.opts.OnError(path, err)
	}
}

func (s *sweeper) progress() {
	if s.opts.Progress != nil {
		s.opts.Progress(s.stats.Scanned, s.total)
	}
}

// sniff reports whether the file at p carries the encryption magic and, if
// so, the key ID its header names (v1 headers imply ID 0). Files shorter
// than the magic are plaintext by definition.
func sniff(ctx context.Context, raw storage.Storage, p string) (bool, int, error) {
	rc, err := raw.Open(ctx, p)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, len(magic)+1)
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, 0, err
	}
	keyID, _, ok := headerLayout(head[:n])
	return ok, keyID, nil
}

func sweepRead(ctx context.Context, st storage.Storage, p string) ([]byte, error) {
	rc, err := st.Open(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// sweepWrite replaces p's content through Create. Backends install the new
// content only on Close (localfs rename, s3 put), so a failed Write must
// not be followed by Close: that would commit a truncated file over the
// intact original. Abandoning the writer leaks at most a temp object; the
// failure is counted and reported.
func sweepWrite(ctx context.Context, st storage.Storage, p string, data []byte) error {
	wc, err := st.Create(ctx, p, int64(len(data)))
	if err != nil {
		return err
	}
	if _, err := wc.Write(data); err != nil {
		return err
	}
	return wc.Close()
}
