package files

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
)

// TestShareSealOpenRoundTrip pins SealShareMeta/OpenShareMeta for a directory
// target (ADR-0104 phase 3a): the ciphertext path is the tree's cipherPath,
// mount_name_enc is the DETERMINISTIC NCGOFN1 token of the basename under
// the target's OWN key (pinning it byte-exact — the parent-DK copy would be
// a different token), and abs_path_enc opens back to the plaintext path.
func TestShareSealOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	dir := env.mkdir(t, "/Photos", 0x10)
	uuid, dk := fixedKey(0x10)

	ctPath, mountEnc, absEnc, err := env.tr.SealShareMeta(ctx, env.uid, "/Photos")
	if err != nil {
		t.Fatal(err)
	}
	if want := env.cipher(t, "/Photos"); ctPath != want {
		t.Errorf("ctPath = %q, want cipherPath %q", ctPath, want)
	}
	// The mount token is keyed to the target's own key — NOT the parent-keyed
	// tree token (which is what files.name carries).
	if want := tokenOf(t, dk, uuid, "Photos"); mountEnc != want {
		t.Errorf("mount_name_enc = %q, want the target-keyed token %q", mountEnc, want)
	}
	if mountEnc == env.rawRow(t, dir.ID).Name {
		t.Error("mount_name_enc equals the parent-keyed tree token — wrong key")
	}
	if strings.Contains(absEnc, "Photos") {
		t.Errorf("abs_path_enc leaks plaintext: %q", absEnc)
	}

	abs, mount, err := env.tr.OpenShareMeta(ctx, env.uid, &Share{
		OwnerUserID: env.uid, Path: ctPath, MountNameEnc: mountEnc, AbsPathEnc: absEnc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if abs != "/Photos" || mount != "Photos" {
		t.Errorf("open = %q %q, want /Photos Photos", abs, mount)
	}
}

// TestShareSealFileTarget pins that a FILE target seals under its file key
// playing the directory-key role (ADR-0104 §7).
func TestShareSealFileTarget(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	f := env.mkfile(t, "/report.xlsx")
	uuid, dk := fixedKey(0x77)
	env.claimDK(t, f, uuid, dk)

	ctPath, mountEnc, absEnc, err := env.tr.SealShareMeta(ctx, env.uid, "/report.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	if want := tokenOf(t, dk, uuid, "report.xlsx"); mountEnc != want {
		t.Errorf("mount_name_enc = %q, want the file-keyed token %q", mountEnc, want)
	}
	abs, mount, err := env.tr.OpenShareMeta(ctx, env.uid, &Share{
		OwnerUserID: env.uid, Path: ctPath, MountNameEnc: mountEnc, AbsPathEnc: absEnc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if abs != "/report.xlsx" || mount != "report.xlsx" {
		t.Errorf("open = %q %q", abs, mount)
	}
}

// TestShareSealScheme0Passthrough pins the scheme-0 contract: plaintext path,
// empty enc fields, zero resolver calls — the bit-identical carve-out.
func TestShareSealScheme0Passthrough(t *testing.T) {
	env := newNameCoreEnv(t, 0)
	ctPath, mountEnc, absEnc, err := env.tr.SealShareMeta(context.Background(), env.uid, "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if ctPath != "/docs" || mountEnc != "" || absEnc != "" {
		t.Errorf("scheme-0 seal = %q %q %q, want plaintext passthrough", ctPath, mountEnc, absEnc)
	}
	if env.res.calls != 0 {
		t.Errorf("resolver calls = %d, want 0", env.res.calls)
	}
}

// TestShareOpenPlaintextPassthrough pins the legacy/plaintext read path: a
// share row without sealed fields returns (Path, basename) untouched.
func TestShareOpenPlaintextPassthrough(t *testing.T) {
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	abs, mount, err := env.tr.OpenShareMeta(context.Background(), env.uid, &Share{Path: "/docs/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if abs != "/docs/sub" || mount != "sub" {
		t.Errorf("passthrough = %q %q", abs, mount)
	}
}

// TestShareOpenTamper pins that corrupted enc fields surface ErrIntegrity —
// never a plaintext fallback (ADR-0104 §2's tamper stance, extended to §7).
func TestShareOpenTamper(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	env.mkdir(t, "/Photos", 0x10)
	ctPath, mountEnc, absEnc, err := env.tr.SealShareMeta(ctx, env.uid, "/Photos")
	if err != nil {
		t.Fatal(err)
	}
	sh := &Share{OwnerUserID: env.uid, Path: ctPath, MountNameEnc: mountEnc, AbsPathEnc: absEnc}
	if _, _, err := env.tr.OpenShareMeta(ctx, env.uid, sh); err != nil {
		t.Fatalf("baseline open: %v", err)
	}

	flip := func(s string) string {
		return s[:len(s)-2] + "AA"
	}
	if _, _, err := env.tr.OpenShareMeta(ctx, env.uid, &Share{
		OwnerUserID: env.uid, Path: ctPath, MountNameEnc: flip(mountEnc), AbsPathEnc: absEnc,
	}); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered mount_name_enc err = %v, want ErrIntegrity", err)
	}
	if _, _, err := env.tr.OpenShareMeta(ctx, env.uid, &Share{
		OwnerUserID: env.uid, Path: ctPath, MountNameEnc: mountEnc, AbsPathEnc: flip(absEnc),
	}); !errors.Is(err, encrypt.ErrIntegrity) {
		t.Errorf("tampered abs_path_enc err = %v, want ErrIntegrity", err)
	}
	// A share row pointing at a deleted target is ErrNotFound, loudly.
	if _, _, err := env.tr.OpenShareMeta(ctx, env.uid, &Share{
		OwnerUserID: env.uid, Path: "/rqZrAhgnaEN35P61yNTBQ3UuflDzpi5AUWAqpjg", MountNameEnc: mountEnc, AbsPathEnc: absEnc,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing target err = %v, want ErrNotFound", err)
	}
}

// TestCipherPathUnderNeverLeavesAnchor pins the phase-3a anchoring: the walk
// resolves ONLY in-subtree keys — with the root's key struck from the
// resolver (a sharee has no wrap for it), CipherPathUnder and
// DecryptUnderAnchor still work while the root-anchored cipherPath fails.
func TestCipherPathUnderNeverLeavesAnchor(t *testing.T) {
	ctx := context.Background()
	env := newNameCoreEnv(t, encrypt.NameSchemeNCGOFN1)
	a := env.mkdir(t, "/a", 0x10)
	env.mkdir(t, "/a/b", 0x20)
	env.mkfile(t, "/a/b/f.txt")

	anchor := env.rawRow(t, a.ID)
	ctA := anchor.Path
	// Expected values computed while every key is still resolvable.
	wantDeep := env.cipher(t, "/a/b/f.txt")
	wantNew := env.cipher(t, "/a/new.txt")
	outside := env.rawRow(t, env.mkdir(t, "/elsewhere", 0x30).ID)

	// The sharee holds wraps for /a and below — never the root.
	rootUUID := vectorParentUUID()
	delete(env.res.keys, rootUUID)

	deep, err := env.tr.CipherPathUnder(ctx, anchor, ctA, "/b/f.txt")
	if err != nil {
		t.Fatalf("CipherPathUnder: %v", err)
	}
	if deep != wantDeep {
		t.Errorf("CipherPathUnder = %q, want %q", deep, wantDeep)
	}
	// The leaf need not exist (creates through the mount).
	newFile, err := env.tr.CipherPathUnder(ctx, anchor, ctA, "/new.txt")
	if err != nil {
		t.Fatalf("CipherPathUnder new leaf: %v", err)
	}
	if newFile != wantNew {
		t.Errorf("CipherPathUnder new leaf = %q, want %q", newFile, wantNew)
	}
	// The anchor itself maps to its own ciphertext path.
	self, err := env.tr.CipherPathUnder(ctx, anchor, ctA, "/")
	if err != nil || self != ctA {
		t.Errorf("anchor self = %q %v, want %q", self, err, ctA)
	}
	// …while the root-anchored walk now fails: the pin that the anchored seam
	// is what lifts the sharee.
	if _, err := env.tr.cipherPath(ctx, newTranslateCache(), env.uid, "/a/b/f.txt"); err == nil {
		t.Error("cipherPath resolved without the root key — the test premise broke")
	}

	// DecryptUnderAnchor rewrites rows into the mount view, again without the
	// root key: the anchor maps to the mount root, children decrypt their
	// name chain relative to the anchor.
	rawUnder := func(p string) File {
		cp := env.cipherWithRootKey(t, rootUUID, p)
		f, err := env.raw.GetByPath(ctx, env.uid, cp)
		if err != nil {
			t.Fatal(err)
		}
		return *f
	}
	dec, err := env.tr.DecryptUnderAnchor(ctx, anchor, ctA, "/shared",
		[]File{*anchor, rawUnder("/a/b"), rawUnder("/a/b/f.txt")})
	if err != nil {
		t.Fatalf("DecryptUnderAnchor: %v", err)
	}
	want := []struct{ name, path string }{
		{"shared", "/shared"},
		{"b", "/shared/b"},
		{"f.txt", "/shared/b/f.txt"},
	}
	if len(dec) != len(want) {
		t.Fatalf("decrypted %d rows, want %d", len(dec), len(want))
	}
	for i, w := range want {
		if dec[i].Name != w.name || dec[i].Path != w.path {
			t.Errorf("row %d = %q %q, want %q %q", i, dec[i].Name, dec[i].Path, w.name, w.path)
		}
	}

	// A row outside the anchor is rejected, loudly.
	if _, err := env.tr.DecryptUnderAnchor(ctx, anchor, ctA, "/shared", []File{*outside}); err == nil {
		t.Error("row outside the anchor decrypted — want a loud error")
	}
}

// cipherWithRootKey re-adds the root key for one lookup (test scaffolding
// for DecryptUnderAnchor fixtures).
func (e *nameCoreEnv) cipherWithRootKey(t *testing.T, rootUUID [16]byte, p string) string {
	t.Helper()
	e.res.keys[rootUUID] = vectorDK()
	defer delete(e.res.keys, rootUUID)
	return e.cipher(t, p)
}
