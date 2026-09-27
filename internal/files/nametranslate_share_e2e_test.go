package files_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/sharing"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// shareRow reads the raw shares row — the phase-3a at-rest view.
func (e *keyShareEnv) shareRow(t *testing.T, id int64) (path, mountEnc, absEnc string) {
	t.Helper()
	if err := e.db.QueryRow(context.Background(),
		`SELECT file_path, mount_name_enc, abs_path_enc FROM shares WHERE id = ?`, id).
		Scan(&path, &mountEnc, &absEnc); err != nil {
		t.Fatal(err)
	}
	return path, mountEnc, absEnc
}

// openShare opens a raw share row through the translation core.
func (e *nameE2EEnv) openShare(t *testing.T, ctx context.Context, ownerID, id int64) (abs, mount string) {
	t.Helper()
	p, mEnc, aEnc := e.shareRow(t, id)
	abs, mount, err := e.xlate.OpenShareMeta(ctx, ownerID, &files.Share{
		OwnerUserID: ownerID, Path: p, MountNameEnc: mEnc, AbsPathEnc: aEnc,
	})
	if err != nil {
		t.Fatalf("open share %d: %v", id, err)
	}
	return abs, mount
}

// TestNameCryptShareCreateSealsRow pins the phase-3a grant-time row shape: a
// scheme-1 owner's share rows carry the ciphertext file_path and the sealed
// metadata copies, with no plaintext path material in any column; a scheme-0
// owner's row is bit-identical to the pre-feature form.
func TestNameCryptShareCreateSealsRow(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.write(t, "/docs/report.xlsx", "sheet")

	dirShare := env.share(t, "/docs", files.ShareTypeUser, "bob")
	fileShare := env.share(t, "/docs/report.xlsx", files.ShareTypeUser, "bob")

	for _, tc := range []struct {
		name    string
		id      int64
		plain   string
		ctWant  string
		baseEnc string
	}{
		{"folder", dirShare.ID, "/docs", env.rawRow(t, "/docs").Path, "docs"},
		{"file", fileShare.ID, "/docs/report.xlsx", env.rawRow(t, "/docs/report.xlsx").Path, "report.xlsx"},
	} {
		p, mEnc, aEnc := env.shareRow(t, tc.id)
		if p != tc.ctWant {
			t.Errorf("%s: file_path = %q, want the ciphertext %q", tc.name, p, tc.ctWant)
		}
		if mEnc == "" || aEnc == "" {
			t.Errorf("%s: sealed metadata empty (mount_name_enc=%q abs_path_enc=%q)", tc.name, mEnc, aEnc)
		}
		for _, leak := range []string{"docs", "report", "xlsx"} {
			if strings.Contains(p, leak) || strings.Contains(mEnc, leak) || strings.Contains(aEnc, leak) {
				t.Errorf("%s: share row leaks %q: %q %q %q", tc.name, leak, p, mEnc, aEnc)
			}
		}
		abs, mount := env.openShare(t, ctx, env.ids["alice"], tc.id)
		if abs != tc.plain || mount != tc.baseEnc {
			t.Errorf("%s: opened = %q %q, want %q %q", tc.name, abs, mount, tc.plain, tc.baseEnc)
		}
	}

	// Scheme 0: the row is bit-identical to the pre-phase-3a form (carol
	// stays scheme 0 with the codec wired — SealShareMeta passes through).
	env0 := newNameE2EEnv(t, "carol", "dave")
	if _, err := env0.dav.Mkdir(ctx, "carol", "/plain"); err != nil {
		t.Fatal(err)
	}
	sh0, err := env0.svc.Create(ctx, "carol", "/plain", files.ShareTypeUser, 0, "dave", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p0, m0, a0 := env0.shareRow(t, sh0.ID)
	if p0 != "/plain" || m0 != "" || a0 != "" {
		t.Errorf("scheme-0 row = %q %q %q, want (/plain, empty, empty)", p0, m0, a0)
	}

	// The file share mounts and reads through the ciphertext branch.
	env.dav.Incoming = files.MultiIncoming{env.svc}
	rc, _, err := env.dav.Read(ctx, "bob", "/report.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "sheet" {
		t.Errorf("file-share mount read = %q %v", body, err)
	}
}

// TestNameCryptListIncomingDecrypts pins the sharee-facing mount table: the
// Mount carries the decrypted basename, OwnerPath the plaintext owner path
// (storage-key derivation), OwnerCipherPath the ciphertext anchor.
func TestNameCryptListIncomingDecrypts(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/shared dir")
	env.write(t, "/shared dir/f.txt", "x")
	env.share(t, "/shared dir", files.ShareTypeUser, "bob")

	mounts, err := env.svc.ListIncoming(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v, want 1", mounts)
	}
	m := mounts[0]
	if m.Mount != "/shared dir" {
		t.Errorf("Mount = %q, want the decrypted /shared dir", m.Mount)
	}
	if m.OwnerPath != "/shared dir" {
		t.Errorf("OwnerPath = %q, want the plaintext owner path", m.OwnerPath)
	}
	ctWant := env.rawRow(t, "/shared dir").Path
	if m.OwnerCipherPath != ctWant {
		t.Errorf("OwnerCipherPath = %q, want the ciphertext anchor %q", m.OwnerCipherPath, ctWant)
	}
	// Plaintext-scheme owners (no scheme flip) keep today's shape verbatim.
	env0 := newNameE2EEnv(t, "carol", "dave")
	if _, err := env0.dav.Mkdir(ctx, "carol", "/p"); err != nil {
		t.Fatal(err)
	}
	if _, err := env0.svc.Create(ctx, "carol", "/p", files.ShareTypeUser, 0, "dave", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	m0, err := env0.svc.ListIncoming(ctx, "dave")
	if err != nil || len(m0) != 1 {
		t.Fatalf("scheme-0 mounts = %+v %v", m0, err)
	}
	if m0[0].Mount != "/p" || m0[0].OwnerPath != "/p" || m0[0].OwnerCipherPath != "" {
		t.Errorf("scheme-0 mount = %+v, want the plaintext verbatim form", m0[0])
	}
}

// TestNameCryptEnrolledShareeLifted is the phase-3a centerpiece: an enrolled
// owner shares a folder to an enrolled sharee, and the sharee — with an
// unlocked session — gets full DAV operability through the ciphertext mount
// (phase 2's documented 403 residual, lifted by share-root anchoring):
// PROPFIND with plaintext names on the wire, GET, PUT (both wrap rows land),
// and the owner's rename re-seals the share metadata so the mount follows.
func TestNameCryptEnrolledShareeLifted(t *testing.T) {
	env := newPWEnv(t, "alice", "bob")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	e2e.encryptUser(t, "bob")
	aliceKey := env.login(t, "alice", "alice-pw")
	bobKey := env.login(t, "bob", "bob-pw")
	actx := pctx("alice", aliceKey)
	bctx := pctx("bob", bobKey)

	// Alice builds and shares her tree with her unlocked session. The share
	// grants bob read+create+update so the write verbs work through the mount.
	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.xlsx", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	sh, err := env.svc.Create(actx, "alice", "/secret", files.ShareTypeUser,
		webdav.PermRead|webdav.PermUpdate|webdav.PermCreate, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var shPath string
	if err := env.db.QueryRow(context.Background(), `SELECT file_path FROM shares WHERE id = ?`, sh.ID).Scan(&shPath); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(shPath, "secret") {
		t.Fatalf("share row path leaks plaintext: %q", shPath)
	}
	// The sharee's DAV view comes from the REAL ListIncoming (ciphertext
	// mount, opened in bob's ctx).
	env.dav.Incoming = files.MultiIncoming{env.svc}

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, target string, body io.Reader, p *auth.Principal) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(context.Background(), method, target, body)
		if method == "PROPFIND" {
			req.Header.Set("Depth", "1")
		}
		req = req.WithContext(auth.WithUser(req.Context(), p))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	bobUnlocked := &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: bobKey}

	// PROPFIND the mount: 207 with plaintext names, no token leakage.
	rr := do("PROPFIND", "/remote.php/dav/files/bob/secret", nil, bobUnlocked)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("sharee mount PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "plans.xlsx") {
		t.Errorf("mount PROPFIND missing the plaintext name:\n%s", rr.Body.String())
	}
	if row := e2e.rawRowAs(t, actx, "/secret/plans.xlsx"); strings.Contains(rr.Body.String(), row.Name) {
		t.Error("mount PROPFIND leaks the name token")
	}

	// GET through the mount.
	rr = do(http.MethodGet, "/remote.php/dav/files/bob/secret/plans.xlsx", nil, bobUnlocked)
	if rr.Code != http.StatusOK || rr.Body.String() != "plans" {
		t.Fatalf("sharee GET = %d %q", rr.Code, rr.Body.String())
	}

	// PUT a new file through the mount (MKCOL's lock check included: the
	// webdav handler runs CheckLock before PUT).
	rr = do(http.MethodPut, "/remote.php/dav/files/bob/secret/upload.txt", strings.NewReader("from bob"), bobUnlocked)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("sharee PUT = %d: %s", rr.Code, rr.Body.String())
	}
	// The row sits in alice's tree in ciphertext form, and both the owner's
	// and the writing sharee's wrap rows exist (SQL counts).
	newRow := e2e.rawRowAs(t, actx, "/secret/upload.txt")
	if strings.Contains(newRow.Name, "upload") || newRow.NameScheme != encrypt.NameSchemeNCGOFN1 {
		t.Errorf("mounted PUT row = %+v, want ciphertext scheme 1", newRow)
	}
	for uid, want := range map[string]int64{"alice": 1, "bob": 1} {
		var n int64
		if err := env.db.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`,
			newRow.KeyUUID, env.ids[uid]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s wrap rows on the mounted PUT key = %d, want %d", uid, n, want)
		}
	}
	// Both parties read the new file back.
	for _, tc := range []struct {
		ctx     context.Context
		user, p string
	}{
		{actx, "alice", "/secret/upload.txt"},
		{bctx, "bob", "/secret/upload.txt"},
	} {
		rc, _, err := env.dav.Read(tc.ctx, tc.user, tc.p)
		if err != nil {
			t.Fatalf("%s read %s: %v", tc.user, tc.p, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(body) != "from bob" {
			t.Fatalf("%s read = %q %v", tc.user, body, err)
		}
	}

	// MKCOL through the mount: the fresh folder's DK is minted for the owner
	// and wrapped for the writing sharee.
	rr = do("MKCOL", "/remote.php/dav/files/bob/secret/subdir", nil, bobUnlocked)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("sharee MKCOL = %d: %s", rr.Code, rr.Body.String())
	}
	subRow := e2e.rawRowAs(t, actx, "/secret/subdir")
	if got := env.wrapRowsForUUID(t, subRow.KeyUUID, "bob"); got != 1 {
		t.Errorf("mounted MKCOL DK: bob wrap rows = %d, want 1", got)
	}

	// The owner renames the share root: file_path prefix-rewrites and the
	// sealed metadata re-seals — the sharee's mount follows.
	if _, _, err := env.dav.Move(actx, "alice", "/secret", "alice", "/vault", false); err != nil {
		t.Fatal(err)
	}
	abs, mount := e2e.openShare(t, actx, env.ids["alice"], sh.ID)
	if abs != "/vault" || mount != "vault" {
		t.Fatalf("re-sealed share meta = %q %q, want /vault vault", abs, mount)
	}
	rr = do("PROPFIND", "/remote.php/dav/files/bob/vault", nil, bobUnlocked)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("post-rename mount PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"plans.xlsx", "upload.txt", "subdir"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("post-rename PROPFIND missing %s", want)
		}
	}
	rr = do(http.MethodGet, "/remote.php/dav/files/bob/vault/plans.xlsx", nil, bobUnlocked)
	if rr.Code != http.StatusOK || rr.Body.String() != "plans" {
		t.Fatalf("post-rename GET = %d %q", rr.Code, rr.Body.String())
	}
}

// TestNameCryptEnrolledShareeNoSessionLocked is the 403 variant pinned
// against the REAL ListIncoming: an enrolled sharee without an unlocked
// session cannot open the sealed mount metadata — ErrKeyLocked → 403 (the
// ADR-0101 boundary holds at the mount).
func TestNameCryptEnrolledShareeNoSessionLocked(t *testing.T) {
	env := newPWEnv(t, "alice", "bob")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	e2e.encryptUser(t, "bob")
	aliceKey := env.login(t, "alice", "alice-pw")
	env.login(t, "bob", "bob-pw")
	actx := pctx("alice", aliceKey)

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.xlsx", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Create(actx, "alice", "/secret", files.ShareTypeUser, 0, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	env.dav.Incoming = files.MultiIncoming{env.svc}

	// Keyless bob: opening the mount metadata needs his box wrap's private
	// key — ErrKeyLocked, never a plaintext fallback.
	_, err := env.svc.ListIncoming(pctx("bob", nil), "bob")
	if !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Fatalf("keyless ListIncoming = %v, want ErrKeyLocked", err)
	}

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), "PROPFIND", "/remote.php/dav/files/bob/secret", nil)
	req.Header.Set("Depth", "1")
	req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodAppPassword}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("keyless sharee PROPFIND = %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// TestNameCryptShareRenameReseals pins the rename re-seal matrix (ADR-0104
// phase 3a): renaming the share ROOT refreshes mount_name_enc and
// abs_path_enc; renaming an ANCESTOR refreshes abs_path_enc while the mount
// basename is unchanged; a share below the renamed point follows; unrelated
// shares stay untouched.
func TestNameCryptShareRenameReseals(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob", "carol", "dave")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.mkdir(t, "/docs/sub")
	env.write(t, "/docs/sub/f.txt", "x")
	env.mkdir(t, "/other")
	env.write(t, "/other/g.txt", "y")

	rootShare := env.share(t, "/docs", files.ShareTypeUser, "bob")
	belowShare := env.share(t, "/docs/sub", files.ShareTypeUser, "carol")
	otherShare := env.share(t, "/other", files.ShareTypeUser, "dave")
	otherAbsBefore, otherMountBefore := env.openShare(t, ctx, env.ids["alice"], otherShare.ID)

	// Rename the share root (and with it the ancestor of the below-share).
	if _, _, err := env.dav.Move(ctx, "alice", "/docs", "alice", "/newdocs", false); err != nil {
		t.Fatal(err)
	}

	abs, mount := env.openShare(t, ctx, env.ids["alice"], rootShare.ID)
	if abs != "/newdocs" || mount != "newdocs" {
		t.Errorf("root share after rename = %q %q, want /newdocs newdocs", abs, mount)
	}
	abs, mount = env.openShare(t, ctx, env.ids["alice"], belowShare.ID)
	if abs != "/newdocs/sub" || mount != "sub" {
		t.Errorf("below share after ancestor rename = %q %q, want /newdocs/sub sub", abs, mount)
	}
	// The ciphertext file_path prefix was rewritten too (exact matching still
	// works against the live tree).
	p, _, _ := env.shareRow(t, rootShare.ID)
	if want := env.rawRow(t, "/newdocs").Path; p != want {
		t.Errorf("root share file_path = %q, want %q", p, want)
	}
	p, _, _ = env.shareRow(t, belowShare.ID)
	if want := env.rawRow(t, "/newdocs/sub").Path; p != want {
		t.Errorf("below share file_path = %q, want %q", p, want)
	}
	// Unrelated shares are untouched.
	abs, mount = env.openShare(t, ctx, env.ids["alice"], otherShare.ID)
	if abs != otherAbsBefore || mount != otherMountBefore {
		t.Errorf("unrelated share changed: %q %q", abs, mount)
	}

	// Renaming just the subfolder refreshes its own share's mount name.
	if _, _, err := env.dav.Move(ctx, "alice", "/newdocs/sub", "alice", "/newdocs/renamed", false); err != nil {
		t.Fatal(err)
	}
	abs, mount = env.openShare(t, ctx, env.ids["alice"], belowShare.ID)
	if abs != "/newdocs/renamed" || mount != "renamed" {
		t.Errorf("below share after own rename = %q %q, want /newdocs/renamed renamed", abs, mount)
	}
	// The parent share's metadata is stable (its own root did not move).
	abs, mount = env.openShare(t, ctx, env.ids["alice"], rootShare.ID)
	if abs != "/newdocs" || mount != "newdocs" {
		t.Errorf("root share after child rename = %q %q, want unchanged /newdocs newdocs", abs, mount)
	}

	// Deleting a shared subtree deletes its share rows — the DeleteByPath
	// call sites match ciphertext paths now (phase-3a translation pin).
	if err := env.dav.Remove(ctx, "alice", "/newdocs"); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := env.db.QueryRow(ctx, `SELECT COUNT(*) FROM shares WHERE id IN (?, ?)`, rootShare.ID, belowShare.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("share rows after subtree delete = %d, want 0", n)
	}
	abs, mount = env.openShare(t, ctx, env.ids["alice"], otherShare.ID)
	if abs != "/other" {
		t.Errorf("unrelated share after delete = %q %q, want /other", abs, mount)
	}
}

// TestNameCryptOCSOwnerViewsPlaintext pins the owner-facing OCS surface: the
// list payload shows the PLAINTEXT path (shareMap opens the sealed row), and
// the ?path= filter matches against the plaintext form.
func TestNameCryptOCSOwnerViewsPlaintext(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")
	env.mkdir(t, "/docs")
	env.share(t, "/docs", files.ShareTypeUser, "bob")

	h := sharing.Handler{Service: env.svc, Version: ocs.V2}
	get := func(target string) map[string]any {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		req = req.WithContext(auth.WithUser(req.Context(), &auth.Principal{UID: "alice", Enabled: true}))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("OCS %s = %d: %s", target, rr.Code, rr.Body.String())
		}
		var envv map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &envv); err != nil {
			t.Fatal(err)
		}
		return envv
	}

	body := get("/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json")
	data := body["ocs"].(map[string]any)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("OCS list data = %v", data)
	}
	if got := data[0].(map[string]any)["path"]; got != "/docs" {
		t.Errorf("OCS owner list path = %v, want plaintext /docs", got)
	}

	// The plaintext path filter matches the ciphertext row.
	body = get("/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json&path=/docs")
	data = body["ocs"].(map[string]any)["data"].([]any)
	if len(data) != 1 {
		t.Errorf("OCS path-filtered list = %v, want the one share", data)
	}
	body = get("/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json&path=/nope")
	if data := body["ocs"].(map[string]any)["data"].([]any); len(data) != 0 {
		t.Errorf("OCS missing-path filter = %v, want empty", data)
	}
}

// TestNameCryptPublicLinkMasterWrapped pins the public-link boundary for a
// MASTER-wrapped scheme-1 owner: the anonymous ctx resolves through the
// owner's rows, names decrypt, content flows (ADR-0104 §7).
func TestNameCryptPublicLinkMasterWrapped(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice")
	env.encryptUser(t, "alice")
	env.write(t, "/report.xlsx", "sheet bytes")

	sh := env.share(t, "/report.xlsx", files.ShareTypeLink, "")
	p, mEnc, aEnc := env.shareRow(t, sh.ID)
	if mEnc == "" || aEnc == "" || strings.Contains(p, "report") {
		t.Fatalf("link share row = %q %q %q, want sealed ciphertext", p, mEnc, aEnc)
	}

	pub := &files.PublicDAV{Files: env.dav, Resolve: env.svc.LookupValid, NameCodec: env.xlate}
	// Anonymous ctx (no principal — a link download carries no session).
	rc, ent, err := pub.Read(ctx, sh.Token, "/")
	if err != nil {
		t.Fatalf("master-wrapped public read: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "sheet bytes" {
		t.Fatalf("public read = %q %v", body, err)
	}
	if ent.Size != int64(len("sheet bytes")) {
		t.Errorf("public entry size = %d", ent.Size)
	}

	// The direct /s/{token} download handler resolves the same way.
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/s/"+sh.Token, nil)
	env.svc.PublicLinkHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "sheet bytes" {
		t.Errorf("direct link handler = %d %q", rr.Code, rr.Body.String())
	}
}

// TestNameCryptPublicLinkEnrolledLocked pins the other side of the §7
// boundary: an enrolled owner's public link with no unlocked session is
// ErrKeyLocked → 403 on both the DAV jail and the direct download handler —
// the same boundary as content (ADR-0101).
func TestNameCryptPublicLinkEnrolledLocked(t *testing.T) {
	ctx := context.Background()
	env := newPWEnv(t, "alice")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	aliceKey := env.login(t, "alice", "alice-pw")
	actx := pctx("alice", aliceKey)

	if _, _, err := env.dav.Write(actx, "alice", "/secret.txt", strings.NewReader("locked"), nil); err != nil {
		t.Fatal(err)
	}
	sh, err := env.svc.Create(actx, "alice", "/secret.txt", files.ShareTypeLink, 0, "", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, aEnc := env.shareRow(t, sh.ID); aEnc == "" {
		t.Fatal("enrolled link share row carries no sealed metadata")
	}

	pub := &files.PublicDAV{Files: env.dav, Resolve: env.svc.LookupValid, NameCodec: e2e.xlate}
	_, _, err = pub.Read(ctx, sh.Token, "/")
	if !errors.Is(err, webdav.ErrForbidden) || !errors.Is(err, encrypt.ErrKeyLocked) {
		t.Fatalf("enrolled public read = %v, want the dual-matched 403/ErrKeyLocked", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/s/"+sh.Token, nil)
	env.svc.PublicLinkHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("direct link handler = %d, want 403: %s", rr.Code, rr.Body.String())
	}
}

// TestNameCryptMountedWriteVersionSnapshot pins the overwrite path through a
// ciphertext mount: the sharee's overwrite snapshots the old content into
// file_versions keyed by the CIPHERTEXT owner path, and the recipient wraps
// carry across the key change (ADR-0098/0101 semantics preserved).
func TestNameCryptMountedWriteVersionSnapshot(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice")

	env.mkdir(t, "/docs")
	env.write(t, "/docs/f.txt", "v1")
	// Explicit update+create rights (the Create default is read-only).
	if _, err := env.svc.Create(ctx, "alice", "/docs", files.ShareTypeUser, webdav.PermRead|webdav.PermUpdate|webdav.PermCreate, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	env.dav.Incoming = files.MultiIncoming{env.svc}

	if _, _, err := env.dav.Write(ctx, "bob", "/docs/f.txt", strings.NewReader("v2"), nil); err != nil {
		t.Fatal(err)
	}
	// The version row carries the ciphertext path of the owner tree.
	var vPath string
	if err := env.db.QueryRow(ctx, `SELECT file_path FROM file_versions`).Scan(&vPath); err != nil {
		t.Fatal(err)
	}
	if want := env.rawRow(t, "/docs/f.txt").Path; vPath != want {
		t.Errorf("version row path = %q, want the ciphertext %q", vPath, want)
	}
	// Recipient wraps carried to the fresh key.
	newUUID := env.rawRow(t, "/docs/f.txt").KeyUUID
	if got := env.wrapRowsForUUID(t, newUUID, "bob"); got != 1 {
		t.Errorf("bob wrap rows on the overwritten key = %d, want 1", got)
	}
	// The owner reads the current content and the version back.
	rc, _, err := env.dav.Read(ctx, "alice", "/docs/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "v2" {
		t.Fatalf("owner read = %q %v", body, err)
	}
	st, err := env.dav.Stat(ctx, "alice", "/docs/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	vers, err := env.dav.Versions.List(ctx, "alice", "/versions/"+itoa(st.NumericID))
	if err != nil || len(vers) != 1 {
		t.Fatalf("owner versions = %v %v", vers, err)
	}
	vrc, _, err := env.dav.Versions.Read(ctx, "alice", "/versions/"+itoa(st.NumericID)+vers[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	vbody, err := io.ReadAll(vrc)
	_ = vrc.Close()
	if err != nil || string(vbody) != "v1" {
		t.Errorf("version content = %q %v", vbody, err)
	}
}

// itoa formats the numeric file id for the versions DAV path.
func itoa(id uint64) string {
	return strconv.FormatUint(id, 10)
}
