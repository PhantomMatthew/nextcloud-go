package sharing

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// insertShare inserts one share row directly (Create would fire the OCM
// notify path for remote shares, which these tests do not exercise).
func insertShare(t *testing.T, svc *Service, sh *files.Share) {
	t.Helper()
	alice, err := svc.Users.GetByUID(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	sh.OwnerUserID = alice.ID
	if err := svc.Store.Insert(t.Context(), sh); err != nil {
		t.Fatal(err)
	}
}

func TestLookupValidPublicDAVRemoteShare(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeRemote, Path: "/pub", ItemType: "folder",
		Token: "ocmtoken00000001", Permissions: webdav.PermRead,
		ShareWith: "bob@https://remote.example.com",
	})

	// The public-link resolvers stay link-only: an OCM token must NOT open
	// the /s/{token} page or the legacy lookup.
	if _, _, err := svc.LookupValid(ctx, "ocmtoken00000001"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("LookupValid(remote) = %v, want ErrNotFound", err)
	}
	if _, _, err := svc.ResolvePublic(ctx, "ocmtoken00000001", ""); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("ResolvePublic(remote) = %v, want ErrNotFound", err)
	}

	// The public-DAV resolvers accept it, with no password (Create clears
	// PasswordHash for non-link shares).
	sh, owner, err := svc.LookupValidPublicDAV(ctx, "ocmtoken00000001")
	if err != nil {
		t.Fatal(err)
	}
	if sh.ShareType != files.ShareTypeRemote || owner.UID != "alice" {
		t.Fatalf("resolved = %+v owner %q", sh, owner.UID)
	}
	if _, _, err := svc.ResolvePublicDAV(ctx, "ocmtoken00000001", ""); err != nil {
		t.Fatalf("ResolvePublicDAV(remote, \"\") = %v", err)
	}

	// A user share never resolves through any public resolver.
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeUser, Path: "/a.txt", ItemType: "file",
		Token: "usershare0000001", Permissions: webdav.PermRead, ShareWith: "bob",
	})
	if _, _, err := svc.LookupValidPublicDAV(ctx, "usershare0000001"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("LookupValidPublicDAV(user share) = %v, want ErrNotFound", err)
	}

	// An expired remote share is gone for the public DAV too.
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeRemote, Path: "/a.txt", ItemType: "file",
		Token: "ocmtoken00000002", Permissions: webdav.PermRead,
		ShareWith: "bob@https://remote.example.com", ExpireMs: 1,
	})
	if _, _, err := svc.LookupValidPublicDAV(ctx, "ocmtoken00000002"); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("LookupValidPublicDAV(expired) = %v, want ErrNotFound", err)
	}

	// Link shares keep resolving through the widened resolver (unchanged).
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeLink, Path: "/a.txt", ItemType: "file",
		Token: "linktoken00000001", Permissions: webdav.PermRead,
	})
	if _, _, err := svc.LookupValidPublicDAV(ctx, "linktoken00000001"); err != nil {
		t.Fatalf("LookupValidPublicDAV(link) = %v", err)
	}
}

func TestTokenVerifierRemoteToken(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeRemote, Path: "/pub", ItemType: "folder",
		Token: "ocmtoken00000003", Permissions: webdav.PermRead,
		ShareWith: "bob@https://remote.example.com",
	})
	v := &TokenVerifier{Service: svc}
	p, err := v.Verify(ctx, "ocmtoken00000003", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "ocmtoken00000003" || !p.Enabled || p.AuthMethod != auth.AuthMethodBasic {
		t.Fatalf("principal = %+v", p)
	}
	if _, err := v.Verify(ctx, "nosuchtoken0000000", ""); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("Verify(unknown) = %v, want ErrInvalidCredentials", err)
	}
}

// TestPublicDAVRemoteShareRoundTrip is the provider-side counterpart of the
// 3d3/3d6 client proxy: a remote server holding the OCM token reads (and,
// with the grant's write permissions, writes) share content through
// /public.php/webdav — the exact flow that returned 401 before this resolver.
func TestPublicDAVRemoteShareRoundTrip(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	dav := svc.Files
	if _, _, err := dav.Write(ctx, "alice", "/pub/hello.txt", strings.NewReader("hi"), nil); err != nil {
		t.Fatal(err)
	}
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeRemote, Path: "/pub", ItemType: "folder",
		Token: "ocmtoken00000004", Permissions: webdav.PermRead,
		ShareWith: "bob@https://remote.example.com",
	})
	insertShare(t, svc, &files.Share{
		ShareType: files.ShareTypeRemote, Path: "/pub", ItemType: "folder",
		Token:       "ocmtoken00000005",
		Permissions: webdav.PermRead | webdav.PermCreate | webdav.PermUpdate | webdav.PermDelete,
		ShareWith:   "carol@https://remote.example.com",
	})
	pub := &files.PublicDAV{Files: dav, Resolve: svc.LookupValidPublicDAV}

	// Read-only remote token: content flows, writes are gated.
	rc, ent, err := pub.Read(ctx, "ocmtoken00000004", "/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hi" || ent.Path != "/hello.txt" {
		t.Fatalf("read = %q %+v", b, ent)
	}
	if _, _, err := pub.Write(ctx, "ocmtoken00000004", "/deny.txt", strings.NewReader("x"), nil); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("read-only write = %v, want ErrForbidden", err)
	}

	// Write-enabled remote token: the 3d6 write-back's provider counterpart.
	if _, _, err := pub.Write(ctx, "ocmtoken00000005", "/new.txt", strings.NewReader("n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Mkdir(ctx, "ocmtoken00000005", "/sub"); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Stat(ctx, "alice", "/pub/new.txt"); err != nil {
		t.Fatalf("owner stat after remote write = %v", err)
	}
	if err := pub.Remove(ctx, "ocmtoken00000005", "/new.txt"); err != nil {
		t.Fatal(err)
	}
}
