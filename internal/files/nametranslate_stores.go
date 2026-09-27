package files

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// TranslatingStore decorates the files Store with ADR-0104 filename
// translation: callers keep speaking plaintext paths, the wrapped SQLStore
// sees only ciphertext-materialized paths. Writes encrypt when the owner's
// users.name_scheme is 1 (the authoritative write switch); reads dual-read on
// each row's own files.name_scheme marker, so mixed trees stay readable.
// With no wrapper wired (the flag off) every row stays bit-identical.
type TranslatingStore struct {
	raw *SQLStore
	t   *NameTranslator
}

// NewTranslatingStore wraps raw with filename translation driven by t (shared
// with the satellite wrappers — the translator itself is stateless across
// calls, all caches are per-call).
func NewTranslatingStore(raw *SQLStore, t *NameTranslator) *TranslatingStore {
	return &TranslatingStore{raw: raw, t: t}
}

// Raw returns the wrapped store (wiring audits and tests).
func (s *TranslatingStore) Raw() *SQLStore { return s.raw }

// EnsureRoot passes through: the root (path "/", name "") is never encrypted.
func (s *TranslatingStore) EnsureRoot(ctx context.Context, userID int64) (*File, error) {
	return s.raw.EnsureRoot(ctx, userID)
}

func (s *TranslatingStore) GetByPath(ctx context.Context, userID int64, p string) (*File, error) {
	c := newTranslateCache()
	cp, err := s.t.cipherPath(ctx, c, userID, p)
	if err != nil {
		return nil, err
	}
	f, err := s.raw.GetByPath(ctx, userID, cp)
	if err != nil {
		return nil, err
	}
	if err := s.t.decryptRow(ctx, c, f); err != nil {
		return nil, err
	}
	return f, nil
}

func (s *TranslatingStore) GetByID(ctx context.Context, id int64) (*File, error) {
	c := newTranslateCache()
	f, err := s.raw.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.t.decryptRow(ctx, c, f); err != nil {
		return nil, err
	}
	return f, nil
}

// ListChildren decrypts the children of parentID and re-sorts by plaintext
// (Name, ID): ORDER BY path over ciphertext tokens is meaningless, and the
// Go-side sort unifies listing order across dialects (ADR-0104 §4). A
// scheme-0 user short-circuits to the raw query untouched (bit-identical).
func (s *TranslatingStore) ListChildren(ctx context.Context, userID, parentID int64) ([]File, error) {
	c := newTranslateCache()
	scheme, err := s.t.schemeFor(ctx, c, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.raw.ListChildren(ctx, userID, parentID)
	if err != nil {
		return nil, err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return rows, nil
	}
	if err := s.t.decryptRows(ctx, c, rows); err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

// Insert tokens the row's basename under its parent folder's key and stores
// the ciphertext name/path; KeyUUID (a phase-1 minted DK or FK) passes
// through untouched. Rows under a scheme-0 user insert plaintext. The
// caller's struct is restored to the plaintext view on success.
func (s *TranslatingStore) Insert(ctx context.Context, f *File) error {
	if f == nil || f.UserID == 0 {
		return s.raw.Insert(ctx, f) // the raw validation error
	}
	np, err := NormalizePath(f.Path)
	if err != nil {
		return err
	}
	if np == "/" {
		return s.raw.Insert(ctx, f) // the root is never encrypted
	}
	c := newTranslateCache()
	scheme, err := s.t.schemeFor(ctx, c, f.UserID)
	if err != nil {
		return err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return s.raw.Insert(ctx, f)
	}
	parent, err := s.cipherParent(ctx, c, f.UserID, np)
	if err != nil {
		return err
	}
	name := f.Name
	if name == "" {
		name = path.Base(np)
	}
	tok, err := s.t.cipherSegment(ctx, c, parent, name)
	if err != nil {
		return err
	}
	ctPath, err := joinTokenPath(parent.Path, tok)
	if err != nil {
		return err
	}
	f.Name = tok
	f.Path = ctPath
	f.NameScheme = encrypt.NameSchemeNCGOFN1
	if err := s.raw.Insert(ctx, f); err != nil {
		return err
	}
	// The raw insert re-read the row (ciphertext); hand the caller back the
	// plaintext view this boundary speaks.
	f.Name = name
	f.Path = np
	return nil
}

// cipherParent resolves the ciphertext parent row of a plaintext path,
// mapping a missing parent to ErrParentMissing (the raw Insert's semantics).
func (s *TranslatingStore) cipherParent(ctx context.Context, c *translateCache, userID int64, np string) (*File, error) {
	ctParent, err := s.t.cipherPath(ctx, c, userID, dirPath(np))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrParentMissing
		}
		return nil, err
	}
	parent, err := s.raw.GetByPath(ctx, userID, ctParent)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrParentMissing
		}
		return nil, err
	}
	if !parent.IsDir {
		return nil, ErrNotDir
	}
	return parent, nil
}

// UpdateMeta re-encrypts Name/Path from the plaintext caller view, exactly
// like Insert: callers pass rows they read through this boundary, which are
// decrypted. Dual-write mirrors dual-read — a scheme-0 row under a scheme-1
// user is written back plaintext.
func (s *TranslatingStore) UpdateMeta(ctx context.Context, f *File) error {
	f, undo, err := s.reCipherRow(ctx, f)
	if err != nil {
		return err
	}
	err = s.raw.UpdateMeta(ctx, f)
	undo()
	return err
}

// UpdateMetaIfETag is UpdateMeta with the raw store's etag guard.
func (s *TranslatingStore) UpdateMetaIfETag(ctx context.Context, f *File, expectETag string) error {
	f, undo, err := s.reCipherRow(ctx, f)
	if err != nil {
		return err
	}
	err = s.raw.UpdateMetaIfETag(ctx, f, expectETag)
	undo()
	return err
}

// reCipherRow converts a plaintext-view row to its ciphertext form for a
// meta update, returning the row to hand the raw store (the same pointer)
// and an undo restoring the plaintext view. Scheme-0 rows and the root pass
// through with a no-op undo.
func (s *TranslatingStore) reCipherRow(ctx context.Context, f *File) (*File, func(), error) {
	noop := func() {}
	if f == nil || f.ID == 0 {
		return f, noop, nil // the raw store reports the invalid row
	}
	np, err := NormalizePath(f.Path)
	if err != nil {
		return nil, noop, err
	}
	if np == "/" || f.NameScheme != encrypt.NameSchemeNCGOFN1 {
		return f, noop, nil
	}
	if f.ParentID == nil {
		return nil, noop, errors.New("files: non-root row without parent_id")
	}
	c := newTranslateCache()
	parent, err := s.raw.GetByID(ctx, *f.ParentID)
	if err != nil {
		return nil, noop, err
	}
	name := f.Name
	if name == "" {
		name = path.Base(np)
	}
	tok, err := s.t.cipherSegment(ctx, c, parent, name)
	if err != nil {
		return nil, noop, err
	}
	ctPath, err := joinTokenPath(parent.Path, tok)
	if err != nil {
		return nil, noop, err
	}
	f.Name = tok
	f.Path = ctPath
	return f, func() {
		f.Name = name
		f.Path = np
	}, nil
}

// RenameSubtree re-tokens the moved entry's basename under the DESTINATION
// parent's key and hands the raw store two ciphertext paths, so the internal
// prefix rewrite is pure string work on tokens (ADR-0104 §5): descendants'
// name tokens are keyed to their own folders and stay unchanged — one crypto
// step covers both same-parent rename and cross-parent move.
func (s *TranslatingStore) RenameSubtree(ctx context.Context, userID int64, srcPath, dstPath string, now time.Time) error {
	srcN, err := NormalizePath(srcPath)
	if err != nil {
		return err
	}
	dstN, err := NormalizePath(dstPath)
	if err != nil {
		return err
	}
	if srcN == "/" || dstN == "/" {
		// The raw store owns the root guard (ErrForbidden); a ciphertext
		// destination is never literally "/", so the check must happen here.
		return s.raw.RenameSubtree(ctx, userID, srcN, dstN, now)
	}
	c := newTranslateCache()
	scheme, err := s.t.schemeFor(ctx, c, userID)
	if err != nil {
		return err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return s.raw.RenameSubtree(ctx, userID, srcN, dstN, now)
	}
	ctSrc, err := s.t.cipherPath(ctx, c, userID, srcN)
	if err != nil {
		return err
	}
	dstParent, err := s.cipherParent(ctx, c, userID, dstN)
	if err != nil {
		return err
	}
	tok, err := s.t.cipherSegment(ctx, c, dstParent, path.Base(dstN))
	if err != nil {
		return err
	}
	ctDst, err := joinTokenPath(dstParent.Path, tok)
	if err != nil {
		return err
	}
	return s.raw.RenameSubtree(ctx, userID, ctSrc, ctDst, now)
}

func (s *TranslatingStore) DeleteSubtree(ctx context.Context, userID int64, p string) error {
	c := newTranslateCache()
	cp, err := s.t.cipherPath(ctx, c, userID, p)
	if err != nil {
		return err
	}
	return s.raw.DeleteSubtree(ctx, userID, cp)
}

// ListSealedSubtree translates the path argument and decrypts the sealed rows
// (KeySharer consumes only KeyUUID, but the rows keep the plaintext view this
// boundary speaks). Order stays the raw ciphertext order — no consumer sorts.
func (s *TranslatingStore) ListSealedSubtree(ctx context.Context, userID int64, p string) ([]File, error) {
	c := newTranslateCache()
	cp, err := s.t.cipherPath(ctx, c, userID, p)
	if err != nil {
		return nil, err
	}
	rows, err := s.raw.ListSealedSubtree(ctx, userID, cp)
	if err != nil {
		return nil, err
	}
	if err := s.t.decryptRows(ctx, c, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// SetKeyUUIDIfNull passes through (id-addressed; the lazy root DK claim).
func (s *TranslatingStore) SetKeyUUIDIfNull(ctx context.Context, fileID int64, keyUUID []byte) (bool, error) {
	return s.raw.SetKeyUUIDIfNull(ctx, fileID, keyUUID)
}

// Usage passes through (no paths involved).
func (s *TranslatingStore) Usage(ctx context.Context, userID int64) (int64, error) {
	return s.raw.Usage(ctx, userID)
}

// RecalcAncestors passes through: it walks parent ids and recomputes sizes
// and directory etags from child etags, all name-agnostic. The raw store's
// internal rows stay ciphertext end to end, so no decryption is needed.
func (s *TranslatingStore) RecalcAncestors(ctx context.Context, userID int64, startParent *int64, now time.Time) error {
	return s.raw.RecalcAncestors(ctx, userID, startParent, now)
}

// SearchByName: scheme-0 users keep the raw SQL LIKE/ILIKE path untouched.
// For scheme 1, SQL LIKE cannot match tokens, so search scans the user's
// rows (ListAllByUser — documented per-user bound, ADR-0104 §8), decrypts
// names in Go, and applies a Unicode case-folded contains match
// (strings.ToLower on both sides), unifying the old sqlite-ASCII/pg-ILIKE
// divergence. The limit applies AFTER filtering; results sort by (Name, ID).
// Rows whose ancestor chain is gone (impossible through the files API — the
// FK cascade removes subtrees — but defended here) are skipped, while a token
// that fails authentication fails the search loudly (ErrIntegrity).
func (s *TranslatingStore) SearchByName(ctx context.Context, userID int64, term string, limit int) ([]File, error) {
	term = strings.TrimSpace(term)
	if userID == 0 || term == "" {
		return nil, nil
	}
	c := newTranslateCache()
	scheme, err := s.t.schemeFor(ctx, c, userID)
	if err != nil {
		return nil, err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return s.raw.SearchByName(ctx, userID, term, limit)
	}
	if limit <= 0 || limit > searchByNameMax {
		limit = searchByNameMax
	}
	rows, err := s.raw.ListAllByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	fold := strings.ToLower(term)
	var out []File
	for i := range rows {
		f := &rows[i]
		if f.ParentID == nil {
			continue // the root is not a search hit (matches the SQL path)
		}
		if err := s.t.decryptRow(ctx, c, f); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // orphan defense: skip, never fail the whole search
			}
			return nil, err
		}
		if strings.Contains(strings.ToLower(f.Name), fold) {
			out = append(out, *f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// CheckNameBudget exposes the translation core's plaintext name budget check
// so the DAV boundary can reject over-long names before the storage backend
// sees them (whose own limits would surface as 500s). The raw SQLStore has
// no budget: the never-enable carve-out stays bit-identical.
func (s *TranslatingStore) CheckNameBudget(ctx context.Context, userID int64, p string) error {
	return s.t.checkNameBudget(ctx, userID, p)
}

// CipherPath exposes the translation core's plaintext→ciphertext path mapping
// for callers whose store rows live outside files (the shares table,
// ADR-0104 phase 3a): KeySharer wrap-on-write and the share rename/delete
// rewrites match ciphertext file_path strings, while the DAV boundary speaks
// plaintext. Scheme-0 users pass through as normalized plaintext.
func (s *TranslatingStore) CipherPath(ctx context.Context, userID int64, p string) (string, error) {
	return s.t.cipherPath(ctx, newTranslateCache(), userID, p)
}

var (
	_ Store        = (*TranslatingStore)(nil)
	_ KeyShareMeta = (*TranslatingStore)(nil)
)

// TranslatingLockStore carries ciphertext paths in file_locks (ADR-0104 §6).
// Rows are never decrypted back: DAV lock responses echo the request path, so
// path-keyed reads restore the caller's own plaintext path onto the returned
// row (an echo, not decryption); token-keyed rows keep the ciphertext.
type TranslatingLockStore struct {
	raw LockStore
	t   *NameTranslator
}

// NewTranslatingLockStore wraps raw with filename translation.
func NewTranslatingLockStore(raw LockStore, t *NameTranslator) *TranslatingLockStore {
	return &TranslatingLockStore{raw: raw, t: t}
}

// Raw returns the wrapped store: the ciphertext-mount lock check
// (ADR-0104 phase 3a) queries it with anchor-derived ciphertext paths the
// translator must not re-translate.
func (s *TranslatingLockStore) Raw() LockStore { return s.raw }

func (s *TranslatingLockStore) GetByPath(ctx context.Context, userID int64, filePath string) (*FileLock, error) {
	np, err := NormalizePath(filePath)
	if err != nil {
		return nil, err
	}
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, np)
	if err != nil {
		return nil, err
	}
	l, err := s.raw.GetByPath(ctx, userID, cp)
	if err != nil {
		return nil, err
	}
	l.Path = np // echo the request path (rows carry ciphertext only)
	return l, nil
}

// GetByToken passes through; the row's path stays ciphertext — no consumer
// displays it (UNLOCK and lock checks resolve by path).
func (s *TranslatingLockStore) GetByToken(ctx context.Context, token string) (*FileLock, error) {
	return s.raw.GetByToken(ctx, token)
}

func (s *TranslatingLockStore) Insert(ctx context.Context, l *FileLock) error {
	if l == nil {
		return s.raw.Insert(ctx, l)
	}
	np, err := NormalizePath(l.Path)
	if err != nil {
		return err
	}
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), l.UserID, np)
	if err != nil {
		return err
	}
	l.Path = cp
	err = s.raw.Insert(ctx, l)
	l.Path = np // echo the request path on the caller's struct
	return err
}

func (s *TranslatingLockStore) UpdateTimeout(ctx context.Context, id, timeoutMs int64) error {
	return s.raw.UpdateTimeout(ctx, id, timeoutMs)
}

func (s *TranslatingLockStore) Delete(ctx context.Context, id int64) error {
	return s.raw.Delete(ctx, id)
}

func (s *TranslatingLockStore) DeleteByPath(ctx context.Context, userID int64, filePath string) error {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return err
	}
	return s.raw.DeleteByPath(ctx, userID, cp)
}

// RenamePath translates both endpoints; the raw prefix rewrite is pure string
// work on tokens.
func (s *TranslatingLockStore) RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error {
	c := newTranslateCache()
	ctSrc, err := s.t.cipherPath(ctx, c, userID, srcPath)
	if err != nil {
		return err
	}
	ctDst, err := s.t.cipherPath(ctx, c, userID, dstPath)
	if err != nil {
		return err
	}
	return s.raw.RenamePath(ctx, userID, ctSrc, ctDst)
}

func (s *TranslatingLockStore) DeleteExpired(ctx context.Context, nowMs int64) error {
	return s.raw.DeleteExpired(ctx, nowMs)
}

var _ LockStore = (*TranslatingLockStore)(nil)

// TranslatingTrashStore carries ciphertext in trash_items.original_path/name
// (ADR-0104 §6). Insert tokenizes the plaintext original path the Trash
// service computed; reads decrypt both fields. A trash item whose ancestors
// have since been deleted or re-minted (a child trashed before its parent)
// can no longer resolve mid-path rows: the item degrades to its ciphertext
// fields (the ADR-0104 §9 placeholder philosophy — never silent plaintext, it
// IS the stored ciphertext), so listing stays available and restore to an
// explicit destination still works; a tampered token stays a hard
// ErrIntegrity failure.
type TranslatingTrashStore struct {
	raw TrashStore
	t   *NameTranslator
}

// NewTranslatingTrashStore wraps raw with filename translation.
func NewTranslatingTrashStore(raw TrashStore, t *NameTranslator) *TranslatingTrashStore {
	return &TranslatingTrashStore{raw: raw, t: t}
}

func (s *TranslatingTrashStore) Insert(ctx context.Context, item *TrashItem) error {
	if item == nil {
		return s.raw.Insert(ctx, item)
	}
	plain, err := NormalizePath(item.OriginalPath)
	if err != nil {
		return err
	}
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), item.UserID, plain)
	if err != nil {
		return err
	}
	item.OriginalPath = cp
	item.Name = path.Base(cp) // the last ciphertext segment (string op)
	err = s.raw.Insert(ctx, item)
	item.OriginalPath = plain
	if err == nil {
		item.Name = path.Base(plain)
	}
	return err
}

// decryptItem restores an item's plaintext OriginalPath and Name, degrading
// to ciphertext fields when mid-path ancestors are unresolvable.
func (s *TranslatingTrashStore) decryptItem(ctx context.Context, item *TrashItem) error {
	plain, err := s.t.PlainPath(ctx, item.UserID, item.OriginalPath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // degraded: ciphertext fields stand
		}
		return err
	}
	item.OriginalPath = plain
	item.Name = path.Base(plain)
	return nil
}

func (s *TranslatingTrashStore) GetByLocation(ctx context.Context, userID int64, locationID string) (*TrashItem, error) {
	item, err := s.raw.GetByLocation(ctx, userID, locationID)
	if err != nil {
		return nil, err
	}
	if err := s.decryptItem(ctx, item); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *TranslatingTrashStore) List(ctx context.Context, userID int64) ([]TrashItem, error) {
	items, err := s.raw.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if err := s.decryptItem(ctx, &items[i]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func (s *TranslatingTrashStore) Delete(ctx context.Context, userID int64, locationID string) error {
	return s.raw.Delete(ctx, userID, locationID)
}

func (s *TranslatingTrashStore) DeleteExpired(ctx context.Context, userID int64, before time.Time) (int64, error) {
	return s.raw.DeleteExpired(ctx, userID, before)
}

var _ TrashStore = (*TranslatingTrashStore)(nil)

// TranslatingVersionStore carries ciphertext in file_versions.file_path
// (ADR-0104 §6): path arguments translate on the way in; returned rows keep
// their ciphertext path because consumers use only the numeric version IDs
// (storage keys are versions/<uid>/<id>) — nothing decrypts them back.
type TranslatingVersionStore struct {
	raw VersionStore
	t   *NameTranslator
}

// NewTranslatingVersionStore wraps raw with filename translation.
func NewTranslatingVersionStore(raw VersionStore, t *NameTranslator) *TranslatingVersionStore {
	return &TranslatingVersionStore{raw: raw, t: t}
}

// Raw returns the wrapped store: the ciphertext-mount write path
// (ADR-0104 phase 3a) inserts version rows carrying ciphertext paths the
// translator must not re-translate.
func (s *TranslatingVersionStore) Raw() VersionStore { return s.raw }

func (s *TranslatingVersionStore) Insert(ctx context.Context, v *FileVersion) error {
	if v == nil {
		return s.raw.Insert(ctx, v)
	}
	np, err := NormalizePath(v.Path)
	if err != nil {
		return err
	}
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), v.UserID, np)
	if err != nil {
		return err
	}
	v.Path = cp
	err = s.raw.Insert(ctx, v)
	v.Path = np // the caller keeps the plaintext view
	return err
}

func (s *TranslatingVersionStore) Get(ctx context.Context, userID int64, filePath, revision string) (*FileVersion, error) {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return nil, err
	}
	return s.raw.Get(ctx, userID, cp, revision)
}

func (s *TranslatingVersionStore) ListByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error) {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return nil, err
	}
	return s.raw.ListByPath(ctx, userID, cp)
}

func (s *TranslatingVersionStore) Delete(ctx context.Context, userID int64, filePath, revision string) error {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return err
	}
	return s.raw.Delete(ctx, userID, cp, revision)
}

func (s *TranslatingVersionStore) DeleteByPath(ctx context.Context, userID int64, filePath string) ([]FileVersion, error) {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return nil, err
	}
	return s.raw.DeleteByPath(ctx, userID, cp)
}

func (s *TranslatingVersionStore) DeleteExpired(ctx context.Context, userID int64, filePath string, before time.Time) ([]FileVersion, error) {
	cp, err := s.t.cipherPath(ctx, newTranslateCache(), userID, filePath)
	if err != nil {
		return nil, err
	}
	return s.raw.DeleteExpired(ctx, userID, cp, before)
}

// RenamePath translates both endpoints; the raw prefix rewrite is pure string
// work on tokens.
func (s *TranslatingVersionStore) RenamePath(ctx context.Context, userID int64, srcPath, dstPath string) error {
	c := newTranslateCache()
	ctSrc, err := s.t.cipherPath(ctx, c, userID, srcPath)
	if err != nil {
		return err
	}
	ctDst, err := s.t.cipherPath(ctx, c, userID, dstPath)
	if err != nil {
		return err
	}
	return s.raw.RenamePath(ctx, userID, ctSrc, ctDst)
}

var _ VersionStore = (*TranslatingVersionStore)(nil)
