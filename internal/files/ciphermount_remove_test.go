package files_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// cipherMountRig is the enrolled-owner ciphertext-mount DELETE rig: alice
// (enrolled owner) and bob (enrolled sharee) over the REAL ListIncoming,
// served by a real webdav handler (mirrors TestNameCryptEnrolledShareeLifted).
type cipherMountRig struct {
	env    *pwEnv
	e2e    *nameE2EEnv
	h      *webdav.Handler
	actx   context.Context
	bobKey []byte
}

func newCipherMountRig(t *testing.T) *cipherMountRig {
	t.Helper()
	env := newPWEnv(t, "alice", "bob")
	e2e := upgradeNameCrypt(t, &env.keyShareEnv, env.res)
	e2e.encryptUser(t, "alice")
	e2e.encryptUser(t, "bob")
	aliceKey := env.login(t, "alice", "alice-pw")
	bobKey := env.login(t, "bob", "bob-pw")
	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	return &cipherMountRig{env: env, e2e: e2e, h: h, actx: pctx("alice", aliceKey), bobKey: bobKey}
}

// shareSecret shares alice's /secret to bob with the given permissions and
// points DAV's incoming feed at the real sharing service.
func (r *cipherMountRig) shareSecret(t *testing.T, perms int) {
	t.Helper()
	if _, err := r.env.svc.Create(r.actx, "alice", "/secret", files.ShareTypeUser,
		perms, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	r.env.dav.Incoming = files.MultiIncoming{r.env.svc}
}

// do serves one request through the handler as the given principal.
func (r *cipherMountRig) do(t *testing.T, method, target string, p *auth.Principal) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	if method == "PROPFIND" {
		req.Header.Set("Depth", "1")
	}
	req = req.WithContext(auth.WithUser(req.Context(), p))
	rr := httptest.NewRecorder()
	r.h.ServeHTTP(rr, req)
	return rr
}

func (r *cipherMountRig) bobUnlocked() *auth.Principal {
	return &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodSession, UnlockedKey: r.bobKey}
}

// TestNameCryptCipherMountDeleteTrashes pins the ADR-0104 phase-3a DELETE
// residual fix: an enrolled sharee's DELETE inside a ciphertext mount lands
// the item in the OWNER's trashbin — the trashed row's ciphertext path
// derives from the share-root anchor alone, so no owner ancestor walk runs
// in the sharee's ctx. At rest the trash row carries ciphertext verbatim;
// the owner lists and restores it in their own ctx.
func TestNameCryptCipherMountDeleteTrashes(t *testing.T) {
	rig := newCipherMountRig(t)
	env, e2e, actx := rig.env, rig.e2e, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.txt", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.dav.Mkdir(actx, "alice", "/secret/subdir"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/subdir/inner.txt", strings.NewReader("inner"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead|webdav.PermDelete)

	plansRow := e2e.rawRowAs(t, actx, "/secret/plans.txt")

	rr := rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret/plans.txt", rig.bobUnlocked())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("sharee DELETE = %d, want 204: %s", rr.Code, rr.Body.String())
	}

	// The owner-facing trash view, translated in alice's own ctx.
	items, err := env.dav.Trash.Sessions.List(actx, env.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("owner trash items = %+v, want exactly 1", items)
	}
	if items[0].OriginalPath != "/secret/plans.txt" || items[0].Name != "plans.txt" || items[0].DeletedBy != "bob" {
		t.Errorf("owner trash item = %+v, want original /secret/plans.txt, name plans.txt, deleted-by bob", items[0])
	}

	// The at-rest row carries ciphertext verbatim, anchored at the share
	// root; the location id embeds the capped leaf token + ".d<unix>".
	var rawOrig, rawName, loc string
	if err := env.db.QueryRow(context.Background(),
		`SELECT original_path, name, location_id FROM trash_items WHERE user_id = ?`, env.ids["alice"]).
		Scan(&rawOrig, &rawName, &loc); err != nil {
		t.Fatal(err)
	}
	if rawOrig != plansRow.Path {
		t.Errorf("raw original_path = %q, want the ciphertext %q", rawOrig, plansRow.Path)
	}
	if rawName != plansRow.Name {
		t.Errorf("raw name = %q, want the leaf token %q", rawName, plansRow.Name)
	}
	if !strings.HasPrefix(loc, plansRow.Name+".d") {
		t.Errorf("location_id = %q, want %q + \".d<unix>\"", loc, plansRow.Name)
	}

	// The delete keeps both parties' wrap rows on the file key.
	for uid, want := range map[string]int64{"alice": 1, "bob": 1} {
		var n int64
		if err := env.db.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM file_keys WHERE key_uuid = ? AND user_id = ?`,
			plansRow.KeyUUID, env.ids[uid]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s wrap rows on the trashed file key = %d, want %d", uid, n, want)
		}
	}

	// The mount no longer lists the trashed file.
	rr = rig.do(t, "PROPFIND", "/remote.php/dav/files/bob/secret", rig.bobUnlocked())
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("post-delete mount PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "plans.txt") {
		t.Error("post-delete mount PROPFIND still lists plans.txt")
	}

	// The bytes sit at the token-based trash object key.
	if _, err := env.dav.Storage.Stat(context.Background(), "trash/alice/"+loc); err != nil {
		t.Errorf("trash storage object trash/alice/%s: %v", loc, err)
	}

	// The owner restores in their own ctx and reads the content back.
	got, _, err := env.dav.Trash.Restore(actx, "alice", loc, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/secret/plans.txt" {
		t.Errorf("restored entry path = %q, want /secret/plans.txt", got.Path)
	}
	rc, _, err := env.dav.Read(actx, "alice", "/secret/plans.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "plans" {
		t.Fatalf("restored read = %q %v", body, err)
	}

	// A directory delete trashes the whole subtree the same way.
	rr = rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret/subdir", rig.bobUnlocked())
	if rr.Code != http.StatusNoContent {
		t.Fatalf("sharee DELETE dir = %d, want 204: %s", rr.Code, rr.Body.String())
	}
	items, err = env.dav.Trash.Sessions.List(actx, env.ids["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].OriginalPath != "/secret/subdir" {
		t.Fatalf("owner trash after dir delete = %+v, want exactly the /secret/subdir item", items)
	}
	got, _, err = env.dav.Trash.Restore(actx, "alice", items[0].LocationID, "alice", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/secret/subdir" {
		t.Errorf("restored dir entry path = %q, want /secret/subdir", got.Path)
	}
	rc, _, err = env.dav.Read(actx, "alice", "/secret/subdir/inner.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "inner" {
		t.Fatalf("restored dir read = %q %v", body, err)
	}
	// Known phase-2 gap (NOT asserted): after the owner's restore, bob cannot
	// resolve the restored item's names — ingestFromStorage does not re-wrap
	// the sharee's directory keys (dav.go restore path). Unrelated to this
	// increment.
}

// TestNameCryptCipherMountDeleteRootRefused pins the share-root boundary:
// DELETE on the mount itself is 403 (unsharing goes through OCS; the owner
// deletes through their own tree).
func TestNameCryptCipherMountDeleteRootRefused(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.txt", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead|webdav.PermDelete)

	rr := rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret", rig.bobUnlocked())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("sharee DELETE mount root = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	// The owner's tree is untouched.
	rc, _, err := env.dav.Read(actx, "alice", "/secret/plans.txt")
	if err != nil {
		t.Fatalf("owner read after refused delete: %v", err)
	}
	_ = rc.Close()
}

// TestNameCryptCipherMountDeleteNoPerm pins the permission boundary: a
// read-only ciphertext mount refuses DELETE with 403.
func TestNameCryptCipherMountDeleteNoPerm(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.txt", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead)

	rr := rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret/plans.txt", rig.bobUnlocked())
	if rr.Code != http.StatusForbidden {
		t.Fatalf("sharee DELETE without delete perm = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	rc, _, err := env.dav.Read(actx, "alice", "/secret/plans.txt")
	if err != nil {
		t.Fatalf("owner read after refused delete: %v", err)
	}
	_ = rc.Close()
}

// TestNameCryptCipherMountDeleteKeyless pins the ADR-0101 boundary at the
// mount: a sharee without an unlocked session cannot open the sealed mount
// metadata — ErrKeyLocked → 403, never a plaintext fallback.
func TestNameCryptCipherMountDeleteKeyless(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/plans.txt", strings.NewReader("plans"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead|webdav.PermDelete)

	bobKeyless := &auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodAppPassword}
	rr := rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret/plans.txt", bobKeyless)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("keyless sharee DELETE = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	rc, _, err := env.dav.Read(actx, "alice", "/secret/plans.txt")
	if err != nil {
		t.Fatalf("owner read after refused delete: %v", err)
	}
	_ = rc.Close()
}

// TestNameCryptCipherMountDeleteLocked pins the lock boundary: an owner-side
// lock on a file inside the mount surfaces as 423 through the sharee's
// DELETE (the handler's pre-DELETE CheckLock → checkLockCipherMount).
func TestNameCryptCipherMountDeleteLocked(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/locked.txt", strings.NewReader("locked"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead|webdav.PermDelete)

	if _, err := env.dav.Lock(actx, "alice", "/secret/locked.txt",
		webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second}); err != nil {
		t.Fatal(err)
	}
	rr := rig.do(t, "DELETE", "/remote.php/dav/files/bob/secret/locked.txt", rig.bobUnlocked())
	if rr.Code != http.StatusLocked {
		t.Fatalf("sharee DELETE locked file = %d, want 423: %s", rr.Code, rr.Body.String())
	}
	rc, _, err := env.dav.Read(actx, "alice", "/secret/locked.txt")
	if err != nil {
		t.Fatalf("owner read after refused delete: %v", err)
	}
	_ = rc.Close()
}
