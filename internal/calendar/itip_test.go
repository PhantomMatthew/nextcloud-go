package calendar

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const inviteICS = "BEGIN:VCALENDAR\nVERSION:2.0\nPRODID:-//nextcloud-go//EN\nBEGIN:VEVENT\nUID:ncgo-meet-001\nDTSTAMP:20250501T120000Z\nDTSTART:20250506T140000Z\nDTEND:20250506T150000Z\nSUMMARY:Phase 3e4\nORGANIZER;CN=Alice:mailto:alice@example.com\nATTENDEE;PARTSTAT=NEEDS-ACTION;CN=Bob:mailto:bob@example.com\nATTENDEE;CN=Ext:mailto:ext@other.example\nEND:VEVENT\nEND:VCALENDAR\n"

func TestParseScheduling(t *testing.T) {
	info := parseScheduling([]byte(inviteICS))
	if info == nil {
		t.Fatal("info = nil")
	}
	if info.Organizer != "alice@example.com" {
		t.Fatalf("organizer = %q", info.Organizer)
	}
	if len(info.Attendees) != 2 {
		t.Fatalf("attendees = %+v", info.Attendees)
	}
	if info.Attendees[0].Email != "bob@example.com" || info.Attendees[0].Partstat != "NEEDS-ACTION" {
		t.Fatalf("attendee0 = %+v", info.Attendees[0])
	}
	if got := parseScheduling([]byte(sampleICS)); got != nil {
		t.Fatalf("no organizer = %+v", got)
	}
}

func TestUpdateAttendeePartstat(t *testing.T) {
	out := string(updateAttendeePartstat([]byte(inviteICS), "bob@example.com", "ACCEPTED"))
	if !strings.Contains(out, "ATTENDEE;PARTSTAT=ACCEPTED;CN=Bob:mailto:bob@example.com") {
		t.Fatalf("out:\n%s", out)
	}
	if strings.Contains(out, "PARTSTAT=NEEDS-ACTION;CN=Bob") {
		t.Fatalf("old partstat kept:\n%s", out)
	}

	insert := string(updateAttendeePartstat([]byte("BEGIN:VEVENT\nATTENDEE:mailto:x@y.z\nEND:VEVENT"), "x@y.z", "DECLINED"))
	if !strings.Contains(insert, "ATTENDEE;PARTSTAT=DECLINED:mailto:x@y.z") {
		t.Fatalf("insert:\n%s", insert)
	}

	crlf := string(updateAttendeePartstat([]byte("BEGIN:VEVENT\r\nATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:x@y.z\r\nEND:VEVENT\r\n"), "x@y.z", "ACCEPTED"))
	if !strings.Contains(crlf, "ATTENDEE;PARTSTAT=ACCEPTED:mailto:x@y.z\r\n") {
		t.Fatalf("crlf:\n%q", crlf)
	}

	orig := []byte("BEGIN:VEVENT\nATTENDEE:mailto:a@b.c\nEND:VEVENT")
	if got := updateAttendeePartstat(orig, "nobody@d.e", "ACCEPTED"); !bytes.Equal(got, orig) {
		t.Fatalf("no-match changed data: %s", got)
	}
}

func schedulingDAV(t *testing.T) (context.Context, *DAV) {
	t.Helper()
	ctx := context.Background()
	db := testDB(t)
	us := users.NewSQLStore(db)
	for _, u := range []*users.User{
		{UID: "alice", DisplayName: "Alice", Email: "alice@example.com", PasswordHash: "x", Enabled: true},
		{UID: "bob", DisplayName: "Bob", Email: "bob@example.com", PasswordHash: "x", Enabled: true},
		{UID: "carol", DisplayName: "Carol", PasswordHash: "x", Enabled: true},
	} {
		if err := us.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	store := NewSQLStore(db)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	store.Clock = func() time.Time { return freeze }
	return ctx, &DAV{Store: store, Users: us, Clock: func() time.Time { return freeze }}
}

func TestSchedulingFlow(t *testing.T) {
	ctx, dav := schedulingDAV(t)

	// REQUEST: alice invites bob (and an external address, skipped).
	if _, _, err := dav.Write(ctx, "alice", "/personal/ncgo-meet-001.ics", strings.NewReader(inviteICS), nil); err != nil {
		t.Fatal(err)
	}
	rc, ent, err := dav.Read(ctx, "bob", "/personal/ncgo-meet-001.ics")
	if err != nil {
		t.Fatalf("bob invite copy: %v", err)
	}
	_ = rc.Close()
	if ent.ETag == "" {
		t.Fatalf("bob entry = %+v", ent)
	}

	// REPLY: bob accepts; his own copy stores and alice's copy updates.
	replyICS := strings.Replace(inviteICS, "PARTSTAT=NEEDS-ACTION;CN=Bob", "PARTSTAT=ACCEPTED;CN=Bob", 1)
	if _, _, err := dav.Write(ctx, "bob", "/personal/ncgo-meet-001.ics", strings.NewReader(replyICS), nil); err != nil {
		t.Fatal(err)
	}
	rc, _, err = dav.Read(ctx, "alice", "/personal/ncgo-meet-001.ics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	body := string(raw)
	if !strings.Contains(body, "PARTSTAT=ACCEPTED;CN=Bob") {
		t.Fatalf("organizer copy:\n%s", body)
	}

	// CANCEL: alice deletes; bob's copy disappears.
	if err := dav.Remove(ctx, "alice", "/personal/ncgo-meet-001.ics"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Read(ctx, "bob", "/personal/ncgo-meet-001.ics"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("bob copy after cancel: %v", err)
	}

	// Writer without email: no scheduling, plain store.
	if _, _, err := dav.Write(ctx, "carol", "/personal/ncgo-meet-001.ics", strings.NewReader(inviteICS), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := dav.Read(ctx, "bob", "/personal/ncgo-meet-001.ics"); !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("no delivery expected for email-less writer: %v", err)
	}
}
