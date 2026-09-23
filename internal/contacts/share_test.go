package contacts

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

func shareTestDAV(t *testing.T) (context.Context, *DAV, *users.User, *users.User) {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	us := users.NewSQLStore(db)
	alice := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	bob := &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	carol := &users.User{UID: "carol", DisplayName: "Carol", PasswordHash: "x", Enabled: true}
	for _, u := range []*users.User{alice, bob, carol} {
		if err := us.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	dav := &DAV{Store: store, Users: us, Clock: func() time.Time { return freeze }}
	if _, err := dav.Stat(ctx, "alice", "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Stat(ctx, "bob", "/"); err != nil {
		t.Fatal(err)
	}
	return ctx, dav, alice, bob
}

func TestAddressbookShareStore(t *testing.T) {
	ctx, dav, alice, bob := shareTestDAV(t)
	book, err := dav.Store.GetBookByURI(ctx, alice.ID, DefaultBookURI)
	if err != nil {
		t.Fatal(err)
	}
	if err := dav.Store.UpsertAddressbookShare(ctx, book.ID, bob.ID, ShareAccessRead); err != nil {
		t.Fatal(err)
	}
	shared, err := dav.Store.ListSharedAddressbooks(ctx, bob.ID)
	if err != nil || len(shared) != 1 {
		t.Fatalf("shared = %v %v", shared, err)
	}
	if shared[0].OwnerUID != "alice" || shared[0].Access != ShareAccessRead || shared[0].URI != DefaultBookURI {
		t.Fatalf("shared = %+v", shared[0])
	}
	if err := dav.Store.UpsertAddressbookShare(ctx, book.ID, bob.ID, ShareAccessReadWrite); err != nil {
		t.Fatal(err)
	}
	sb, err := dav.Store.GetSharedAddressbook(ctx, bob.ID, DefaultBookURI)
	if err != nil || sb.Access != ShareAccessReadWrite {
		t.Fatalf("sb = %+v %v", sb, err)
	}
	if err := dav.Store.DeleteAddressbookShare(ctx, book.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Store.GetSharedAddressbook(ctx, bob.ID, DefaultBookURI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	if err := dav.Store.UpsertAddressbookShare(ctx, book.ID, bob.ID, "bogus"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bogus access err = %v", err)
	}
}

func TestAddressbookSharingDAV(t *testing.T) {
	ctx, dav, alice, _ := shareTestDAV(t)
	if _, _, err := dav.Write(ctx, "alice", "/contacts/ncgo-contact-001.vcf", bytes.NewReader([]byte(sampleVCard)), nil); err != nil {
		t.Fatal(err)
	}
	shareBody := []byte(`<?xml version="1.0"?>` +
		`<cs:share xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/">` +
		`<cs:set><d:href>/remote.php/dav/principals/users/bob/</d:href><cs:read-write/></cs:set>` +
		`<cs:set><d:href>/remote.php/dav/principals/users/carol/</d:href><cs:read/></cs:set>` +
		`</cs:share>`)
	if err := dav.Share(ctx, "alice", "/contacts", shareBody); err != nil {
		t.Fatal(err)
	}

	kids, err := dav.List(ctx, "bob", "/")
	if err != nil || len(kids) != 2 {
		t.Fatalf("bob home = %v %v", kids, err)
	}
	var sharedEntry *webdav.Entry
	for _, k := range kids {
		if k.Shared {
			sharedEntry = k
		}
	}
	if sharedEntry == nil {
		t.Fatalf("no shared entry in %v", kids)
	}
	if !sharedEntry.IsAddressbook || sharedEntry.ShareAccess != ShareAccessReadWrite || sharedEntry.OwnerPrincipal != "/remote.php/dav/principals/users/alice/" {
		t.Fatalf("shared entry = %+v", sharedEntry)
	}

	rc, _, err := dav.Read(ctx, "bob", "/contacts_shared_by_alice/ncgo-contact-001.vcf")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	const bobVCard = "BEGIN:VCARD\nVERSION:3.0\nUID:bob-001\nFN:Bob Smith\nN:Smith;Bob;;;\nEND:VCARD\n"
	if _, _, err := dav.Write(ctx, "bob", "/contacts_shared_by_alice/bob-001.vcf", bytes.NewReader([]byte(bobVCard)), nil); err != nil {
		t.Fatalf("bob write read-write share: %v", err)
	}
	// The card lands in the OWNER's book.
	ownerObjs, err := dav.Store.ListObjects(ctx, alice.ID, DefaultBookURI)
	if err != nil || len(ownerObjs) != 2 {
		t.Fatalf("owner objects = %v %v", ownerObjs, err)
	}
	if _, _, err := dav.Write(ctx, "carol", "/contacts_shared_by_alice/carol-001.vcf", bytes.NewReader([]byte(bobVCard)), nil); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("carol write read-only share: %v", err)
	}
	if err := dav.Remove(ctx, "bob", "/contacts_shared_by_alice"); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("bob remove addressbook: %v", err)
	}
	if err := dav.Remove(ctx, "carol", "/contacts_shared_by_alice/ncgo-contact-001.vcf"); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("carol delete object: %v", err)
	}

	entries, err := dav.Report(ctx, "bob", "/contacts_shared_by_alice", webdav.ReportRequest{
		Name: "addressbook-query",
		Body: []byte(`<c:addressbook-query xmlns:c="urn:ietf:params:xml:ns:carddav"/>`),
	})
	if err != nil || len(entries) != 2 {
		t.Fatalf("bob report shared = %v %v", entries, err)
	}
	if err := dav.Share(ctx, "bob", "/contacts_shared_by_alice", shareBody); !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("sharee re-share: %v", err)
	}

	if err := dav.Share(ctx, "alice", "/contacts", []byte(`<cs:share xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><cs:remove><d:href>/remote.php/dav/principals/users/carol/</d:href></cs:remove></cs:share>`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Read(ctx, "carol", "/contacts_shared_by_alice/ncgo-contact-001.vcf"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("carol after unshare: %v", err)
	}

	if err := dav.Share(ctx, "alice", "/contacts", []byte(`<cs:share xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><cs:set><d:href>/remote.php/dav/principals/users/nobody/</d:href></cs:set></cs:share>`)); !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("unknown target: %v", err)
	}
	if err := dav.Share(ctx, "alice", "/contacts", []byte(`<cs:share xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><cs:set><d:href>/remote.php/dav/principals/users/alice/</d:href></cs:set></cs:share>`)); !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("self share: %v", err)
	}
	if err := dav.Share(ctx, "alice", "/contacts", []byte(`<cs:share/>`)); !errors.Is(err, webdav.ErrBadRequest) {
		t.Fatalf("empty share: %v", err)
	}
}

func TestPrincipalUID(t *testing.T) {
	uid, err := principalUID("/remote.php/dav/principals/users/bob/")
	if err != nil || uid != "bob" {
		t.Fatalf("uid = %q %v", uid, err)
	}
	for _, bad := range []string{"/remote.php/dav/principals/groups/admins/", "/principals/users/", ""} {
		if _, err := principalUID(bad); err == nil {
			t.Fatalf("%q should fail", bad)
		}
	}
}
