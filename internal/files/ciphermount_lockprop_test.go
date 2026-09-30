package files_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TestNameCryptCipherMountLockIndicatorShown is the phase-3a lock-indicator
// residual fix: an owner-side lock on a file inside a ciphertext mount shows
// up in the enrolled sharee's PROPFIND/Stat — the indicator walk runs
// anchor-relative over the raw lock store (applyLockCipherMount) instead of
// the translating store's owner-chain walk, which an enrolled owner's sharee
// cannot resolve (the old silent degradation: display-only, always absent).
func TestNameCryptCipherMountLockIndicatorShown(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/locked.txt", strings.NewReader("locked"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead)

	info, err := env.dav.Lock(actx, "alice", "/secret/locked.txt",
		webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	bctx := pctx("bob", rig.bobKey)

	// Stat through the mount (statCipherShare): exact lock fields.
	ent, err := env.dav.Stat(bctx, "bob", "/secret/locked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.LockToken != info.Token || ent.LockOwner != "alice" || ent.LockTimeout <= 0 {
		t.Errorf("mount stat lock = (%q, %q, %v), want (%q, alice, >0)",
			ent.LockToken, ent.LockOwner, ent.LockTimeout, info.Token)
	}

	// The wire form, Depth:0 on the file itself.
	rr := rig.do(t, "PROPFIND", "/remote.php/dav/files/bob/secret/locked.txt", rig.bobUnlocked())
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("mount file PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), info.Token) {
		t.Errorf("mount file PROPFIND missing the lock token:\n%s", rr.Body.String())
	}

	// Depth:1 on the folder (listCipherShare): the child's entry carries it.
	rr = rig.do(t, "PROPFIND", "/remote.php/dav/files/bob/secret", rig.bobUnlocked())
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("mount list PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), info.Token) {
		t.Errorf("mount list PROPFIND missing the lock token:\n%s", rr.Body.String())
	}
}

// TestNameCryptCipherMountLockIndicatorInheritedFromRoot pins the walk's
// lower bound: a lock ON the share root is inside the sharee's view — the
// anchored indicator walk includes the mount-root level before stopping.
func TestNameCryptCipherMountLockIndicatorInheritedFromRoot(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/secret/child.txt", strings.NewReader("child"), nil); err != nil {
		t.Fatal(err)
	}
	rig.shareSecret(t, webdav.PermRead)

	info, err := env.dav.Lock(actx, "alice", "/secret",
		webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	ent, err := env.dav.Stat(pctx("bob", rig.bobKey), "bob", "/secret/child.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.LockToken != info.Token {
		t.Errorf("child stat lock token = %q, want the inherited root lock %q", ent.LockToken, info.Token)
	}
}

// TestNameCryptCipherMountLockIndicatorAboveRootHidden pins the boundary: a
// lock on an owner-side ANCESTOR ABOVE the share root stays invisible through
// the mount — the same boundary checkLockCipherMount enforces on writes (an
// ancestor lock neither blocks the sharee nor shows on the wire).
func TestNameCryptCipherMountLockIndicatorAboveRootHidden(t *testing.T) {
	rig := newCipherMountRig(t)
	env, actx := rig.env, rig.actx

	if _, err := env.dav.Mkdir(actx, "alice", "/parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.dav.Mkdir(actx, "alice", "/parent/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(actx, "alice", "/parent/secret/child.txt", strings.NewReader("child"), nil); err != nil {
		t.Fatal(err)
	}
	// The share targets the nested folder; bob's mount basename is "secret".
	if _, err := env.svc.Create(actx, "alice", "/parent/secret", files.ShareTypeUser,
		webdav.PermRead, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	env.dav.Incoming = files.MultiIncoming{env.svc}

	if _, err := env.dav.Lock(actx, "alice", "/parent",
		webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second}); err != nil {
		t.Fatal(err)
	}

	ent, err := env.dav.Stat(pctx("bob", rig.bobKey), "bob", "/secret/child.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.LockToken != "" {
		t.Errorf("above-root lock leaked into the mount: token %q", ent.LockToken)
	}
	rr := rig.do(t, "PROPFIND", "/remote.php/dav/files/bob/secret/child.txt", rig.bobUnlocked())
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("mount PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	// "opaquelocktoken:" is a fixed literal that only renders with an active
	// lock — safe as an absence assertion (no random-token substring trap).
	if strings.Contains(rr.Body.String(), "opaquelocktoken:") {
		t.Errorf("above-root lock rendered through the mount:\n%s", rr.Body.String())
	}
}

// TestNameCryptCipherMountLockIndicatorMasterOwnerSessionless pins parity for
// a master-wrapped (scheme-1, not password-enrolled) OWNER: name resolution
// needs no session on either side, the lock indicator already worked through
// the translating store's walk, and the anchored walk must keep it working
// sessionless — the regression guard for the enrolled-owner fix.
func TestNameCryptCipherMountLockIndicatorMasterOwnerSessionless(t *testing.T) {
	ctx := context.Background()
	env := newNameE2EEnv(t, "alice", "bob")
	env.encryptUser(t, "alice") // scheme 1 via master wrap; bob stays scheme 0

	if _, err := env.dav.Mkdir(ctx, "alice", "/secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.dav.Write(ctx, "alice", "/secret/locked.txt", strings.NewReader("locked"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Create(ctx, "alice", "/secret", files.ShareTypeUser,
		webdav.PermRead, "bob", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	env.dav.Incoming = files.MultiIncoming{env.svc}

	info, err := env.dav.Lock(ctx, "alice", "/secret/locked.txt",
		webdav.LockRequest{Owner: "alice", Timeout: 1800 * time.Second})
	if err != nil {
		t.Fatal(err)
	}

	// Sessionless on both sides: master-side resolution needs no principal.
	ent, err := env.dav.Stat(ctx, "bob", "/secret/locked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ent.LockToken != info.Token || ent.LockOwner != "alice" {
		t.Errorf("master owner mount stat lock = (%q, %q), want (%q, alice)",
			ent.LockToken, ent.LockOwner, info.Token)
	}

	h, err := webdav.NewHandler("/remote.php/dav/files/", env.dav, "oc123abc")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), "PROPFIND", "/remote.php/dav/files/bob/secret/locked.txt", nil)
	req.Header.Set("Depth", "0")
	req = req.WithContext(auth.WithUser(req.Context(),
		&auth.Principal{UID: "bob", Enabled: true, AuthMethod: auth.AuthMethodAppPassword}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusMultiStatus {
		t.Fatalf("master owner PROPFIND = %d, want 207: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), info.Token) {
		t.Errorf("master owner PROPFIND missing the lock token:\n%s", rr.Body.String())
	}
}
