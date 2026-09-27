package files

import (
	"context"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// ADR-0104 §3 length budget, enforced on every dialect for uniformity:
// plaintext names are capped at 255 runes (MySQL VARCHAR(255) parity) and the
// computed ciphertext path at 768 chars (the MySQL index ceiling).
const (
	maxNameRunes       = 255
	maxCipherPathChars = 768
	keyUUIDSize        = 16 // mirrors encrypt's unexported keyUUIDSize
)

// NameKeyResolver resolves a directory key (DK) by its key UUID — the narrow
// seam the translator needs. *encrypt.SQLResolver satisfies it; the interface
// keeps the translation core free of a concrete encrypt dependency beyond the
// NCGOFN1 primitives (mirroring KeyWrapper/DirKeyMinter).
type NameKeyResolver interface {
	Resolve(ctx context.Context, keyUUID [16]byte) ([]byte, error)
}

// UserNameSchemer reads users.name_scheme, the authoritative per-user write
// switch (ADR-0104 §3). *users.SQLStore satisfies it; files must not import
// users, so the seam is declared here structurally.
type UserNameSchemer interface {
	UserNameScheme(ctx context.Context, userID int64) (int, error)
}

// NameTranslator is the ADR-0104 phase-2 translation core shared by the
// store decorators: it maps plaintext DAV paths to the
// ciphertext-materialized paths the rows carry (cipherPath) and decrypts rows
// back to plaintext on the way out (decryptRow). The root (path "/", name "")
// is never encrypted.
//
// Caching is strictly per top-level call: every TranslatingStore method (and
// every satellite wrapper method) mints a fresh translateCache, and a cache
// never outlives the call that created it. A shared or global NK/DK cache
// would be an authorization bypass — Resolve enforces per-ctx authorization
// (an enrolled reader resolves through their own wrap rows, ADR-0100/0101),
// so key material resolved under one request's identity must never serve
// another request.
type NameTranslator struct {
	raw   *SQLStore
	keys  NameKeyResolver
	users UserNameSchemer
}

// NewNameTranslator returns the translation core over the raw filecache
// store, the key resolver, and the users name_scheme seam.
func NewNameTranslator(raw *SQLStore, keys NameKeyResolver, users UserNameSchemer) *NameTranslator {
	return &NameTranslator{raw: raw, keys: keys, users: users}
}

// translateCache is the per-call cache set: the user's name_scheme (looked up
// once), directory keys and derived name keys by key UUID, raw rows by id,
// and decrypted paths by row id. Created fresh by every top-level store call
// (see NameTranslator's comment for the authorization rationale).
type translateCache struct {
	scheme    *int
	dk        map[[keyUUIDSize]byte][]byte
	nk        map[[keyUUIDSize]byte][]byte
	rowsByID  map[int64]*File
	plainPath map[int64]string
}

func newTranslateCache() *translateCache {
	return &translateCache{
		dk:        map[[keyUUIDSize]byte][]byte{},
		nk:        map[[keyUUIDSize]byte][]byte{},
		rowsByID:  map[int64]*File{},
		plainPath: map[int64]string{},
	}
}

// schemeFor returns the user's users.name_scheme, cached for the call. Any
// value other than encrypt.NameSchemeNCGOFN1 is plaintext (0 today).
func (t *NameTranslator) schemeFor(ctx context.Context, c *translateCache, userID int64) (int, error) {
	if c.scheme != nil {
		return *c.scheme, nil
	}
	scheme, err := t.users.UserNameScheme(ctx, userID)
	if err != nil {
		return 0, err
	}
	c.scheme = &scheme
	return scheme, nil
}

// nkFor resolves the directory key named by keyUUID and derives its name key,
// both cached by key UUID for the call. The 16-byte key UUID comes from a
// folder row's key_uuid.
func (t *NameTranslator) nkFor(ctx context.Context, c *translateCache, keyUUID []byte) ([]byte, [keyUUIDSize]byte, error) {
	var id [keyUUIDSize]byte
	if len(keyUUID) != keyUUIDSize {
		return nil, id, fmt.Errorf("files: folder key uuid is %d bytes, want %d (directory key missing)", len(keyUUID), keyUUIDSize)
	}
	copy(id[:], keyUUID)
	if nk, ok := c.nk[id]; ok {
		return nk, id, nil
	}
	dk, ok := c.dk[id]
	if !ok {
		resolved, err := t.keys.Resolve(ctx, id)
		if err != nil {
			return nil, id, err
		}
		dk = resolved
		c.dk[id] = dk
	}
	nk, err := encrypt.DeriveNameKey(dk, id)
	if err != nil {
		return nil, id, err
	}
	c.nk[id] = nk
	return nk, id, nil
}

// cipherPath maps a plaintext path to its ciphertext-materialized form: the
// per-segment NCGOFN1 token under each parent folder's name key, joined by
// "/". A scheme-0 user short-circuits to the normalized plaintext (fast
// path). Intermediate segments resolve their folder rows by the ciphertext
// built so far — a missing intermediate is ErrNotFound, a non-directory one
// ErrNotDir. The final segment's token needs only its parent's key, so the
// leaf may not exist yet (create, lock-null). Over-budget names (>255 runes)
// or ciphertext paths (>768 chars) fail with ErrNameBudget.
func (t *NameTranslator) cipherPath(ctx context.Context, c *translateCache, userID int64, plainPath string) (string, error) {
	np, err := NormalizePath(plainPath)
	if err != nil {
		return "", err
	}
	if np == "/" {
		return "/", nil // the root is never encrypted
	}
	scheme, err := t.schemeFor(ctx, c, userID)
	if err != nil {
		return "", err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return np, nil
	}
	root, err := t.raw.GetByPath(ctx, userID, "/")
	if err != nil {
		return "", err
	}
	segs := strings.Split(strings.TrimPrefix(np, "/"), "/")
	parent := root
	ct := ""
	for i, seg := range segs {
		tok, err := t.cipherSegment(ctx, c, parent, seg)
		if err != nil {
			return "", err
		}
		ct += "/" + tok
		if len(ct) > maxCipherPathChars {
			return "", fmt.Errorf("files: ciphertext path of %q exceeds %d chars: %w", np, maxCipherPathChars, ErrNameBudget)
		}
		if i < len(segs)-1 {
			row, err := t.raw.GetByPath(ctx, userID, ct)
			if err != nil {
				return "", err
			}
			if !row.IsDir {
				return "", ErrNotDir
			}
			parent = row
		}
	}
	return ct, nil
}

// cipherSegment tokens one plaintext name under the parent folder row,
// enforcing the name rune budget.
func (t *NameTranslator) cipherSegment(ctx context.Context, c *translateCache, parent *File, name string) (string, error) {
	if utf8.RuneCountInString(name) > maxNameRunes {
		return "", fmt.Errorf("files: name exceeds %d runes: %w", maxNameRunes, ErrNameBudget)
	}
	nk, parentUUID, err := t.nkFor(ctx, c, parent.KeyUUID)
	if err != nil {
		return "", err
	}
	return encrypt.EncryptName(nk, parentUUID, name)
}

// rowByID fetches a raw row by id, cached for the call.
func (t *NameTranslator) rowByID(ctx context.Context, c *translateCache, id int64) (*File, error) {
	if row, ok := c.rowsByID[id]; ok {
		return row, nil
	}
	row, err := t.raw.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	c.rowsByID[id] = row
	return row, nil
}

// plainPathOf rebuilds the plaintext path of the row id by walking ancestors
// to the root, decrypting each scheme-1 basename token under its parent
// folder's name key (rows and paths cached for the call). Scheme-0 rows pass
// through verbatim (dual-read). A tampered token surfaces ErrIntegrity —
// never a silent plaintext fallback. A missing ancestor row surfaces
// ErrNotFound: files rows guarantee the chain by construction, but trash-side
// callers walk paths whose ancestors may already be deleted.
func (t *NameTranslator) plainPathOf(ctx context.Context, c *translateCache, id int64) (string, error) {
	if p, ok := c.plainPath[id]; ok {
		return p, nil
	}
	row, err := t.rowByID(ctx, c, id)
	if err != nil {
		return "", err
	}
	out := "/"
	if row.ParentID != nil {
		pp, err := t.plainPathOf(ctx, c, *row.ParentID)
		if err != nil {
			return "", err
		}
		name := row.Name
		if row.NameScheme == encrypt.NameSchemeNCGOFN1 {
			parent, err := t.rowByID(ctx, c, *row.ParentID)
			if err != nil {
				return "", err
			}
			name, err = t.decryptSegment(ctx, c, parent, row.Name)
			if err != nil {
				return "", err
			}
		}
		out = joinNamePath(pp, name)
	}
	c.plainPath[id] = out
	return out, nil
}

// decryptSegment reverses cipherSegment for a token under the parent row.
func (t *NameTranslator) decryptSegment(ctx context.Context, c *translateCache, parent *File, token string) (string, error) {
	nk, parentUUID, err := t.nkFor(ctx, c, parent.KeyUUID)
	if err != nil {
		return "", err
	}
	return encrypt.DecryptName(nk, parentUUID, token)
}

// decryptRow rewrites a row's Name and Path from ciphertext to plaintext in
// place. Rows not marked NCGOFN1 pass through unchanged (dual-read of mixed
// trees), as does the root. The plaintext path is rebuilt by walking the
// row's ancestors (per-call caches make this O(distinct parents) fetches).
func (t *NameTranslator) decryptRow(ctx context.Context, c *translateCache, f *File) error {
	if f == nil || f.NameScheme != encrypt.NameSchemeNCGOFN1 {
		return nil
	}
	if f.ParentID == nil {
		// The root is never encrypted; a scheme-1 root marker is an
		// inconsistent row — normalize to the literal root rather than
		// attempting to decrypt the empty name.
		f.Name = ""
		f.Path = "/"
		return nil
	}
	pp, err := t.plainPathOf(ctx, c, *f.ParentID)
	if err != nil {
		return err
	}
	parent, err := t.rowByID(ctx, c, *f.ParentID)
	if err != nil {
		return err
	}
	name, err := t.decryptSegment(ctx, c, parent, f.Name)
	if err != nil {
		return err
	}
	f.Name = name
	f.Path = joinNamePath(pp, name)
	return nil
}

// decryptRows decrypts a row set in place; every error propagates (a row set
// read under a live parent cannot hit the missing-ancestor corner).
func (t *NameTranslator) decryptRows(ctx context.Context, c *translateCache, rows []File) error {
	for i := range rows {
		if err := t.decryptRow(ctx, c, &rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// PlainPath maps a ciphertext path back to plaintext by decrypting each
// segment token under the parent folder row found at the ciphertext prefix —
// the read-direction mirror of cipherPath, for rows outside files (trash)
// whose own row is gone but whose ancestors usually still exist. A scheme-0
// user short-circuits to the normalized input. A missing intermediate row is
// ErrNotFound; a token that fails authentication is ErrIntegrity, never a
// silent fallback.
func (t *NameTranslator) PlainPath(ctx context.Context, userID int64, cipherPath string) (string, error) {
	c := newTranslateCache()
	np, err := NormalizePath(cipherPath)
	if err != nil {
		return "", err
	}
	if np == "/" {
		return "/", nil
	}
	scheme, err := t.schemeFor(ctx, c, userID)
	if err != nil {
		return "", err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return np, nil
	}
	root, err := t.raw.GetByPath(ctx, userID, "/")
	if err != nil {
		return "", err
	}
	segs := strings.Split(strings.TrimPrefix(np, "/"), "/")
	parent := root
	plain := ""
	ct := ""
	for i, tok := range segs {
		seg, err := t.decryptSegment(ctx, c, parent, tok)
		if err != nil {
			return "", err
		}
		plain += "/" + seg
		ct += "/" + tok
		if i < len(segs)-1 {
			row, err := t.raw.GetByPath(ctx, userID, ct)
			if err != nil {
				return "", err
			}
			if !row.IsDir {
				return "", ErrNotDir
			}
			parent = row
		}
	}
	return plain, nil
}

// checkNameBudget enforces the ADR-0104 §3 plaintext name budget (255 runes
// per segment) for scheme-1 users, for paths that have not been written yet
// (mkdir, move destination). It intentionally does NOT walk rows: the leaf
// need not exist, and missing intermediates are the caller's own error
// semantics (ErrParentMissing), not this check's. The ciphertext path budget
// lives in cipherPath at insert/rename time. Scheme-0 users keep the
// pre-mode behavior exactly (no DAV-visible budget).
func (t *NameTranslator) checkNameBudget(ctx context.Context, userID int64, p string) error {
	np, err := NormalizePath(p)
	if err != nil {
		return err
	}
	if np == "/" {
		return nil
	}
	scheme, err := t.schemeFor(ctx, newTranslateCache(), userID)
	if err != nil {
		return err
	}
	if scheme != encrypt.NameSchemeNCGOFN1 {
		return nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(np, "/"), "/") {
		if utf8.RuneCountInString(seg) > maxNameRunes {
			return fmt.Errorf("files: name exceeds %d runes: %w", maxNameRunes, ErrNameBudget)
		}
	}
	return nil
}

// joinNamePath joins a plaintext parent path and a basename.
func joinNamePath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// joinTokenPath joins a ciphertext parent path and a name token, enforcing
// the ciphertext path budget on the result.
func joinTokenPath(parent, token string) (string, error) {
	ct := joinNamePath(parent, token)
	if len(ct) > maxCipherPathChars {
		return "", fmt.Errorf("files: ciphertext path exceeds %d chars: %w", maxCipherPathChars, ErrNameBudget)
	}
	return ct, nil
}

// dirPath returns the plaintext parent path of a normalized non-root path.
func dirPath(np string) string {
	parent := path.Dir(np)
	if parent == "." {
		return "/"
	}
	return parent
}
