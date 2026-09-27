package files

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// stubNameResolver resolves fixed directory keys by key UUID, so the core
// tests compute tokens by hand from known DKs (the phase-1 NCGOFN1 vectors).
type stubNameResolver struct {
	keys  map[[16]byte][]byte
	calls int
}

func (s *stubNameResolver) Resolve(_ context.Context, id [16]byte) ([]byte, error) {
	s.calls++
	dk, ok := s.keys[id]
	if !ok {
		return nil, fmt.Errorf("stub resolver: unknown key uuid %x", id)
	}
	return dk, nil
}

// stubNameSchemes is the users.name_scheme seam for the core tests.
type stubNameSchemes map[int64]int

func (s stubNameSchemes) UserNameScheme(_ context.Context, userID int64) (int, error) {
	return s[userID], nil
}

// The phase-1 frozen NCGOFN1 vector material (storage/encrypt
// nametoken_test.go): DK = bytes 0x00..0x1f, parent key UUID = 0xa0..0xaf.
func vectorDK() []byte {
	dk := make([]byte, 32)
	for i := range dk {
		dk[i] = byte(i)
	}
	return dk
}

func vectorParentUUID() (id [16]byte) {
	for i := range id {
		id[i] = byte(0xa0 + i)
	}
	return id
}

// fixedKey materializes a deterministic key UUID + DK pair per seed.
func fixedKey(seed byte) (uuid [16]byte, dk []byte) {
	for i := range uuid {
		uuid[i] = seed + byte(i)
	}
	dk = make([]byte, 32)
	for i := range dk {
		dk[i] = seed ^ byte(i)
	}
	return uuid, dk
}

// tokenOf computes the NCGOFN1 token of name under (dk, parentUUID).
func tokenOf(t *testing.T, dk []byte, parentUUID [16]byte, name string) string {
	t.Helper()
	nk, err := encrypt.DeriveNameKey(dk, parentUUID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := encrypt.EncryptName(nk, parentUUID, name)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// nameCoreEnv wires the translator over a raw SQLStore on sqlite with a stub
// resolver. The scheme-1 root carries the phase-1 vector DK, so root-level
// tokens are the frozen test vectors.
type nameCoreEnv struct {
	db    database.DB
	raw   *SQLStore
	tr    *NameTranslator
	store *TranslatingStore
	res   *stubNameResolver
	uid   int64
	root  *File
}

func newNameCoreEnv(t *testing.T, scheme int) *nameCoreEnv {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	uid := seedUser(t, db)
	raw := NewSQLStore(db)
	root, err := raw.EnsureRoot(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	res := &stubNameResolver{keys: map[[16]byte][]byte{}}
	tr := NewNameTranslator(raw, res, stubNameSchemes{uid: scheme})
	env := &nameCoreEnv{db: db, raw: raw, tr: tr, store: NewTranslatingStore(raw, tr), res: res, uid: uid, root: root}
	if scheme == encrypt.NameSchemeNCGOFN1 {
		env.claimDK(t, root, vectorParentUUID(), vectorDK())
	}
	return env
}

// claimDK pins a directory key onto a folder row — what the DAV DirKeys mint
// does at folder creation — and teaches the stub resolver.
func (e *nameCoreEnv) claimDK(t *testing.T, f *File, uuid [16]byte, dk []byte) {
	t.Helper()
	claimed, err := e.raw.SetKeyUUIDIfNull(context.Background(), f.ID, uuid[:])
	if err != nil || !claimed {
		t.Fatalf("claim DK: %v %v", claimed, err)
	}
	e.res.keys[uuid] = dk
	f.KeyUUID = uuid[:]
}

// mkdir inserts a folder through the translating store and claims its DK.
func (e *nameCoreEnv) mkdir(t *testing.T, p string, seed byte) *File {
	t.Helper()
	f := &File{UserID: e.uid, Path: p, IsDir: true, Permissions: 31}
	if err := e.store.Insert(context.Background(), f); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	uuid, dk := fixedKey(seed)
	e.claimDK(t, f, uuid, dk)
	return f
}

func (e *nameCoreEnv) mkfile(t *testing.T, p string) *File {
	t.Helper()
	f := &File{UserID: e.uid, Path: p, Size: 1, Permissions: 31}
	if err := e.store.Insert(context.Background(), f); err != nil {
		t.Fatalf("mkfile %s: %v", p, err)
	}
	return f
}

// rawRow re-reads the row through the raw store — the ciphertext view.
func (e *nameCoreEnv) rawRow(t *testing.T, id int64) *File {
	t.Helper()
	f, err := e.raw.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (e *nameCoreEnv) cipher(t *testing.T, p string) string {
	t.Helper()
	ct, err := e.tr.cipherPath(context.Background(), newTranslateCache(), e.uid, p)
	if err != nil {
		t.Fatalf("cipherPath %q: %v", p, err)
	}
	return ct
}

func TestCipherPathScheme0Passthrough(t *testing.T) {
	env := newNameCoreEnv(t, 0)
	// No root DK claimed: the fast path must not touch the resolver at all,
	// and missing intermediates are irrelevant (the raw SQL just misses).
	for _, p := range []string{"/", "/docs", "/docs/报告.txt", "/a/b/c"} {
		got, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, p)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		if got != p {
			t.Errorf("%q: got %q", p, got)
		}
	}
	if env.res.calls != 0 {
		t.Errorf("resolver calls = %d, want 0 (scheme-0 fast path)", env.res.calls)
	}
	// Writes stay plaintext with the row marker 0.
	f := &File{UserID: env.uid, Path: "/docs", IsDir: true, Permissions: 31}
	if err := env.store.Insert(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	row := env.rawRow(t, f.ID)
	if row.Name != "docs" || row.Path != "/docs" || row.NameScheme != 0 {
		t.Errorf("scheme-0 row = %+v, want plaintext marker 0", row)
	}
}

func TestCipherPathRootNeverEncrypted(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	got, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/")
	if err != nil || got != "/" {
		t.Fatalf("root = %q %v", got, err)
	}
	if env.res.calls != 0 {
		t.Errorf("resolver calls = %d, want 0 (the root is never encrypted)", env.res.calls)
	}
	if env.root.NameScheme != 0 || env.root.Name != "" || env.root.Path != "/" {
		t.Errorf("root row = %+v, want literal root marker 0", env.root)
	}
}

// TestCipherPathFrozenVectors pins the segment walk against the phase-1
// frozen NCGOFN1 vectors: fixed root DK + key UUID → fixed tokens.
func TestCipherPathFrozenVectors(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	vectors := []struct{ plain, ct string }{
		{"/a", "/rqZrAhgnaEN35P61yNTBQ3UuflDzpi5AUWAqpjg"},
		{"/Photos", "/qZWZpJKpWz_8nGJnVovb1X67-6Fitow0FSkVwM83aEwduQ"},
		{"/2026-裁员名单.xlsx", "/roymrFSmPkViy4_10QDSBJwwPvON_7tS94zFQ8zsseHAQOb7KkjfSdPugIrleR_kObc"},
	}
	for _, v := range vectors {
		if got := env.cipher(t, v.plain); got != v.ct {
			t.Errorf("%q: token path = %q, want %q", v.plain, got, v.ct)
		}
	}

	// Nested: the child's token derives from the parent's own DK.
	env.mkdir(t, "/Photos", 0x10)
	uuid, dk := fixedKey(0x10)
	want := "/qZWZpJKpWz_8nGJnVovb1X67-6Fitow0FSkVwM83aEwduQ/" + tokenOf(t, dk, uuid, "cat.jpg")
	if got := env.cipher(t, "/Photos/cat.jpg"); got != want {
		t.Errorf("nested token path = %q, want %q", got, want)
	}
}

// TestCipherPathLeafVsIntermediate pins existence handling: the final
// segment's token needs only its parent's key (create, lock-null), while a
// missing intermediate is ErrNotFound and a file intermediate is ErrNotDir.
func TestCipherPathLeafVsIntermediate(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkdir(t, "/Photos", 0x10)
	env.mkfile(t, "/a.txt")

	// The leaf does not exist — the walk still resolves (create/lock-null).
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/Photos/new.txt"); err != nil {
		t.Errorf("nonexistent leaf: %v", err)
	}
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/nope/x.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing intermediate = %v, want ErrNotFound", err)
	}
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/a.txt/b"); !errors.Is(err, ErrNotDir) {
		t.Errorf("file intermediate = %v, want ErrNotDir", err)
	}
}

// TestCipherPathBudgets pins the ADR-0104 §3 length budget: 255 runes per
// name and 768 chars per ciphertext path, both ErrNameBudget.
func TestCipherPathBudgets(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)

	// 255 runes fit; 256 fail the name budget.
	ascii255 := "/" + strings.Repeat("n", 255)
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, ascii255); err != nil {
		t.Errorf("255-rune name: %v", err)
	}
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/"+strings.Repeat("n", 256)); !errors.Is(err, ErrNameBudget) {
		t.Errorf("256-rune name = %v, want ErrNameBudget", err)
	}
	// 255 CJK runes pass the rune budget but blow the 768-char ciphertext
	// path budget (765 name bytes → a ~1058-char token).
	if _, err := env.tr.cipherPath(context.Background(), newTranslateCache(), env.uid, "/"+strings.Repeat("界", 255)); !errors.Is(err, ErrNameBudget) {
		t.Errorf("255 CJK runes = %v, want ErrNameBudget (path budget)", err)
	}

	// Depth: one-character names tokenize to 39 chars, so 19 levels fit
	// (760 chars) and the 20th exceeds 768.
	p := ""
	for i := 1; i <= 19; i++ {
		p += "/a"
		env.mkdir(t, p, byte(i))
	}
	deep := &File{UserID: env.uid, Path: p + "/a", IsDir: true, Permissions: 31}
	if err := env.store.Insert(context.Background(), deep); !errors.Is(err, ErrNameBudget) {
		t.Errorf("depth-20 insert = %v, want ErrNameBudget", err)
	}
}

// TestTranslatingStoreRoundTrip pins the write/read translation: DB rows
// literally hold tokens while the store boundary speaks plaintext.
func TestTranslatingStoreRoundTrip(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	docs := env.mkdir(t, "/docs", 0x20)
	a := env.mkfile(t, "/docs/a.txt")
	env.mkfile(t, "/docs/b.txt")

	// The DB literally holds tokens: name/path are ciphertext, marked
	// scheme 1; the root stays the literal root.
	uuid, dk := fixedKey(0x20)
	docsRow := env.rawRow(t, docs.ID)
	if want := tokenOf(t, vectorDK(), vectorParentUUID(), "docs"); docsRow.Name != want {
		t.Errorf("/docs stored name = %q, want the hand-computed token %q", docsRow.Name, want)
	}
	if docsRow.Path != "/"+docsRow.Name || docsRow.NameScheme != encrypt.NameSchemeNCGOFN1 {
		t.Errorf("/docs row path/scheme = %q %d", docsRow.Path, docsRow.NameScheme)
	}
	aRow := env.rawRow(t, a.ID)
	if want := tokenOf(t, dk, uuid, "a.txt"); aRow.Name != want {
		t.Errorf("a.txt stored name = %q, want %q", aRow.Name, want)
	}
	if aRow.Path != docsRow.Path+"/"+aRow.Name {
		t.Errorf("a.txt stored path = %q, want prefix+token", aRow.Path)
	}
	rootRow := env.rawRow(t, env.root.ID)
	if rootRow.Name != "" || rootRow.Path != "/" || rootRow.NameScheme != 0 {
		t.Errorf("root row = %+v, want literal root", rootRow)
	}

	// Reads decrypt: GetByPath, GetByID, ListChildren all speak plaintext.
	got, err := env.store.GetByPath(context.Background(), env.uid, "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID || got.Name != "a.txt" || got.Path != "/docs/a.txt" || got.NameScheme != encrypt.NameSchemeNCGOFN1 {
		t.Errorf("GetByPath = %+v", got)
	}
	byID, err := env.store.GetByID(context.Background(), a.ID)
	if err != nil || byID.Path != "/docs/a.txt" {
		t.Errorf("GetByID = %+v %v", byID, err)
	}
	kids, err := env.store.ListChildren(context.Background(), env.uid, docs.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 || kids[0].Name != "a.txt" || kids[1].Name != "b.txt" {
		t.Fatalf("children = %+v", kids)
	}
	if kids[0].Path != "/docs/a.txt" || kids[1].Path != "/docs/b.txt" {
		t.Errorf("child paths = %q %q", kids[0].Path, kids[1].Path)
	}
	if _, err := env.store.GetByPath(context.Background(), env.uid, "/docs/nope.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v, want ErrNotFound", err)
	}
}

// TestTranslatingStoreDualRead pins mixed-tree reads: a scheme-0 row under a
// scheme-1 user passes through verbatim on id/list reads (and, per the
// whole-tree cutover invariant, is not addressable by the encrypted path
// lookup).
func TestTranslatingStoreDualRead(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkfile(t, "/enc.txt")
	legacy := &File{UserID: env.uid, Path: "/legacy.txt", Size: 1, Permissions: 31}
	if err := env.raw.Insert(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}

	byID, err := env.store.GetByID(context.Background(), legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.Name != "legacy.txt" || byID.Path != "/legacy.txt" || byID.NameScheme != 0 {
		t.Errorf("dual-read row = %+v, want plaintext passthrough", byID)
	}
	kids, err := env.store.ListChildren(context.Background(), env.uid, env.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 || kids[0].Name != "enc.txt" || kids[1].Name != "legacy.txt" {
		t.Fatalf("mixed children = %+v", kids)
	}
	// Path lookup of a scheme-0 row under a scheme-1 user misses: the lookup
	// encrypts, and no ciphertext row exists. Whole-tree cutover (phase 4)
	// is what keeps this state transient.
	if _, err := env.store.GetByPath(context.Background(), env.uid, "/legacy.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("legacy path lookup = %v, want ErrNotFound (ciphertext lookup)", err)
	}
}

// TestTranslatingStoreTamperedToken pins that a token failing authentication
// surfaces ErrIntegrity — never a silent plaintext fallback.
func TestTranslatingStoreTamperedToken(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	docs := env.mkdir(t, "/docs", 0x20)
	a := env.mkfile(t, "/docs/a.txt")

	flip := func(s string) string {
		if s[0] == '_' {
			return "-" + s[1:]
		}
		return "_" + s[1:]
	}

	// A tampered leaf token fails GetByID with ErrIntegrity.
	row := env.rawRow(t, a.ID)
	if _, err := env.db.Exec(ctx, `UPDATE files SET name = ? WHERE id = ?`, flip(row.Name), a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.GetByID(ctx, a.ID); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered leaf = %v, want ErrIntegrity", err)
	}
	if _, err := env.db.Exec(ctx, `UPDATE files SET name = ? WHERE id = ?`, row.Name, a.ID); err != nil {
		t.Fatal(err)
	}

	// A tampered parent token fails listings loudly (no silent skip).
	drow := env.rawRow(t, docs.ID)
	if _, err := env.db.Exec(ctx, `UPDATE files SET name = ? WHERE id = ?`, flip(drow.Name), docs.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.ListChildren(ctx, env.uid, env.root.ID); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered parent in list = %v, want ErrIntegrity", err)
	}
}

// TestTranslatingStoreListChildrenSort pins the post-decryption plaintext
// sort (ORDER BY path over tokens is meaningless; the Go sort unifies
// dialect order): bytewise by (Name, ID).
func TestTranslatingStoreListChildrenSort(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	for _, n := range []string{"/b.txt", "/A.txt", "/c.txt"} {
		env.mkfile(t, n)
	}
	kids, err := env.store.ListChildren(context.Background(), env.uid, env.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A.txt", "b.txt", "c.txt"}
	if len(kids) != len(want) {
		t.Fatalf("children = %+v", kids)
	}
	for i, w := range want {
		if kids[i].Name != w {
			t.Fatalf("plaintext order = %v, want %v", []string{kids[0].Name, kids[1].Name, kids[2].Name}, want)
		}
	}
}

// TestTranslatingStoreRenameMoveTokens pins the ADR-0104 §5 token math: a
// same-parent rename re-tokens the basename under the same parent key; a
// cross-parent move re-tokens under the destination parent's key; descendant
// tokens stay unchanged in both cases — only their path strings get the
// prefix rewrite.
func TestTranslatingStoreRenameMoveTokens(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	docs := env.mkdir(t, "/docs", 0x20)
	sub := env.mkdir(t, "/docs/sub", 0x21)
	f := env.mkfile(t, "/docs/sub/f.txt")
	other := env.mkdir(t, "/other", 0x22)

	docsUUID, docsDK := fixedKey(0x20)
	otherUUID, otherDK := fixedKey(0x22)

	subRow := env.rawRow(t, sub.ID)
	fRow := env.rawRow(t, f.ID)
	fTok, fPath := fRow.Name, fRow.Path
	if subRow.Name != tokenOf(t, docsDK, docsUUID, "sub") {
		t.Fatalf("sub token = %q", subRow.Name)
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	// Same-parent rename: /docs/sub → /docs/sub2.
	if err := env.store.RenameSubtree(ctx, env.uid, "/docs/sub", "/docs/sub2", now); err != nil {
		t.Fatal(err)
	}
	sub2 := env.rawRow(t, sub.ID)
	if want := tokenOf(t, docsDK, docsUUID, "sub2"); sub2.Name != want {
		t.Errorf("renamed name = %q, want re-tokened %q", sub2.Name, want)
	}
	if sub2.ParentID == nil || *sub2.ParentID != docs.ID {
		t.Errorf("renamed parent = %v, want /docs", sub2.ParentID)
	}
	f2 := env.rawRow(t, f.ID)
	if f2.Name != fTok {
		t.Errorf("descendant token changed on rename: %q → %q", fTok, f2.Name)
	}
	if f2.Path != sub2.Path+"/"+fTok {
		t.Errorf("descendant path = %q, want prefix rewrite of %q", f2.Path, fPath)
	}

	// Cross-parent move: /docs/sub2 → /other/sub2.
	if err := env.store.RenameSubtree(ctx, env.uid, "/docs/sub2", "/other/sub2", now); err != nil {
		t.Fatal(err)
	}
	sub3 := env.rawRow(t, sub.ID)
	if want := tokenOf(t, otherDK, otherUUID, "sub2"); sub3.Name != want {
		t.Errorf("moved name = %q, want token under destination parent %q", sub3.Name, want)
	}
	if sub3.ParentID == nil || *sub3.ParentID != other.ID {
		t.Errorf("moved parent = %v, want /other", sub3.ParentID)
	}
	f3 := env.rawRow(t, f.ID)
	if f3.Name != fTok {
		t.Errorf("descendant token changed on move: %q → %q", fTok, f3.Name)
	}
	if f3.Path != sub3.Path+"/"+fTok || f3.ParentID == nil || *f3.ParentID != sub.ID {
		t.Errorf("descendant after move = %q parent %v", f3.Path, f3.ParentID)
	}
	// The plaintext view keeps resolving.
	if _, err := env.store.GetByPath(ctx, env.uid, "/other/sub2/f.txt"); err != nil {
		t.Errorf("moved lookup: %v", err)
	}

	// Error mapping: missing destination parent → ErrParentMissing; an
	// existing destination → ErrExists (token determinism preserves the
	// collision check).
	if err := env.store.RenameSubtree(ctx, env.uid, "/other/sub2", "/nope/sub2", now); !errors.Is(err, ErrParentMissing) {
		t.Errorf("missing dst parent = %v, want ErrParentMissing", err)
	}
	env.mkdir(t, "/other/exists", 0x23)
	if err := env.store.RenameSubtree(ctx, env.uid, "/other/sub2", "/other/exists", now); !errors.Is(err, ErrExists) {
		t.Errorf("existing dst = %v, want ErrExists", err)
	}
}

// TestTranslatingStoreUpdateMeta pins that meta updates re-encrypt the
// decrypted caller view to the identical deterministic tokens (name/path are
// rewrite-stable) and restore the plaintext view afterwards.
func TestTranslatingStoreUpdateMeta(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	a := env.mkfile(t, "/a.txt")
	before := env.rawRow(t, a.ID)

	got, err := env.store.GetByPath(ctx, env.uid, "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got.Size = 99
	got.ETag = "rotated"
	if err := env.store.UpdateMeta(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a.txt" || got.Path != "/a.txt" {
		t.Errorf("caller view after update = %q %q, want plaintext", got.Name, got.Path)
	}
	after := env.rawRow(t, a.ID)
	if after.Name != before.Name || after.Path != before.Path {
		t.Errorf("tokens changed across meta update: %q → %q", before.Name, after.Name)
	}
	if after.Size != 99 || after.ETag != "rotated" {
		t.Errorf("meta update did not land: %+v", after)
	}

	if err := env.store.UpdateMetaIfETag(ctx, got, "bogus"); !errors.Is(err, ErrETagConflict) {
		t.Errorf("stale guard = %v, want ErrETagConflict", err)
	}
	if err := env.store.UpdateMetaIfETag(ctx, got, "rotated"); err != nil {
		t.Errorf("matching guard: %v", err)
	}
}

// TestTranslatingStoreSearch pins scheme-1 scan-and-fold search: Unicode
// case-folded contains, limit after filtering, (Name, ID) order; and the
// mixed-tree dual-read inclusion of scheme-0 rows.
func TestTranslatingStoreSearch(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkfile(t, "/Hello.TXT")
	env.mkfile(t, "/hello_world.txt")
	env.mkfile(t, "/notes.md")
	env.mkdir(t, "/docs", 0x30)
	env.mkfile(t, "/docs/HELLO-again.txt")
	env.mkfile(t, "/Äpfel.txt")
	legacy := &File{UserID: env.uid, Path: "/LEGACY-hello.txt", Size: 1, Permissions: 31}
	if err := env.raw.Insert(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	names := func(term string, limit int) []string {
		t.Helper()
		hits, err := env.store.SearchByName(ctx, env.uid, term, limit)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(hits))
		for _, h := range hits {
			out = append(out, h.Name)
		}
		return out
	}
	join := func(xs []string) string { return strings.Join(xs, ",") }

	// Case-folded contains, sorted bytewise by (Name, ID); the dual-read
	// scheme-0 row matches too.
	if got := names("hello", 20); join(got) != "HELLO-again.txt,Hello.TXT,LEGACY-hello.txt,hello_world.txt" {
		t.Errorf("hello = %v", got)
	}
	if got := names("HELLO", 20); len(got) != 4 {
		t.Errorf("HELLO = %v", got)
	}
	// Unicode fold: lowercase ä matches uppercase Ä.
	if got := names("äpfel", 20); join(got) != "Äpfel.txt" {
		t.Errorf("äpfel = %v", got)
	}
	// Limit applies AFTER filtering, on the sorted set.
	if got := names("hello", 2); join(got) != "HELLO-again.txt,Hello.TXT" {
		t.Errorf("limit 2 = %v", got)
	}
	if got := names("nomatch", 20); len(got) != 0 {
		t.Errorf("nomatch = %v", got)
	}
	if got := names("   ", 20); len(got) != 0 {
		t.Errorf("empty term = %v", got)
	}
}

// TestTranslatingStoreSearchScheme0 pins that a scheme-0 user keeps the raw
// SQL LIKE path untouched through the wrapper.
func TestTranslatingStoreSearchScheme0(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, 0)
	for _, p := range []string{"/hello.txt", "/hello_world.txt", "/100%off.txt"} {
		env.mkfile(t, p)
	}
	hits, err := env.store.SearchByName(ctx, env.uid, "hello", 20)
	if err != nil || len(hits) != 2 {
		t.Fatalf("hello = %v %v", hits, err)
	}
	if hits[0].Name != "hello.txt" || hits[1].Name != "hello_world.txt" {
		t.Errorf("order = %+v", hits)
	}
	// SQL escaping still applies on the raw path.
	hits, err = env.store.SearchByName(ctx, env.uid, "100%", 20)
	if err != nil || len(hits) != 1 || hits[0].Name != "100%off.txt" {
		t.Errorf("escaped percent = %v %v", hits, err)
	}
	if env.res.calls != 0 {
		t.Errorf("resolver calls = %d, want 0 on the SQL path", env.res.calls)
	}
}

// TestTranslatingStoreBudgetErrors pins ErrNameBudget surfacing through
// Insert and RenameSubtree (DAV maps it to 400).
func TestTranslatingStoreBudgetErrors(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	big := "/" + strings.Repeat("n", 256)
	if err := env.store.Insert(ctx, &File{UserID: env.uid, Path: big, Size: 1, Permissions: 31}); !errors.Is(err, ErrNameBudget) {
		t.Errorf("insert 256-rune name = %v, want ErrNameBudget", err)
	}
	env.mkfile(t, "/a.txt")
	if err := env.store.RenameSubtree(ctx, env.uid, "/a.txt", big, time.Now()); !errors.Is(err, ErrNameBudget) {
		t.Errorf("rename to 256-rune name = %v, want ErrNameBudget", err)
	}
}

// TestTranslatingLockStore pins ciphertext at rest with the plaintext echo
// on the boundary, including the lock-null (nonexistent leaf) case.
func TestTranslatingLockStore(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkdir(t, "/docs", 0x40)
	locks := NewTranslatingLockStore(NewSQLLockStore(env.db), env.tr)

	l := &FileLock{UserID: env.uid, Path: "/docs/f.txt", Token: "opaquelocktoken:aaaa", Owner: "alice", TimeoutMs: 100, CreatedMs: 1}
	if err := locks.Insert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if l.Path != "/docs/f.txt" {
		t.Errorf("insert echo = %q, want the plaintext request path", l.Path)
	}
	var stored string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_locks WHERE id = ?`, l.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := env.cipher(t, "/docs/f.txt"); stored != want {
		t.Errorf("lock row path = %q, want the ciphertext path %q", stored, want)
	}

	got, err := locks.GetByPath(ctx, env.uid, "/docs/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/docs/f.txt" {
		t.Errorf("get echo = %q, want plaintext", got.Path)
	}
	byToken, err := locks.GetByToken(ctx, l.Token)
	if err != nil {
		t.Fatal(err)
	}
	if byToken.Path != stored {
		t.Errorf("token lookup path = %q, want the ciphertext row (nothing decrypts it back)", byToken.Path)
	}

	// Lock-null: the target file does not exist; the leaf token needs only
	// the parent's key.
	null := &FileLock{UserID: env.uid, Path: "/docs/never-created.txt", Token: "opaquelocktoken:bbbb", Owner: "alice", TimeoutMs: 100, CreatedMs: 1}
	if err := locks.Insert(ctx, null); err != nil {
		t.Fatalf("lock-null insert: %v", err)
	}

	// Rename translates both endpoints; the row follows the new ciphertext.
	if err := locks.RenamePath(ctx, env.uid, "/docs/f.txt", "/docs/g.txt"); err != nil {
		t.Fatal(err)
	}
	got, err = locks.GetByPath(ctx, env.uid, "/docs/g.txt")
	if err != nil || got.ID != l.ID {
		t.Fatalf("renamed lock = %+v %v", got, err)
	}
	if _, err := locks.GetByPath(ctx, env.uid, "/docs/f.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old lock path = %v, want ErrNotFound", err)
	}
	if err := locks.DeleteByPath(ctx, env.uid, "/docs"); err != nil {
		t.Fatal(err)
	}
	if _, err := locks.GetByPath(ctx, env.uid, "/docs/g.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete = %v, want ErrNotFound", err)
	}
}

// TestTranslatingVersionStore pins ciphertext file_path at rest with
// plaintext arguments at the boundary; returned rows keep ciphertext paths
// (only the numeric IDs are consumed).
func TestTranslatingVersionStore(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkdir(t, "/docs", 0x40)
	versions := NewTranslatingVersionStore(NewSQLVersionStore(env.db), env.tr)

	v := &FileVersion{UserID: env.uid, Path: "/docs/f.txt", Revision: "100", Size: 5}
	if err := versions.Insert(ctx, v); err != nil {
		t.Fatal(err)
	}
	if v.Path != "/docs/f.txt" {
		t.Errorf("insert caller view = %q, want plaintext", v.Path)
	}
	var stored string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions WHERE id = ?`, v.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if want := env.cipher(t, "/docs/f.txt"); stored != want {
		t.Errorf("version row path = %q, want %q", stored, want)
	}

	got, err := versions.Get(ctx, env.uid, "/docs/f.txt", "100")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != stored {
		t.Errorf("returned row path = %q, want ciphertext (IDs only consumed)", got.Path)
	}
	list, err := versions.ListByPath(ctx, env.uid, "/docs/f.txt")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v %v", list, err)
	}

	if err := versions.RenamePath(ctx, env.uid, "/docs/f.txt", "/docs/g.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := versions.Get(ctx, env.uid, "/docs/g.txt", "100"); err != nil {
		t.Errorf("renamed version lookup: %v", err)
	}
	if _, err := versions.Get(ctx, env.uid, "/docs/f.txt", "100"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old version path = %v, want ErrNotFound", err)
	}
	deleted, err := versions.DeleteByPath(ctx, env.uid, "/docs")
	if err != nil || len(deleted) != 1 {
		t.Fatalf("delete by path = %v %v", deleted, err)
	}
}

// TestTranslatingTrashStore pins ciphertext original_path/name at rest,
// decryption on the way out, the missing-ancestor degradation, and the loud
// ErrIntegrity on tamper.
func TestTranslatingTrashStore(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkdir(t, "/docs", 0x40)
	trash := NewTranslatingTrashStore(NewSQLTrashStore(env.db), env.tr)

	item := &TrashItem{UserID: env.uid, OriginalPath: "/docs/f.txt", LocationID: "f.txt.d100", Size: 1}
	if err := trash.Insert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if item.OriginalPath != "/docs/f.txt" || item.Name != "f.txt" {
		t.Errorf("insert caller view = %q %q, want plaintext", item.OriginalPath, item.Name)
	}
	var storedPath, storedName string
	if err := env.db.QueryRow(ctx, `SELECT original_path, name FROM trash_items WHERE id = ?`, item.ID).Scan(&storedPath, &storedName); err != nil {
		t.Fatal(err)
	}
	if want := env.cipher(t, "/docs/f.txt"); storedPath != want {
		t.Errorf("trash row path = %q, want the ciphertext path %q", storedPath, want)
	}
	if storedName != path.Base(storedPath) {
		t.Errorf("trash name = %q, want the last ciphertext segment of %q", storedName, storedPath)
	}

	got, err := trash.GetByLocation(ctx, env.uid, "f.txt.d100")
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginalPath != "/docs/f.txt" || got.Name != "f.txt" {
		t.Errorf("decrypted = %q %q, want plaintext", got.OriginalPath, got.Name)
	}

	// The missing-ancestor corner: trash a second item, then delete /docs
	// (the FK cascade removes its rows). Both items can no longer resolve
	// mid-path rows and degrade to their ciphertext fields — listing stays
	// available, restore to an explicit destination still works, and nothing
	// renders as fabricated plaintext.
	second := &TrashItem{UserID: env.uid, OriginalPath: "/docs/g.txt", LocationID: "g.txt.d101", Size: 1}
	if err := trash.Insert(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := env.raw.DeleteSubtree(ctx, env.uid, env.cipher(t, "/docs")); err != nil {
		t.Fatal(err)
	}
	degraded, err := trash.GetByLocation(ctx, env.uid, "g.txt.d101")
	if err != nil {
		t.Fatalf("degraded get: %v", err)
	}
	if degraded.OriginalPath == "/docs/g.txt" {
		t.Errorf("degraded path = %q, want the ciphertext left standing", degraded.OriginalPath)
	}
	items, err := trash.List(ctx, env.uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("list with degraded items = %v", items)
	}

	// Tamper is loud: a root-level token that fails authentication is
	// ErrIntegrity (the root row always resolves).
	solo := &TrashItem{UserID: env.uid, OriginalPath: "/solo.txt", LocationID: "solo.txt.d102", Size: 1}
	if err := trash.Insert(ctx, solo); err != nil {
		t.Fatal(err)
	}
	var soloPath string
	if err := env.db.QueryRow(ctx, `SELECT original_path FROM trash_items WHERE id = ?`, solo.ID).Scan(&soloPath); err != nil {
		t.Fatal(err)
	}
	bad := soloPath
	if bad[1] == '_' {
		bad = "/-" + bad[2:]
	} else {
		bad = "/_" + bad[2:]
	}
	if _, err := env.db.Exec(ctx, `UPDATE trash_items SET original_path = ? WHERE id = ?`, bad, solo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := trash.GetByLocation(ctx, env.uid, "solo.txt.d102"); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered trash path = %v, want ErrIntegrity", err)
	}
}
