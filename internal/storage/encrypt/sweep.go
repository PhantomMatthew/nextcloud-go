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
)

// String names the direction for messages.
func (d SweepDirection) String() string {
	if d == SweepOpen {
		return "open"
	}
	return "seal"
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
// place, SweepOpen writes sealed files back as plaintext. Files already in
// the target encoding are skipped (empty files and files shorter than the
// magic count as plaintext), so a sweep is idempotent and safe to re-run
// after an interruption. The walk is serial and holds at most one file's
// content in memory; rewrites go through Create, which backends install
// atomically (localfs temp+rename, s3 put-on-close), so concurrent readers
// always see the old or the new encoding, never a partial file.
//
// A per-file failure is counted (and reported via OnError) and the sweep
// continues; a nonzero Failed count does not by itself fail the run. The
// one exception: in SweepOpen direction an ErrIntegrity read means the
// master key does not match the sealed data (or the file is corrupt), and
// continuing would fail every remaining sealed file — the sweep aborts at
// the first such file with an error wrapping ErrIntegrity.
func Sweep(ctx context.Context, raw storage.Storage, enc *FS, opts SweepOptions) (SweepStats, error) {
	if raw == nil || enc == nil {
		return SweepStats{}, fmt.Errorf("encrypt: sweep: nil storage")
	}
	if opts.Direction != SweepSeal && opts.Direction != SweepOpen {
		return SweepStats{}, fmt.Errorf("encrypt: sweep: unknown direction %d", opts.Direction)
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
// the only returned error is the SweepOpen ErrIntegrity abort.
func (s *sweeper) process(ctx context.Context, fi *storage.FileInfo) error {
	s.stats.Scanned++
	defer s.progress()
	sealed, err := sniff(ctx, s.raw, fi.Path)
	if err != nil {
		s.fail(fi.Path, err)
		return nil
	}
	if sealed == (s.opts.Direction == SweepSeal) {
		s.stats.Skipped++
		return nil
	}
	if s.opts.DryRun {
		size := fi.Size
		if s.opts.Direction == SweepOpen {
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
	if s.opts.Direction == SweepSeal {
		data, err = sweepRead(ctx, s.raw, fi.Path)
	} else {
		data, err = sweepRead(ctx, s.enc, fi.Path)
		dst = s.raw
	}
	if err != nil {
		if s.opts.Direction == SweepOpen && errors.Is(err, ErrIntegrity) {
			s.stats.Failed++
			return fmt.Errorf("encrypt: sweep: %s: master key does not match the sealed data (or the file is corrupt): %w", fi.Path, err)
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

// sniff reports whether the file at p carries the encryption magic. Files
// shorter than the magic are plaintext by definition.
func sniff(ctx context.Context, raw storage.Storage, p string) (bool, error) {
	rc, err := raw.Open(ctx, p)
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, len(magic))
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(magic) && string(head) == magic, nil
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
