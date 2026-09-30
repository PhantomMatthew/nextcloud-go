package files

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

// ADR-0104 phase 4: the per-user name-encryption sweeps. EncryptUser converts
// an existing plaintext tree (users.name_scheme 0 → 1) — minting directory
// keys for every folder, rewriting files/file_locks/file_versions/
// file_properties/trash_items/shares rows to NCGOFN1 ciphertext — and
// DecryptUser reverses it
// (decommissioning/rollback). The conversion is ONE database transaction per
// user: a mid-sweep mixed tree is unreadable because both the write switch
// and the read path key off users.name_scheme, so the tree rewrite and the
// switch flip commit atomically (the users row goes last, guarded on the old
// scheme, so a sweep racing another conversion loses loudly and retries).
//
// Trash storage objects (trash/<uid>/<location_id>) embed the location id, so
// they move to the token-based id BEFORE the transaction; a transaction
// failure compensates the moves back best-effort. Satellite rows
// (trash/versions/locks) whose paths no longer resolve against the live tree
// — ancestors deleted after the row was written — are SKIPPED and counted
// (documented residual: retention/expiry self-heals them); skipped trash rows
// keep their plaintext location_id and their objects are not moved.
//
// Enrolled users (ADR-0100 password-wrapped keys) can never be converted by
// an offline run: their key boxes open only with an unlocked session. The
// sweep aborts loudly for them in a principal-less ctx; the web login hook
// runs EncryptUser holding the unlocked key (internal/web NameSweepRunner).
//
// In-flight chunked-upload sessions (uploads.destination) are deliberately
// NOT converted: a session spanning a sweep fails loudly at finalize and the
// client retries cleanly (ADR-0104 §11 — run sweeps quiesced if that matters).

// NameSweepKeys is the sweep's narrow key seam; *encrypt.SQLResolver
// satisfies it. AllocateForUser mints a directory key (an enrolled owner gets
// an X25519 box wrap without any session; the plaintext DK rides back for the
// token pass). Resolve unwraps an existing DK. WrapKeyForFK wraps a
// caller-held DK for a share recipient without a Resolve round-trip (the
// sweep holds every DK in memory). Enrolled reports the ADR-0100 enrollment
// state.
type NameSweepKeys interface {
	AllocateForUser(ctx context.Context, uid string) (keyUUID [16]byte, dk []byte, err error)
	Resolve(ctx context.Context, keyUUID [16]byte) ([]byte, error)
	WrapKeyForFK(ctx context.Context, keyUUID [16]byte, uid string, fk []byte) error
	Enrolled(ctx context.Context, uid string) (bool, error)
}

// NameSweepUsers is the sweep's users-table seam; *users.SQLStore satisfies
// it. GroupMembers expands group shares to their current members for the
// directory-key recipient wraps.
type NameSweepUsers interface {
	GetByUID(ctx context.Context, uid string) (*users.User, error)
	UserNameScheme(ctx context.Context, userID int64) (int, error)
	GroupMembers(ctx context.Context, gid string, limit int) ([]string, error)
}

// NameSweepStats reports one user's conversion — rows rewritten per table,
// folders freshly keyed, trash objects moved, recipient wraps written, and
// satellite rows skipped because their paths no longer resolve. With DryRun
// the counts are what a real run would do (nothing is written or moved).
// Skipped reports the no-op: the user was already in the target scheme.
type NameSweepStats struct {
	Skipped      bool
	FilesRows    int // files rows rewritten (root included)
	FoldersKeyed int // folders that gained a fresh directory key (encrypt only)
	LocksRows    int // file_locks rows rewritten
	VersionsRows int // file_versions rows rewritten
	PropsRows    int // file_properties rows rewritten
	TrashRows    int // trash_items rows rewritten
	TrashMoved   int // trash storage objects renamed
	SharesRows   int // shares rows rewritten
	ShareWraps   int // recipient wraps written for directory keys (encrypt only)
	SkippedRows  int // satellite rows skipped (unresolvable paths — documented residual)
}

// NameSweep converts one user's tree between plaintext and NCGOFN1 names
// (ADR-0104 phase 4). DB is the server database (the transaction scope);
// Store is the RAW filecache store (no translating wrapper — the sweep speaks
// ciphertext to the DB directly); Storage is the default backend (trash
// object renames are path-level pass-throughs under the encryption wrapper).
// Logger is nil-ok; DryRun runs the read/crypto passes and reports, writing
// nothing and moving no objects.
type NameSweep struct {
	DB      database.DB
	Store   *SQLStore
	Keys    NameSweepKeys
	Users   NameSweepUsers
	Storage storage.Storage
	Logger  *slog.Logger // nil-ok
	DryRun  bool
}

func (s *NameSweep) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// EncryptUser converts uid's plaintext tree to NCGOFN1 ciphertext (scheme
// 0 → 1). A scheme-1 user is a skip (idempotent re-run). An enrolled user in
// a principal-less ctx (no unlocked key) aborts loudly wrapping
// encrypt.ErrKeyLocked — enrolled users convert online at password login.
func (s *NameSweep) EncryptUser(ctx context.Context, uid string) (NameSweepStats, error) {
	return s.run(ctx, uid, true)
}

// DecryptUser reverses EncryptUser (scheme 1 → 0): plaintext names/paths
// everywhere, shares' sealed metadata cleared, folder key_uuids NULLed and
// their file_keys wrap rows deleted (files keep their FKs — content stays
// sealed). A scheme-0 user is a skip. An enrolled user aborts loudly: their
// directory keys are boxes no offline run opens — unenroll first.
func (s *NameSweep) DecryptUser(ctx context.Context, uid string) (NameSweepStats, error) {
	return s.run(ctx, uid, false)
}

// run is the shared sweep body; encrypting selects the direction.
func (s *NameSweep) run(ctx context.Context, uid string, encrypting bool) (NameSweepStats, error) {
	verb := "decrypt-names"
	if encrypting {
		verb = "encrypt-names"
	}
	var stats NameSweepStats
	u, err := s.Users.GetByUID(ctx, uid)
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	scheme, err := s.Users.UserNameScheme(ctx, u.ID)
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	target := 0
	if encrypting {
		target = encrypt.NameSchemeNCGOFN1
	}
	if scheme == target {
		stats.Skipped = true
		return stats, nil
	}
	if scheme != 0 && scheme != encrypt.NameSchemeNCGOFN1 {
		return stats, fmt.Errorf("files: %s: user %q has unknown name_scheme %d", verb, uid, scheme)
	}
	// The enrolled gate: an enrolled user's wraps are X25519 boxes only an
	// unlocked session opens (ADR-0100/0101), so an offline run can neither
	// resolve pre-existing DKs nor seal file-share metadata for them. The
	// login hook passes a ctx carrying the unlocked key.
	enrolled, err := s.Keys.Enrolled(ctx, uid)
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q enrollment check: %w", verb, uid, err)
	}
	if enrolled {
		if p, ok := auth.UserFromContext(ctx); !ok || p == nil || len(p.UnlockedKey) == 0 {
			if encrypting {
				return stats, fmt.Errorf("files: %s: user %q is enrolled in password-wrapped keys (ADR-0100) and this run holds no unlocked session key: enrolled users convert online at password login (the login hook runs this sweep holding the unlocked key): %w",
					verb, uid, encrypt.ErrKeyLocked)
			}
			return stats, fmt.Errorf("files: %s: user %q is enrolled in password-wrapped keys (ADR-0100): unenroll first (the user logs in once with encryption.password_wrapped_keys off), then re-run %s: %w",
				verb, uid, verb, encrypt.ErrKeyLocked)
		}
	}
	tree, err := s.loadTree(ctx, u.ID)
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	if encrypting {
		err = s.encryptKeys(ctx, uid, tree, &stats)
	} else {
		err = s.decryptKeys(ctx, uid, tree)
	}
	if err != nil {
		return stats, err
	}
	if encrypting {
		err = tree.encryptPaths()
	} else {
		err = tree.decryptPaths()
	}
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	plan, err := s.planSatellites(ctx, u, tree, encrypting)
	if err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	stats.FilesRows = len(tree.rows)
	stats.LocksRows = len(plan.locks)
	stats.VersionsRows = len(plan.versions)
	stats.PropsRows = len(plan.props)
	stats.TrashRows = len(plan.trash)
	stats.SharesRows = len(plan.shares)
	stats.SkippedRows = plan.skipped
	for _, tm := range plan.trash {
		if tm.oldLoc != tm.newLoc {
			stats.TrashMoved++
		}
	}
	stats.ShareWraps = len(plan.wraps)
	if s.DryRun {
		return stats, nil
	}
	// Directory keys born in the sweep must reach the recipients of the
	// owner's user/group shares, or sharees of an encrypted tree cannot
	// resolve in-subtree names (file FK wraps predate the sweep — grant hooks
	// and reconcile own them). No-op-on-duplicate inserts, before the tx.
	if err := s.writeShareWraps(ctx, plan.wraps); err != nil {
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	moved, err := s.moveTrashObjects(ctx, u.UID, plan.trash)
	if err != nil {
		s.compensateTrashMoves(ctx, moved)
		return stats, fmt.Errorf("files: %s: user %q: trash storage moves: %w", verb, uid, err)
	}
	if err := s.commitPlan(ctx, u.ID, tree, plan, encrypting); err != nil {
		s.compensateTrashMoves(ctx, moved)
		return stats, fmt.Errorf("files: %s: user %q: %w", verb, uid, err)
	}
	return stats, nil
}

// sweepTree is the in-memory mirror of one user's files rows the sweep plans
// against (the token walk is pure memory, never the store): the row set, the
// parent/child index, per-folder directory keys, and the converted name/path
// per row id. Rows keep their STORED names (plaintext for encrypt-names,
// tokens for decrypt-names).
type sweepTree struct {
	rows    []*File
	root    *File
	kids    map[int64][]*File
	byName  map[int64]map[string]*File // parent id → stored basename → row
	dk      map[int64][]byte           // folder id → resolved/minted DK
	uuid    map[int64][16]byte         // folder id → its key uuid
	minted  map[int64]bool             // folder id → the sweep minted this DK
	nk      map[int64][]byte           // folder id → derived name key (lazy)
	newName map[int64]string           // row id → converted basename
	newPath map[int64]string           // row id → converted full path
}

// loadTree reads the user's whole filecache (root included) and indexes it.
// A row whose parent id no row carries is corruption — the FK cascade
// guarantees the chain, so this never silently skips.
func (s *NameSweep) loadTree(ctx context.Context, userID int64) (*sweepTree, error) {
	rows, err := s.Store.ListAllByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	t := &sweepTree{
		kids:    map[int64][]*File{},
		byName:  map[int64]map[string]*File{},
		dk:      map[int64][]byte{},
		uuid:    map[int64][16]byte{},
		minted:  map[int64]bool{},
		nk:      map[int64][]byte{},
		newName: map[int64]string{},
		newPath: map[int64]string{},
	}
	byID := map[int64]*File{}
	for i := range rows {
		f := &rows[i]
		byID[f.ID] = f
		if f.ParentID == nil {
			if t.root != nil {
				return nil, fmt.Errorf("tree has two roots (ids %d and %d)", t.root.ID, f.ID)
			}
			t.root = f
		}
		t.rows = append(t.rows, f)
	}
	for _, f := range t.rows {
		if f.ParentID == nil {
			continue
		}
		parent, ok := byID[*f.ParentID]
		if !ok {
			return nil, fmt.Errorf("row %d (%q) has missing parent id %d", f.ID, f.Path, *f.ParentID)
		}
		t.kids[parent.ID] = append(t.kids[parent.ID], f)
		if t.byName[parent.ID] == nil {
			t.byName[parent.ID] = map[string]*File{}
		}
		t.byName[parent.ID][f.Name] = f
	}
	if t.root == nil && len(t.rows) > 0 {
		return nil, fmt.Errorf("tree has %d rows but no root row", len(t.rows))
	}
	return t, nil
}

// nkOf returns (and caches) the name key of a folder — HKDF over its DK
// (ADR-0104 §2). A folder without a key is the sweep-miss alarm state; its
// children's tokens are unresolvable, so the error is loud.
func (t *sweepTree) nkOf(folderID int64) ([]byte, [16]byte, error) {
	if nk, ok := t.nk[folderID]; ok {
		return nk, t.uuid[folderID], nil
	}
	var uuid [16]byte
	dk, ok := t.dk[folderID]
	if !ok {
		return nil, uuid, fmt.Errorf("folder row id %d has no directory key (name resolution is broken for its children)", folderID)
	}
	uuid = t.uuid[folderID]
	nk, err := encrypt.DeriveNameKey(dk, uuid)
	if err != nil {
		return nil, uuid, err
	}
	t.nk[folderID] = nk
	return nk, uuid, nil
}

// encryptKeys runs the encrypt-direction key pass: Resolve every existing
// folder DK FIRST (an enrolled user's boxes abort before any mint write
// lands), then mint DKs for the folders lacking one — root included.
// AllocateForUser needs no session even for enrolled owners (a box wrap needs
// only the public key) and returns the plaintext DK for the token pass. A
// dry run mints in memory only — nothing is persisted.
func (s *NameSweep) encryptKeys(ctx context.Context, uid string, t *sweepTree, stats *NameSweepStats) error {
	for _, f := range t.rows {
		if !f.IsDir || len(f.KeyUUID) == 0 {
			continue
		}
		if len(f.KeyUUID) != keyUUIDSize {
			return fmt.Errorf("files: encrypt-names: user %q: folder %q key uuid is %d bytes, want %d", uid, f.Path, len(f.KeyUUID), keyUUIDSize)
		}
		var uuid [keyUUIDSize]byte
		copy(uuid[:], f.KeyUUID)
		dk, err := s.Keys.Resolve(ctx, uuid)
		if err != nil {
			if errors.Is(err, encrypt.ErrKeyLocked) {
				return fmt.Errorf("files: encrypt-names: user %q: resolving directory keys needs an unlocked session — enrolled users convert online at password login: %w", uid, err)
			}
			return fmt.Errorf("files: encrypt-names: user %q: resolve directory key of %q: %w", uid, f.Path, err)
		}
		t.dk[f.ID] = dk
		t.uuid[f.ID] = uuid
	}
	for _, f := range t.rows {
		if !f.IsDir {
			continue
		}
		if _, ok := t.dk[f.ID]; ok {
			continue
		}
		if s.DryRun {
			// Report-only minting: the tokens a dry run computes are
			// throwaway, and no wrap row may be written.
			uuid, dk, err := mintLocalKey()
			if err != nil {
				return err
			}
			t.dk[f.ID] = dk
			t.uuid[f.ID] = uuid
		} else {
			uuid, dk, err := s.Keys.AllocateForUser(ctx, uid)
			if err != nil {
				return fmt.Errorf("files: encrypt-names: user %q: mint directory key for %q: %w", uid, f.Path, err)
			}
			t.dk[f.ID] = dk
			t.uuid[f.ID] = uuid
		}
		t.minted[f.ID] = true
		stats.FoldersKeyed++
	}
	return nil
}

// decryptKeys runs the decrypt-direction key pass: Resolve every folder DK
// (the token pass needs them). A folder missing its DK under a scheme-1 user
// is legit only before the lazy root claim or when childless — the miss turns
// loud in nkOf the moment a child's token needs it. A dry run resolves too
// (read-only) so its report is accurate.
func (s *NameSweep) decryptKeys(ctx context.Context, uid string, t *sweepTree) error {
	for _, f := range t.rows {
		if !f.IsDir || len(f.KeyUUID) == 0 {
			continue
		}
		if len(f.KeyUUID) != keyUUIDSize {
			return fmt.Errorf("files: decrypt-names: user %q: folder row id %d key uuid is %d bytes, want %d", uid, f.ID, len(f.KeyUUID), keyUUIDSize)
		}
		var uuid [keyUUIDSize]byte
		copy(uuid[:], f.KeyUUID)
		dk, err := s.Keys.Resolve(ctx, uuid)
		if err != nil {
			if errors.Is(err, encrypt.ErrKeyLocked) {
				return fmt.Errorf("files: decrypt-names: user %q: resolving directory keys needs an unlocked session — unenroll the user first: %w", uid, err)
			}
			return fmt.Errorf("files: decrypt-names: user %q: resolve directory key of row id %d: %w", uid, f.ID, err)
		}
		t.dk[f.ID] = dk
		t.uuid[f.ID] = uuid
	}
	return nil
}

// mintLocalKey generates throwaway key material for dry-run planning (no
// resolver call, no persisted wrap row).
func mintLocalKey() (keyUUID [16]byte, dk []byte, err error) {
	dk = make([]byte, 32)
	if _, err := rand.Read(dk); err != nil {
		return keyUUID, nil, fmt.Errorf("files: name sweep dry-run key: %w", err)
	}
	if _, err := rand.Read(keyUUID[:]); err != nil {
		return keyUUID, nil, fmt.Errorf("files: name sweep dry-run key uuid: %w", err)
	}
	return keyUUID, dk, nil
}

// encryptPaths computes every row's NCGOFN1 token and ciphertext path in one
// BFS from the root (pure memory — the write switch is still off, so the
// store sees nothing until the tx). The root stays "/"/"". A name over 255
// runes or a ciphertext path over 768 chars aborts the user naming the
// offending PLAINTEXT path; nothing has been written at that point.
func (t *sweepTree) encryptPaths() error {
	if t.root == nil {
		return nil
	}
	t.newName[t.root.ID] = ""
	t.newPath[t.root.ID] = "/"
	queue := []*File{t.root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		nk, uuid, err := t.nkOf(parent.ID)
		if err != nil {
			return fmt.Errorf("%q: %w", parent.Path, err)
		}
		for _, child := range t.kids[parent.ID] {
			if utf8.RuneCountInString(child.Name) > maxNameRunes {
				return fmt.Errorf("name of %q exceeds %d runes: %w", child.Path, maxNameRunes, ErrNameBudget)
			}
			tok, err := encrypt.EncryptName(nk, uuid, child.Name)
			if err != nil {
				return fmt.Errorf("%q: %w", child.Path, err)
			}
			ct, err := joinTokenPath(t.newPath[parent.ID], tok)
			if err != nil {
				return fmt.Errorf("%q: %w", child.Path, err)
			}
			t.newName[child.ID] = tok
			t.newPath[child.ID] = ct
			if child.IsDir {
				queue = append(queue, child)
			}
		}
	}
	return nil
}

// decryptPaths mirrors encryptPaths: each scheme-1 row's basename token
// decrypts under its parent's name key; scheme-0 rows in a mixed tree pass
// through verbatim (dual-read defense). A tampered token surfaces
// ErrIntegrity, never a plaintext fallback.
func (t *sweepTree) decryptPaths() error {
	if t.root == nil {
		return nil
	}
	t.newName[t.root.ID] = ""
	t.newPath[t.root.ID] = "/"
	queue := []*File{t.root}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range t.kids[parent.ID] {
			name := child.Name
			if child.NameScheme == encrypt.NameSchemeNCGOFN1 {
				nk, uuid, err := t.nkOf(parent.ID)
				if err != nil {
					return fmt.Errorf("%q: %w", parent.Path, err)
				}
				name, err = encrypt.DecryptName(nk, uuid, child.Name)
				if err != nil {
					return fmt.Errorf("decrypt name of row id %d (path %q): %w", child.ID, child.Path, err)
				}
			}
			t.newName[child.ID] = name
			t.newPath[child.ID] = joinNamePath(t.newPath[parent.ID], name)
			if child.IsDir {
				queue = append(queue, child)
			}
		}
	}
	return nil
}

// cipherFor maps a live-tree plaintext path to its converted ciphertext form
// for satellite rows (locks/versions/trash) whose own row is gone from files:
// every ancestor segment must name an existing directory; the leaf token
// needs only its parent's name key. Unresolvable ancestors return
// ErrNotFound/ErrNotDir (the caller skips and counts the row).
func (t *sweepTree) cipherFor(plain string) (string, error) {
	np, err := NormalizePath(plain)
	if err != nil {
		return "", err
	}
	if np == "/" {
		return "/", nil
	}
	if t.root == nil {
		return "", ErrNotFound
	}
	parent := t.root
	ct := ""
	segs := strings.Split(strings.TrimPrefix(np, "/"), "/")
	for i, seg := range segs {
		nk, uuid, err := t.nkOf(parent.ID)
		if err != nil {
			return "", fmt.Errorf("%q: %w", np, err)
		}
		tok, err := encrypt.EncryptName(nk, uuid, seg)
		if err != nil {
			return "", err
		}
		ct += "/" + tok
		if len(ct) > maxCipherPathChars {
			return "", fmt.Errorf("ciphertext path of %q exceeds %d chars: %w", np, maxCipherPathChars, ErrNameBudget)
		}
		if i < len(segs)-1 {
			next, ok := t.byName[parent.ID][seg]
			if !ok {
				return "", ErrNotFound
			}
			if !next.IsDir {
				return "", ErrNotDir
			}
			parent = next
		}
	}
	return ct, nil
}

// plainFor mirrors cipherFor for the decrypt sweep: satellite ciphertext
// paths resolve segment-wise against the stored tokens; the leaf token
// decrypts under its parent's name key whether or not its row still exists.
func (t *sweepTree) plainFor(cipher string) (string, error) {
	np, err := NormalizePath(cipher)
	if err != nil {
		return "", err
	}
	if np == "/" {
		return "/", nil
	}
	if t.root == nil {
		return "", ErrNotFound
	}
	parent := t.root
	plain := ""
	segs := strings.Split(strings.TrimPrefix(np, "/"), "/")
	for i, tok := range segs {
		nk, uuid, err := t.nkOf(parent.ID)
		if err != nil {
			return "", fmt.Errorf("%q: %w", np, err)
		}
		seg, err := encrypt.DecryptName(nk, uuid, tok)
		if err != nil {
			return "", err
		}
		plain += "/" + seg
		if i < len(segs)-1 {
			next, ok := t.byName[parent.ID][tok]
			if !ok {
				return "", ErrNotFound
			}
			if !next.IsDir {
				return "", ErrNotDir
			}
			parent = next
		}
	}
	return plain, nil
}

// lookupStored resolves a full stored path to its row (every segment must
// exist, leaf included) — share targets need their row (the target's own key
// seals the share metadata copies).
func (t *sweepTree) lookupStored(stored string) (*File, error) {
	np, err := NormalizePath(stored)
	if err != nil {
		return nil, err
	}
	if t.root == nil {
		return nil, ErrNotFound
	}
	if np == "/" {
		return t.root, nil
	}
	parent := t.root
	for _, seg := range strings.Split(strings.TrimPrefix(np, "/"), "/") {
		next, ok := t.byName[parent.ID][seg]
		if !ok {
			return nil, ErrNotFound
		}
		parent = next
	}
	return parent, nil
}

// sweepLockPlan / sweepVersionPlan are row-id-keyed path rewrites.
type sweepLockPlan struct {
	id      int64
	newPath string
}

type sweepVersionPlan struct {
	id      int64
	newPath string
}

// sweepPropPlan rewrites one file_properties row (oc:favorite marks).
type sweepPropPlan struct {
	id      int64
	newPath string
}

// sweepTrashPlan rewrites one trash_items row and (when the location id
// changes) moves its storage object.
type sweepTrashPlan struct {
	id      int64
	newPath string
	newName string
	oldLoc  string
	newLoc  string
}

// sweepSharePlan rewrites one shares row: ciphertext file_path plus the
// phase-3a sealed metadata copies (encrypt), or plaintext with cleared enc
// fields (decrypt).
type sweepSharePlan struct {
	id       int64
	newPath  string
	mountEnc string
	absEnc   string
}

// sweepWrapPlan is one directory-key wrap for a share recipient.
type sweepWrapPlan struct {
	keyUUID [16]byte
	dk      []byte
	uid     string
}

// satellitePlan carries every satellite-table rewrite plus the skip count.
type satellitePlan struct {
	locks    []sweepLockPlan
	versions []sweepVersionPlan
	props    []sweepPropPlan
	trash    []sweepTrashPlan
	shares   []sweepSharePlan
	wraps    []sweepWrapPlan
	skipped  int
}

// planSatellites loads the user's file_locks / file_versions /
// file_properties / trash_items / shares rows and computes each one's
// converted form against the in-memory tree. Rows whose paths no longer
// resolve (ancestors deleted after the row was written) are skipped and
// counted — the documented phase-4 residual (retention/expiry self-heals
// them). Shares need their target row (its own key seals
// mount_name_enc/abs_path_enc exactly like phase-3a SealShareMeta); a share
// whose target is gone is skipped. A trash row with an invalid location id
// is skipped (its suffix cannot be trusted).
func (s *NameSweep) planSatellites(ctx context.Context, u *users.User, t *sweepTree, encrypting bool) (*satellitePlan, error) {
	p := &satellitePlan{}
	if err := s.planLocks(ctx, u.ID, t, encrypting, p); err != nil {
		return nil, err
	}
	if err := s.planVersions(ctx, u.ID, t, encrypting, p); err != nil {
		return nil, err
	}
	if err := s.planProps(ctx, u.ID, t, encrypting, p); err != nil {
		return nil, err
	}
	if err := s.planTrash(ctx, u.ID, t, encrypting, p); err != nil {
		return nil, err
	}
	if err := s.planShares(ctx, u, t, encrypting, p); err != nil {
		return nil, err
	}
	return p, nil
}

// convertPath maps one stored satellite path to the target scheme;
// unresolvable paths report ok=false (skip-and-count).
func (t *sweepTree) convertPath(stored string, encrypting bool) (string, bool, error) {
	var out string
	var err error
	if encrypting {
		out, err = t.cipherFor(stored)
	} else {
		out, err = t.plainFor(stored)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

func (s *NameSweep) planLocks(ctx context.Context, userID int64, t *sweepTree, encrypting bool, p *satellitePlan) error {
	rows, err := s.DB.Query(ctx, `SELECT id, file_path FROM file_locks WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return fmt.Errorf("list locks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			return fmt.Errorf("list locks: %w", err)
		}
		out, ok, err := t.convertPath(stored, encrypting)
		if err != nil {
			return fmt.Errorf("lock id %d (%q): %w", id, stored, err)
		}
		if !ok {
			p.skipped++
			continue
		}
		if out != stored {
			p.locks = append(p.locks, sweepLockPlan{id: id, newPath: out})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list locks: %w", err)
	}
	return nil
}

func (s *NameSweep) planVersions(ctx context.Context, userID int64, t *sweepTree, encrypting bool, p *satellitePlan) error {
	rows, err := s.DB.Query(ctx, `SELECT id, file_path FROM file_versions WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			return fmt.Errorf("list versions: %w", err)
		}
		out, ok, err := t.convertPath(stored, encrypting)
		if err != nil {
			return fmt.Errorf("version id %d (%q): %w", id, stored, err)
		}
		if !ok {
			p.skipped++
			continue
		}
		if out != stored {
			p.versions = append(p.versions, sweepVersionPlan{id: id, newPath: out})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list versions: %w", err)
	}
	return nil
}

// planProps converts file_properties rows (oc:favorite is the only
// persisted path-keyed property) — same shape as planLocks.
func (s *NameSweep) planProps(ctx context.Context, userID int64, t *sweepTree, encrypting bool, p *satellitePlan) error {
	rows, err := s.DB.Query(ctx, `SELECT id, file_path FROM file_properties WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return fmt.Errorf("list props: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stored string
		if err := rows.Scan(&id, &stored); err != nil {
			return fmt.Errorf("list props: %w", err)
		}
		out, ok, err := t.convertPath(stored, encrypting)
		if err != nil {
			return fmt.Errorf("prop id %d (%q): %w", id, stored, err)
		}
		if !ok {
			p.skipped++
			continue
		}
		if out != stored {
			p.props = append(p.props, sweepPropPlan{id: id, newPath: out})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list props: %w", err)
	}
	return nil
}

// planTrash converts trash_items rows: original_path and name re-token (or
// decrypt), and location_id keeps its ".d<unix>[-N]" suffix while its
// embedded basename swaps to the (200-char-capped) converted basename — the
// same contract the Trash service's LocationNamer seam produces at delete
// time (ADR-0104 §6). Final ids dedup with the "-N" convention against
// skipped rows' kept ids and ids already assigned this sweep (capped bases
// can collide where full names never did).
func (s *NameSweep) planTrash(ctx context.Context, userID int64, t *sweepTree, encrypting bool, p *satellitePlan) error {
	type trashRow struct {
		id       int64
		origPath string
		loc      string
	}
	var items []trashRow
	rows, err := s.DB.Query(ctx, `SELECT id, original_path, location_id FROM trash_items WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return fmt.Errorf("list trash: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item trashRow
		if err := rows.Scan(&item.id, &item.origPath, &item.loc); err != nil {
			return fmt.Errorf("list trash: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list trash: %w", err)
	}
	used := map[string]bool{}
	var converted []sweepTrashPlan
	for _, item := range items {
		_, suffix, ok := splitTrashLocationID(item.loc)
		if !ok {
			p.skipped++
			continue
		}
		out, resolved, err := t.convertPath(item.origPath, encrypting)
		if err != nil {
			return fmt.Errorf("trash id %d (%q): %w", item.id, item.origPath, err)
		}
		if !resolved {
			p.skipped++
			continue
		}
		converted = append(converted, sweepTrashPlan{
			id:      item.id,
			newPath: out,
			newName: path.Base(out),
			oldLoc:  item.loc,
			// Placeholder; the dedup pass below assigns the final id.
			newLoc: capTrashBase(path.Base(out)) + suffix,
		})
	}
	// Skipped rows keep their ids — they reserve them for the dedup.
	for _, item := range items {
		kept := true
		for i := range converted {
			if converted[i].oldLoc == item.loc {
				kept = false
				break
			}
		}
		if kept {
			used[item.loc] = true
		}
	}
	for i := range converted {
		converted[i].newLoc = dedupTrashLoc(converted[i].newLoc, used)
	}
	p.trash = converted
	return nil
}

// splitTrashLocationID splits a trash location id into its embedded basename
// and the ".d<unix>[-N]" suffix (the makeLocationID/uniqueLocationID
// contract); ok is false when the id does not parse (ValidLocationID rules).
func splitTrashLocationID(loc string) (base, suffix string, ok bool) {
	if !ValidLocationID(loc) {
		return "", "", false
	}
	i := strings.LastIndex(loc, ".d")
	if i <= 0 {
		return "", "", false
	}
	return loc[:i], loc[i:], true
}

// capTrashBase caps the basename a trash location id embeds so the id (base +
// ".d<unix>" + optional "-N") stays within ValidLocationID's 255 chars —
// trashLocationBaseMax, shared with the phase-2 LocationNamer seam. The cut
// backs off to a rune boundary (plaintext bases can be multibyte).
func capTrashBase(base string) string {
	if len(base) <= trashLocationBaseMax {
		return base
	}
	cut := trashLocationBaseMax
	for cut > 0 && !utf8.RuneStart(base[cut]) {
		cut--
	}
	return base[:cut]
}

// dedupTrashLoc returns loc, or loc+"-N" with the smallest free N, marking
// the result used (the uniqueLocationID convention).
func dedupTrashLoc(loc string, used map[string]bool) string {
	if !used[loc] {
		used[loc] = true
		return loc
	}
	for i := 1; ; i++ {
		candidate := loc + "-" + strconv.Itoa(i)
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
}

// planShares converts the owner's shares rows. Encrypt: the target row must
// exist in the live tree (its OWN key seals mount_name_enc/abs_path_enc —
// byte-identical to phase-3a SealShareMeta, with the DK taken from the
// in-memory key pass; a FILE target's FK resolves in the caller's ctx, which
// the enrolled gate guarantees can satisfy it). The subtree's directory keys
// then wrap for the share's recipients (user/group shares). Decrypt: the
// ciphertext file_path decrypts segment-wise and the enc fields clear.
func (s *NameSweep) planShares(ctx context.Context, u *users.User, t *sweepTree, encrypting bool, p *satellitePlan) error {
	type shareRow struct {
		id        int64
		shareType int
		filePath  string
		shareWith string
	}
	var shares []shareRow
	rows, err := s.DB.Query(ctx, `SELECT id, share_type, file_path, share_with FROM shares WHERE owner_user_id = ? ORDER BY id`, u.ID)
	if err != nil {
		return fmt.Errorf("list shares: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sh shareRow
		if err := rows.Scan(&sh.id, &sh.shareType, &sh.filePath, &sh.shareWith); err != nil {
			return fmt.Errorf("list shares: %w", err)
		}
		shares = append(shares, sh)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list shares: %w", err)
	}
	for _, sh := range shares {
		np, err := NormalizePath(sh.filePath)
		if err != nil {
			return fmt.Errorf("share id %d (%q): %w", sh.id, sh.filePath, err)
		}
		if !encrypting {
			plain, ok, err := t.convertPath(np, false)
			if err != nil {
				return fmt.Errorf("share id %d (%q): %w", sh.id, sh.filePath, err)
			}
			if !ok {
				p.skipped++
				continue
			}
			p.shares = append(p.shares, sweepSharePlan{id: sh.id, newPath: plain})
			continue
		}
		target, err := t.lookupStored(np)
		if err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotDir) {
				p.skipped++
				continue
			}
			return fmt.Errorf("share id %d (%q): %w", sh.id, sh.filePath, err)
		}
		dk, uuid, err := s.shareTargetKey(ctx, t, target)
		if err != nil {
			return fmt.Errorf("share id %d (%q): %w", sh.id, sh.filePath, err)
		}
		nk, err := encrypt.DeriveNameKey(dk, uuid)
		if err != nil {
			return err
		}
		mountEnc, err := encrypt.EncryptName(nk, uuid, path.Base(np))
		if err != nil {
			return err
		}
		absEnc, err := encrypt.SealPath(dk, uuid, np)
		if err != nil {
			return err
		}
		p.shares = append(p.shares, sweepSharePlan{
			id:       sh.id,
			newPath:  t.newPath[target.ID],
			mountEnc: mountEnc,
			absEnc:   absEnc,
		})
		wraps, err := s.planShareWraps(ctx, t, target, sh.shareType, sh.shareWith, u.UID)
		if err != nil {
			return fmt.Errorf("share id %d (%q): %w", sh.id, sh.filePath, err)
		}
		p.wraps = append(p.wraps, wraps...)
	}
	return nil
}

// shareTargetKey returns the key that seals a share row's metadata copies:
// the target's OWN key — the directory key of a folder target (from the
// in-memory pass), the file key of a file target (resolved in the caller's
// ctx). Mirrors phase-3a SealShareMeta.
func (s *NameSweep) shareTargetKey(ctx context.Context, t *sweepTree, target *File) ([]byte, [16]byte, error) {
	var uuid [16]byte
	if target.IsDir {
		dk, ok := t.dk[target.ID]
		if !ok {
			return nil, uuid, fmt.Errorf("folder %q has no directory key", target.Path)
		}
		return dk, t.uuid[target.ID], nil
	}
	if len(target.KeyUUID) != keyUUIDSize {
		return nil, uuid, fmt.Errorf("share target %q is an unsealed file (nothing to key share metadata with)", target.Path)
	}
	copy(uuid[:], target.KeyUUID)
	fk, err := s.Keys.Resolve(ctx, uuid)
	if err != nil {
		return nil, uuid, err
	}
	return fk, uuid, nil
}

// planShareWraps expands one share to the directory-key wraps its recipients
// need: every folder at or below the target (the target included) whose DK
// the sweep holds wraps for the sharee (user shares) or every current group
// member (group shares). Link and OCM-remote shares have no wrappable
// recipient. The owner never wraps for themselves (AllocateForUser wrote
// their row). Wraps dedup by (key uuid, uid) so re-runs plan nothing new.
func (s *NameSweep) planShareWraps(ctx context.Context, t *sweepTree, target *File, shareType int, shareWith, ownerUID string) ([]sweepWrapPlan, error) {
	var recipients []string
	switch shareType {
	case ShareTypeUser:
		if shareWith != "" {
			recipients = []string{shareWith}
		}
	case ShareTypeGroup:
		members, err := s.Users.GroupMembers(ctx, shareWith, 0)
		if err != nil {
			return nil, err
		}
		recipients = members
	default:
		return nil, nil
	}
	if len(recipients) == 0 || !target.IsDir {
		return nil, nil
	}
	var out []sweepWrapPlan
	seen := map[string]bool{}
	queue := []*File{target}
	for len(queue) > 0 {
		folder := queue[0]
		queue = queue[1:]
		if dk, ok := t.dk[folder.ID]; ok {
			for _, uid := range recipients {
				key := fmt.Sprintf("%x/%s", t.uuid[folder.ID], uid)
				if seen[key] || uid == "" || uid == ownerUID {
					continue
				}
				seen[key] = true
				out = append(out, sweepWrapPlan{keyUUID: t.uuid[folder.ID], dk: dk, uid: uid})
			}
		}
		queue = append(queue, t.kids[folder.ID]...)
	}
	return out, nil
}

// writeShareWraps persists the planned directory-key recipient wraps
// (no-op-on-duplicate inserts; racing sweeps tolerate each other).
func (s *NameSweep) writeShareWraps(ctx context.Context, wraps []sweepWrapPlan) error {
	for _, w := range wraps {
		if err := s.Keys.WrapKeyForFK(ctx, w.keyUUID, w.uid, w.dk); err != nil {
			return fmt.Errorf("wrap directory key for %q: %w", w.uid, err)
		}
	}
	return nil
}

// sweepTrashMove records one trash storage object rename for compensation.
type sweepTrashMove struct {
	from string
	to   string
}

// moveTrashObjects renames trash storage objects to their converted location
// ids BEFORE the transaction (the tx must see final ids; storage cannot join
// the DB tx). A missing object is tolerated (the row still converts — the
// pre-sweep state was already broken that way); other errors abort.
func (s *NameSweep) moveTrashObjects(ctx context.Context, uid string, plans []sweepTrashPlan) ([]sweepTrashMove, error) {
	var moved []sweepTrashMove
	for _, tm := range plans {
		if tm.oldLoc == tm.newLoc {
			continue
		}
		from, err := trashStorageKey(uid, tm.oldLoc)
		if err != nil {
			return moved, err
		}
		to, err := trashStorageKey(uid, tm.newLoc)
		if err != nil {
			return moved, err
		}
		if err := s.Storage.Rename(ctx, from, to); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return moved, err
		}
		moved = append(moved, sweepTrashMove{from: from, to: to})
	}
	return moved, nil
}

// compensateTrashMoves best-effort undoes the trash object renames after a
// failed tx (reverse order, NotFound-tolerant, Warn-logged — never masks the
// tx error).
func (s *NameSweep) compensateTrashMoves(ctx context.Context, moved []sweepTrashMove) {
	for i := len(moved) - 1; i >= 0; i-- {
		if err := s.Storage.Rename(ctx, moved[i].to, moved[i].from); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.log().WarnContext(ctx, "files: name sweep: trash object move-back failed (orphaned trash object)",
				slog.String("from", moved[i].to), slog.String("to", moved[i].from), slog.Any("error", err))
		}
	}
}

// commitPlan writes the whole conversion in ONE transaction — a mid-sweep
// mixed tree is unreadable (reads key off users.name_scheme), so the files
// rewrite, the satellite rewrites, and the write-switch flip commit
// atomically. Write order: files rows (by id; minted folder key_uuids claim
// with a key_uuid IS NULL race guard), file_locks, file_versions,
// file_properties, trash_items, shares, (decrypt only) folder wrap-row
// deletes, and LAST the
// users flip guarded on the old scheme — a zero-row guard means a racing
// sweep won and this tx rolls back loudly. Every update checks its
// RowsAffected: a row that changed underneath the sweep fails the tx.
func (s *NameSweep) commitPlan(ctx context.Context, userID int64, t *sweepTree, p *satellitePlan, encrypting bool) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin sweep tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			//nolint:errcheck // a rollback after the failure path has no reportable error; the original error already rides back
			_ = tx.Rollback()
		}
	}()
	exec1 := func(what, q string, args ...any) error {
		res, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if n != 1 {
			return fmt.Errorf("%s: %d rows affected, want 1 (the row changed underneath the sweep)", what, n)
		}
		return nil
	}
	newScheme := 0
	oldScheme := encrypt.NameSchemeNCGOFN1
	if encrypting {
		newScheme = encrypt.NameSchemeNCGOFN1
		oldScheme = 0
	}
	for _, f := range t.rows {
		name, ok := t.newName[f.ID]
		if !ok && t.root != nil {
			return fmt.Errorf("files row %d (%q) missed the conversion plan", f.ID, f.Path)
		}
		newPath := t.newPath[f.ID]
		if encrypting && t.minted[f.ID] {
			uuid := t.uuid[f.ID]
			if err := exec1(fmt.Sprintf("files row %d", f.ID), `
UPDATE files SET name = ?, path = ?, name_scheme = ?, key_uuid = ? WHERE id = ? AND key_uuid IS NULL`,
				name, newPath, newScheme, uuid[:], f.ID); err != nil {
				return err
			}
			continue
		}
		if !encrypting && f.IsDir && len(f.KeyUUID) > 0 {
			// Decommission cleanliness: folder DKs and their wraps leave with
			// the mode; files keep their FKs (content stays sealed).
			if err := exec1(fmt.Sprintf("files row %d", f.ID), `
UPDATE files SET name = ?, path = ?, name_scheme = ?, key_uuid = NULL WHERE id = ?`,
				name, newPath, newScheme, f.ID); err != nil {
				return err
			}
			continue
		}
		if err := exec1(fmt.Sprintf("files row %d", f.ID), `
UPDATE files SET name = ?, path = ?, name_scheme = ? WHERE id = ?`,
			name, newPath, newScheme, f.ID); err != nil {
			return err
		}
	}
	for _, l := range p.locks {
		if err := exec1(fmt.Sprintf("file_locks row %d", l.id), `
UPDATE file_locks SET file_path = ? WHERE id = ?`, l.newPath, l.id); err != nil {
			return err
		}
	}
	for _, v := range p.versions {
		if err := exec1(fmt.Sprintf("file_versions row %d", v.id), `
UPDATE file_versions SET file_path = ? WHERE id = ?`, v.newPath, v.id); err != nil {
			return err
		}
	}
	for _, pr := range p.props {
		if err := exec1(fmt.Sprintf("file_properties row %d", pr.id), `
UPDATE file_properties SET file_path = ? WHERE id = ?`, pr.newPath, pr.id); err != nil {
			return err
		}
	}
	for _, tm := range p.trash {
		if err := exec1(fmt.Sprintf("trash_items row %d", tm.id), `
UPDATE trash_items SET original_path = ?, name = ?, location_id = ? WHERE id = ?`,
			tm.newPath, tm.newName, tm.newLoc, tm.id); err != nil {
			return err
		}
	}
	for _, sh := range p.shares {
		if err := exec1(fmt.Sprintf("shares row %d", sh.id), `
UPDATE shares SET file_path = ?, mount_name_enc = ?, abs_path_enc = ? WHERE id = ?`,
			sh.newPath, sh.mountEnc, sh.absEnc, sh.id); err != nil {
			return err
		}
	}
	if !encrypting {
		for _, f := range t.rows {
			if !f.IsDir || len(f.KeyUUID) == 0 {
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM file_keys WHERE key_uuid = ?`, f.KeyUUID); err != nil {
				return fmt.Errorf("delete folder key wraps of row %d: %w", f.ID, err)
			}
		}
	}
	// The write switch flips LAST, guarded on the old scheme: a racing sweep
	// (two logins at once) finds the guard row gone and rolls back loudly —
	// wrap inserts were no-op-on-duplicate, so the loser retries cleanly.
	if err := exec1("users name_scheme flip", `
UPDATE users SET name_scheme = ? WHERE id = ? AND name_scheme = ?`, newScheme, userID, oldScheme); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sweep tx: %w", err)
	}
	committed = true
	return nil
}
