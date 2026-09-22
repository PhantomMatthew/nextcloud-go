package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// ncInternalTopDirs are datadir top-level directories that are never user
// homes; anything else with a files/ subdirectory is treated as a user dir.
const ncAppDataPrefix = "appdata_"

type importNCFilesFlags struct {
	datadir string
	users   []string
	verbose bool
}

func newImportNCFiles(f *importNCFlags) *cobra.Command {
	ff := &importNCFilesFlags{}
	cmd := &cobra.Command{
		Use:   "files",
		Short: "Import user files from a Nextcloud data directory",
		Long: "Import user files from a PHP Nextcloud data directory into the\n" +
			"configured ncgo storage backend and filecache.\n\n" +
			"The source database flags are NOT needed here: Nextcloud's filecache\n" +
			"table is only a cache — the data directory is canonical for content\n" +
			"(and its mtimes), so the import walks <datadir>/<uid>/files directly.\n" +
			"Sibling Nextcloud-internal directories (files_trashbin, files_versions,\n" +
			"uploads, cache, thumbnails) and non-user top-level entries (appdata_*,\n" +
			"files_external, logs) are never descended into; dotfiles inside the\n" +
			"tree are real user files and are imported.\n\n" +
			"Precondition: the target user must already exist (run\n" +
			"'import-nextcloud users' first); unknown users are skipped with a\n" +
			"warning. Users with a files_encryption directory are skipped —\n" +
			"server-side encrypted sources are not supported.\n\n" +
			"The import is idempotent and resumable: a target file with the same\n" +
			"size and mtime (second resolution, matching Nextcloud's unix-second\n" +
			"mtimes) is skipped unchanged, which also avoids spurious version\n" +
			"snapshots on re-import; a file that differs is rewritten through the\n" +
			"DAV pipeline (which snapshots the previous version, exactly like a\n" +
			"client overwrite). Per-file errors are counted and warned but never\n" +
			"abort the run. With --dry-run the tree is walked and counted against\n" +
			"the target but nothing is written.",
		Args: cobra.NoArgs,
		// files needs no source database: it has its own hook so the parent's
		// --source-driver/--source-dsn validation does not apply.
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			if ff.datadir == "" {
				return fmt.Errorf("ncgo-cli: --datadir is required")
			}
			info, err := os.Stat(ff.datadir)
			if err != nil {
				return fmt.Errorf("ncgo-cli: --datadir: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("ncgo-cli: --datadir %q is not a directory", ff.datadir)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := openDB(ctx, cfg)
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()
			st, err := openStorage(cfg)
			if err != nil {
				return err
			}
			dav := filesDAV(st, db)
			r := newImportReport()
			if err := importNCFiles(ctx, ff.datadir, dav, dav.Users, ff.users, f.dryRun, ff.verbose, r, cmd.OutOrStdout()); err != nil {
				return err
			}
			return r.print(cmd.OutOrStdout(), f.dryRun)
		},
	}
	cmd.Flags().StringVar(&ff.datadir, "datadir", "", "PHP Nextcloud data directory (required)")
	cmd.Flags().StringArrayVar(&ff.users, "user", nil, "import only this user (repeatable; default: all users found under --datadir)")
	cmd.Flags().BoolVar(&ff.verbose, "verbose", false, "print one line per imported file")
	return cmd
}

// importNCFiles walks <datadir>/<uid>/files per selected user and ingests
// every entry through dav, so writes get filecache bookkeeping, checksums,
// mtime preservation, and version snapshots for free. It is best-effort:
// per-file failures warn and continue.
func importNCFiles(ctx context.Context, datadir string, dav *files.DAV, us users.Store, uids []string, dryRun, verbose bool, r *importReport, out io.Writer) error {
	if len(uids) == 0 {
		found, err := discoverNCUsers(datadir)
		if err != nil {
			return err
		}
		uids = found
	}
	uc := r.entity("users")
	for _, uid := range uids {
		if !validNCImportUID(uid) {
			r.warn("user %s: uid not valid for ncgo, skipped", uid)
			uc.skipped++
			continue
		}
		u, err := us.GetByUID(ctx, uid)
		if err != nil {
			if errors.Is(err, users.ErrNotFound) {
				r.warn("user %s: not in target (run 'import-nextcloud users' first), skipped", uid)
				uc.skipped++
				continue
			}
			return fmt.Errorf("import files: lookup user %s: %w", uid, err)
		}
		if info, err := os.Stat(filepath.Join(datadir, uid, "files_encryption")); err == nil && info.IsDir() {
			r.warn("user %s: server-side encrypted source not supported, user skipped", uid)
			uc.skipped++
			continue
		}
		home := filepath.Join(datadir, uid, "files")
		if info, err := os.Stat(home); err != nil || !info.IsDir() {
			r.warn("user %s: no files directory in the data directory, skipped", uid)
			uc.skipped++
			continue
		}
		stats, err := importNCUserFiles(ctx, home, dav, uid, u.ID, dryRun, verbose, r, out)
		if err != nil {
			r.warn("user %s: %v", uid, err)
			uc.failed++
			continue
		}
		uc.created++
		if _, err := fmt.Fprintf(out, "user %s: %d files created, %d updated, %d skipped, %d failed, %d directories created\n",
			uid, stats.created, stats.updated, stats.skipped, stats.failed, stats.dirsCreated); err != nil {
			return err
		}
	}
	return nil
}

// discoverNCUsers returns the sorted datadir entries that look like user
// homes: directories (not appdata_*) containing a files/ subdirectory.
func discoverNCUsers(datadir string) ([]string, error) {
	entries, err := os.ReadDir(datadir)
	if err != nil {
		return nil, fmt.Errorf("import files: read data directory: %w", err)
	}
	var uids []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ncAppDataPrefix) {
			continue
		}
		if info, err := os.Stat(filepath.Join(datadir, e.Name(), "files")); err == nil && info.IsDir() {
			uids = append(uids, e.Name())
		}
	}
	sort.Strings(uids)
	return uids, nil
}

// userFileStats accumulates one user's counts for the progress line.
type userFileStats struct {
	created     int
	updated     int
	skipped     int
	failed      int
	dirsCreated int
}

// importNCUserFiles imports home (the user's <uid>/files tree) through dav.
// Target existence checks go through the read-only filecache (dav.Meta)
// rather than dav.Stat, because dav.Stat lazily creates the user's storage
// home and filecache root — a write that --dry-run must not perform.
func importNCUserFiles(ctx context.Context, home string, dav *files.DAV, uid string, userID int64, dryRun, verbose bool, r *importReport, out io.Writer) (*userFileStats, error) {
	fc := r.entity("files")
	dc := r.entity("directories")
	stats := &userFileStats{}
	err := filepath.WalkDir(home, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			r.warn("user %s: %s: %v", uid, p, walkErr)
			fc.failed++
			stats.failed++
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(home, p)
		if err != nil || rel == "." {
			return err
		}
		target := "/" + filepath.ToSlash(rel)
		switch {
		case entry.IsDir():
			if err := importNCDir(ctx, dav, uid, userID, target, dryRun, dc, stats); err != nil {
				r.warn("user %s: %s: %v", uid, target, err)
				dc.failed++
				stats.failed++
				return filepath.SkipDir
			}
		case entry.Type()&os.ModeSymlink != 0:
			r.warn("user %s: %s: symlink skipped", uid, target)
			fc.skipped++
			stats.skipped++
		case !entry.Type().IsRegular():
			r.warn("user %s: %s: special file skipped", uid, target)
			fc.skipped++
			stats.skipped++
		default:
			importNCFile(ctx, p, target, dav, uid, userID, dryRun, verbose, r, out, fc, stats)
		}
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("walk %s: %w", home, err)
	}
	return stats, nil
}

// importNCDir ensures the target collection exists.
func importNCDir(ctx context.Context, dav *files.DAV, uid string, userID int64, target string, dryRun bool, dc *importCounts, stats *userFileStats) error {
	f, err := dav.Meta.GetByPath(ctx, userID, target)
	if err == nil {
		if !f.IsDir {
			return fmt.Errorf("target exists and is not a directory")
		}
		dc.skipped++
		return nil
	}
	if !errors.Is(err, files.ErrNotFound) {
		return err
	}
	if dryRun {
		dc.created++
		stats.dirsCreated++
		return nil
	}
	if _, err := dav.Mkdir(ctx, uid, target); err != nil && !errors.Is(err, webdav.ErrExists) {
		return err
	}
	dc.created++
	stats.dirsCreated++
	return nil
}

// importNCFile imports one regular file, skipping it when the target already
// has the same size and mtime (second resolution).
func importNCFile(ctx context.Context, src, target string, dav *files.DAV, uid string, userID int64, dryRun, verbose bool, r *importReport, out io.Writer, fc *importCounts, stats *userFileStats) {
	info, err := os.Stat(src)
	if err != nil {
		r.warn("user %s: %s: %v", uid, target, err)
		fc.failed++
		stats.failed++
		return
	}
	// Nextcloud filecache mtimes are unix seconds; truncate so the
	// skip-if-unchanged comparison is stable across filesystems.
	mtime := info.ModTime().Truncate(time.Second).UTC()
	f, statErr := dav.Meta.GetByPath(ctx, userID, target)
	switch {
	case statErr == nil && f.IsDir:
		r.warn("user %s: %s: target is a directory, file skipped", uid, target)
		fc.failed++
		stats.failed++
		return
	case statErr == nil && f.Size == info.Size() && f.Mtime.Unix() == mtime.Unix():
		fc.skipped++
		stats.skipped++
		return
	case statErr != nil && !errors.Is(statErr, files.ErrNotFound):
		r.warn("user %s: %s: stat target: %v", uid, target, statErr)
		fc.failed++
		stats.failed++
		return
	}
	verb := "create"
	if statErr == nil {
		verb = "update"
	}
	if verbose {
		if _, err := fmt.Fprintf(out, "user %s: %s %s (%d bytes)\n", uid, verb, target, info.Size()); err != nil {
			r.warn("user %s: %s: %v", uid, target, err)
		}
	}
	if dryRun {
		if verb == "create" {
			fc.created++
			stats.created++
		} else {
			fc.updated++
			stats.updated++
		}
		return
	}
	fh, err := os.Open(src)
	if err != nil {
		r.warn("user %s: %s: %v", uid, target, err)
		fc.failed++
		stats.failed++
		return
	}
	_, created, writeErr := dav.Write(ctx, uid, target, fh, &mtime)
	closeErr := fh.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		r.warn("user %s: %s: %v", uid, target, writeErr)
		fc.failed++
		stats.failed++
		return
	}
	if created {
		fc.created++
		stats.created++
		return
	}
	fc.updated++
	stats.updated++
}
