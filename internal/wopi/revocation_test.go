package wopi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// TestTokenRevocationDisabledUser pins the ADR-0106 revocation hardening:
// disabling the token's account revokes every outstanding token on the very
// next callback (one undifferentiated 401, no oracle).
func TestTokenRevocationDisabledUser(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	if _, _, err := env.dav.Write(ctx, "alice", "/doc.odt", strings.NewReader("v1"), nil); err != nil {
		t.Fatal(err)
	}
	id := env.fileID(t, "/doc.odt")
	m := env.mintToken(t, "alice", id)

	get := func() *httptest.ResponseRecorder {
		return env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
			filesURL(id, false)+"?access_token="+m.Token, nil))
	}
	if rr := get(); rr.Code != http.StatusOK {
		t.Fatalf("pre-disable CheckFileInfo = %d", rr.Code)
	}
	if err := env.us.SetEnabled(ctx, "alice", false); err != nil {
		t.Fatal(err)
	}
	if rr := get(); rr.Code != http.StatusUnauthorized {
		t.Fatalf("disabled-user CheckFileInfo = %d, want 401", rr.Code)
	}
	if err := env.us.SetEnabled(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	if rr := get(); rr.Code != http.StatusOK {
		t.Fatalf("re-enabled CheckFileInfo = %d", rr.Code)
	}
}

// TestTokenRevocationShareGoneLive pins that share changes take effect on
// the NEXT callback — the token's baked grant is ANDed with a per-request
// re-resolution (ADR-0106): deleting the share turns callbacks into 404,
// downgrading it to read-only turns writes into 403, both without waiting
// for the token's TTL.
func TestTokenRevocationShareGoneLive(t *testing.T) {
	env := newEnv(t)
	ctx := t.Context()
	testShareeSetup(t, env)
	id := env.fileID(t, "/shared/report.odt")

	// Upgrade testShareeSetup's row to read+write and mint: can_write true.
	shares, err := env.shares.Store.ListBySharee(ctx, "bob", nil)
	if err != nil {
		t.Fatal(err)
	}
	var share *files.Share
	for i := range shares {
		if shares[i].Path == "/shared" {
			share = &shares[i]
		}
	}
	if share == nil {
		t.Fatal("read-only share row not found")
	}
	share.Permissions = webdav.PermRead | webdav.PermUpdate | webdav.PermCreate
	if err := env.shares.Store.Update(ctx, share); err != nil {
		t.Fatal(err)
	}
	m := env.mintToken(t, "bob", id)
	if !m.CanWrite {
		t.Fatal("bob can_write = false, want true under the write share")
	}
	put := func() *httptest.ResponseRecorder {
		return env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodPost,
			filesURL(id, true)+"?access_token="+m.Token, strings.NewReader("x")))
	}
	if rr := put(); rr.Code != http.StatusOK {
		t.Fatalf("pre-revoke PutFile = %d", rr.Code)
	}

	// Downgrade the share to read-only: the next write is 403 even though
	// the token was minted with can_write=true.
	share.Permissions = webdav.PermRead
	if err := env.shares.Store.Update(ctx, share); err != nil {
		t.Fatal(err)
	}
	if rr := put(); rr.Code != http.StatusForbidden {
		t.Fatalf("downgraded-share PutFile = %d, want 403", rr.Code)
	}
	if rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, true)+"?access_token="+m.Token, nil)); rr.Code != http.StatusOK {
		t.Fatalf("downgraded-share GetFile = %d, want 200 (read still granted)", rr.Code)
	}

	// Revoke the share entirely: every callback is 404 (no existence
	// oracle), token TTL notwithstanding.
	if err := env.shares.Store.Delete(ctx, share.ID); err != nil {
		t.Fatal(err)
	}
	if rr := env.serveCallback(httptest.NewRequestWithContext(ctx, http.MethodGet,
		filesURL(id, true)+"?access_token="+m.Token, nil)); rr.Code != http.StatusNotFound {
		t.Fatalf("revoked-share GetFile = %d, want 404", rr.Code)
	}
}
