package mail

import (
	"context"
	"errors"
	"testing"
)

// syncstore_test.go covers the M3 mailbox/message half of SQLStore against
// in-memory sqlite with the real migration chain (0028 included).

func TestSQLStoreListAll(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	for _, uid := range []string{"alice", "bob", "carol"} {
		if err := store.Create(ctx, testAccount(uid)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.ListAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].UserID != "alice" || all[1].UserID != "bob" || all[2].UserID != "carol" {
		t.Fatalf("ListAll = %+v", all)
	}
}

func TestSQLStoreMailboxUpsert(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}

	// Insert: defaults apply, the row reads back with an id.
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	if mb.ID <= 0 || mb.Delimiter != "/" || mb.SpecialUse != "" || mb.LastSeenUID != 0 {
		t.Fatalf("mailbox after insert = %+v", mb)
	}

	// Advance the cursor out of band, then upsert LIST-derived columns:
	// the cursors must survive the update.
	if err := store.UpdateMailboxSyncState(ctx, mb.ID, 777, 42, 41); err != nil {
		t.Fatal(err)
	}
	upd := &Mailbox{AccountID: a.ID, Name: "INBOX", Delimiter: ".", Selectable: false, SpecialUse: "sent"}
	if err := store.UpsertMailbox(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if upd.ID != mb.ID || upd.Delimiter != "." || upd.SpecialUse != "sent" || upd.Selectable ||
		upd.UIDValidity != 777 || upd.UIDNext != 42 || upd.LastSeenUID != 41 {
		t.Fatalf("mailbox after upsert = %+v", upd)
	}

	// A second mailbox lists in id order; invalid rows are rejected.
	if err := store.UpsertMailbox(ctx, &Mailbox{AccountID: a.ID, Name: "Sent", Selectable: true, SpecialUse: "sent"}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListMailboxes(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "INBOX" || list[1].Name != "Sent" {
		t.Fatalf("list = %+v", list)
	}
	if err := store.UpsertMailbox(ctx, &Mailbox{AccountID: a.ID}); !errors.Is(err, ErrInvalid) {
		t.Errorf("upsert without name: err = %v", err)
	}
	if err := store.UpsertMailbox(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil upsert: err = %v", err)
	}
}

func TestSQLStoreMessagesAndCounts(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}

	msgs := []Message{
		{MailboxID: mb.ID, UID: 1, MessageID: "m1", Subject: "one", FromAddr: "A <a@x>", ToAddrs: "b@y", DateUnix: 100, Flags: PackFlags([]string{"Seen"}), Size: 10},
		{MailboxID: mb.ID, UID: 2, MessageID: "m2", Subject: "two", FromAddr: "b@x", DateUnix: 200, Flags: PackFlags([]string{"Seen", "Flagged"}), Size: 20},
		{MailboxID: mb.ID, UID: 3, MessageID: "m3", Subject: "three", FromAddr: "c@x", DateUnix: 300, Flags: "" /* no flags: unread */, Size: 30},
		{MailboxID: mb.ID, UID: 4, MessageID: "m4", Subject: "four", FromAddr: "d@x", DateUnix: 400, Flags: PackFlags([]string{"Draft"}), Size: 40},
	}
	if err := store.InsertMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}

	uids, err := store.ListMessageUIDs(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 4 || uids[0] != 1 || uids[3] != 4 {
		t.Fatalf("uids = %v", uids)
	}
	recent, err := store.RecentMessageUIDs(ctx, mb.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0] != 4 || recent[1] != 3 {
		t.Fatalf("recent = %v", recent)
	}

	// Counts: 4 total, 2 without the Seen token.
	counts, err := store.ListMailboxCounts(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[0].Total != 4 || counts[0].Unread != 2 {
		t.Fatalf("counts = %+v", counts)
	}

	// Flag refresh: marking uid 3 seen drops unread to 1; a no-change write
	// keeps the row as-is (both are silent successes).
	if err := store.SetMessageFlags(ctx, mb.ID, 3, PackFlags([]string{"Seen"})); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMessageFlags(ctx, mb.ID, 3, PackFlags([]string{"Seen"})); err != nil {
		t.Fatal(err)
	}
	counts, err = store.ListMailboxCounts(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts[0].Unread != 1 {
		t.Fatalf("counts after flag update = %+v", counts)
	}

	// Vanished uids delete; the rest survive.
	if err := store.DeleteMessagesByUID(ctx, mb.ID, []int64{2, 4}); err != nil {
		t.Fatal(err)
	}
	uids, err = store.ListMessageUIDs(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 2 || uids[0] != 1 || uids[1] != 3 {
		t.Fatalf("uids after delete = %v", uids)
	}

	// UIDVALIDITY wipe empties the mailbox.
	if err := store.DeleteAllMessages(ctx, mb.ID); err != nil {
		t.Fatal(err)
	}
	uids, err = store.ListMessageUIDs(ctx, mb.ID)
	if err != nil || len(uids) != 0 {
		t.Fatalf("uids after wipe = %v %v", uids, err)
	}
}

func TestSQLStoreCursorUpdate(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMailboxSyncState(ctx, mb.ID, 12345, 99, 98); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListMailboxes(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].UIDValidity != 12345 || list[0].UIDNext != 99 || list[0].LastSeenUID != 98 {
		t.Fatalf("cursor = %+v", list[0])
	}
	if err := store.UpdateMailboxSyncState(ctx, 99999, 1, 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("cursor update on a missing mailbox: err = %v", err)
	}
}

func TestSQLStoreDeleteMailboxesCascade(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	keep := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	gone := &Mailbox{AccountID: a.ID, Name: "Old", Selectable: true}
	for _, mb := range []*Mailbox{keep, gone} {
		if err := store.UpsertMailbox(ctx, mb); err != nil {
			t.Fatal(err)
		}
		if err := store.InsertMessages(ctx, []Message{{MailboxID: mb.ID, UID: 1, Subject: "s", Flags: ""}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DeleteMailboxes(ctx, []int64{gone.ID}); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListMailboxes(ctx, a.ID)
	if err != nil || len(list) != 1 || list[0].Name != "INBOX" {
		t.Fatalf("list = %+v %v", list, err)
	}
	uids, err := store.ListMessageUIDs(ctx, gone.ID)
	if err != nil || len(uids) != 0 {
		t.Fatalf("gone mailbox still has messages: %v %v", uids, err)
	}
	uids, err = store.ListMessageUIDs(ctx, keep.ID)
	if err != nil || len(uids) != 1 {
		t.Fatalf("keep mailbox lost messages: %v %v", uids, err)
	}
	if err := store.DeleteMailboxes(ctx, nil); err != nil {
		t.Errorf("empty delete: %v", err)
	}
}

func TestSQLStoreAccountDeleteCascade(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	other := testAccount("bob")
	if err := store.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessages(ctx, []Message{{MailboxID: mb.ID, UID: 7, Subject: "s", Flags: ""}}); err != nil {
		t.Fatal(err)
	}
	ob := &Mailbox{AccountID: other.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, ob); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessages(ctx, []Message{{MailboxID: ob.ID, UID: 9, Subject: "s", Flags: ""}}); err != nil {
		t.Fatal(err)
	}

	if err := store.Delete(ctx, "alice", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByID(ctx, "alice", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("account after delete: err = %v", err)
	}
	mbs, err := store.ListMailboxes(ctx, a.ID)
	if err != nil || len(mbs) != 0 {
		t.Fatalf("mailboxes after cascade = %+v %v", mbs, err)
	}
	uids, err := store.ListMessageUIDs(ctx, mb.ID)
	if err != nil || len(uids) != 0 {
		t.Fatalf("messages after cascade = %v %v", uids, err)
	}
	// Bob's account, mailboxes, and messages are untouched.
	mbs, err = store.ListMailboxes(ctx, other.ID)
	if err != nil || len(mbs) != 1 {
		t.Fatalf("other user's mailboxes = %+v %v", mbs, err)
	}
	uids, err = store.ListMessageUIDs(ctx, ob.ID)
	if err != nil || len(uids) != 1 {
		t.Fatalf("other user's messages = %v %v", uids, err)
	}
}

func TestPackFlags(t *testing.T) {
	if got := PackFlags(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := PackFlags([]string{"Seen"}); got != " Seen " {
		t.Errorf("single = %q", got)
	}
	if got := PackFlags([]string{"Seen", "Flagged"}); got != " Seen Flagged " {
		t.Errorf("pair = %q", got)
	}
}

// TestSQLStoreListMessagesPages covers the M4 keyset pagination: newest
// first, the (date_unix, id) cursor walking same-date ties by id, and a
// walk to exhaustion across uneven pages.
func TestSQLStoreListMessagesPages(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	// Seven messages: uids 1..7, dates 100,300,300,300,200,100,50 — the
	// date-300 tie must walk by id descending.
	msgs := []Message{
		{MailboxID: mb.ID, UID: 1, Subject: "s1", DateUnix: 100},
		{MailboxID: mb.ID, UID: 2, Subject: "s2", DateUnix: 300},
		{MailboxID: mb.ID, UID: 3, Subject: "s3", DateUnix: 300},
		{MailboxID: mb.ID, UID: 4, Subject: "s4", DateUnix: 300},
		{MailboxID: mb.ID, UID: 5, Subject: "s5", DateUnix: 200},
		{MailboxID: mb.ID, UID: 6, Subject: "s6", DateUnix: 100},
		{MailboxID: mb.ID, UID: 7, Subject: "s7", DateUnix: 50},
	}
	if err := store.InsertMessages(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	// Insert order is uid order, so row ids ascend with uid; the wanted
	// newest-first order is (300,id4) (300,id3) (300,id2) (200,…) (100,…)
	// (100,…) (50,…).
	wantUIDOrder := []int64{4, 3, 2, 5, 6, 1, 7}

	var got []int64
	var beforeDate, beforeID int64
	pages := 0
	for {
		page, err := store.ListMessages(ctx, mb.ID, beforeDate, beforeID, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		pages++
		for i := range page {
			got = append(got, page[i].UID)
		}
		last := page[len(page)-1]
		beforeDate, beforeID = last.DateUnix, last.ID
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(got) != 7 {
		t.Fatalf("walk got uids %v", got)
	}
	for i := range wantUIDOrder {
		if got[i] != wantUIDOrder[i] {
			t.Fatalf("walk order = %v, want %v", got, wantUIDOrder)
		}
	}
	if pages != 3 { // 3 + 3 + 1
		t.Errorf("pages = %d, want 3", pages)
	}

	// A mid-list cursor lands exactly after the named row: (300, id of uid
	// 3) → the next page starts at uid 2.
	first, err := store.ListMessages(ctx, mb.ID, 0, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.ListMessages(ctx, mb.ID, first[1].DateUnix, first[1].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next[0].UID != 2 || next[1].UID != 5 {
		t.Errorf("page after cursor = %+v", next)
	}

	// Rows from another mailbox never leak in, and a non-positive limit
	// lists nothing.
	other := &Mailbox{AccountID: a.ID, Name: "Sent", Selectable: true}
	if err := store.UpsertMailbox(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessages(ctx, []Message{{MailboxID: other.ID, UID: 9, DateUnix: 999}}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(ctx, mb.ID, 0, 0, 50)
	if err != nil || len(page) != 7 {
		t.Errorf("page after other-mailbox insert = %d rows %v", len(page), err)
	}
	if page, err := store.ListMessages(ctx, mb.ID, 0, 0, 0); err != nil || len(page) != 0 {
		t.Errorf("limit 0 = %v %v", page, err)
	}
}

func TestSQLStoreGetMessageAndMailbox(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	other := testAccount("bob")
	if err := store.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true, SpecialUse: "trash"}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessages(ctx, []Message{
		{MailboxID: mb.ID, UID: 5, Subject: "keep", FromAddr: "a@x", DateUnix: 42, Flags: " Seen ", Size: 7},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetMailbox(ctx, a.ID, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "INBOX" || got.SpecialUse != "trash" || !got.Selectable {
		t.Errorf("mailbox = %+v", got)
	}
	// Scoped to the owning account: bob's id is a 404, as is a missing row.
	if _, err := store.GetMailbox(ctx, other.ID, mb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-account mailbox: err = %v", err)
	}
	if _, err := store.GetMailbox(ctx, a.ID, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing mailbox: err = %v", err)
	}

	page, err := store.ListMessages(ctx, mb.ID, 0, 0, 1)
	if err != nil || len(page) != 1 {
		t.Fatalf("list = %+v %v", page, err)
	}
	msg, err := store.GetMessage(ctx, mb.ID, page[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if msg.UID != 5 || msg.Subject != "keep" || msg.Flags != " Seen " || msg.Size != 7 || msg.DateUnix != 42 {
		t.Errorf("message = %+v", msg)
	}
	if _, err := store.GetMessage(ctx, 99999, page[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other-mailbox message: err = %v", err)
	}
	if _, err := store.GetMessage(ctx, mb.ID, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing message: err = %v", err)
	}
}

func TestSQLStoreDeleteMessage(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	a := testAccount("alice")
	if err := store.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	mb := &Mailbox{AccountID: a.ID, Name: "INBOX", Selectable: true}
	if err := store.UpsertMailbox(ctx, mb); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertMessages(ctx, []Message{
		{MailboxID: mb.ID, UID: 1, DateUnix: 1},
		{MailboxID: mb.ID, UID: 2, DateUnix: 2},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListMessages(ctx, mb.ID, 0, 0, 10)
	if err != nil || len(page) != 2 {
		t.Fatalf("list = %+v %v", page, err)
	}
	if err := store.DeleteMessage(ctx, mb.ID, page[0].ID); err != nil {
		t.Fatal(err)
	}
	uids, err := store.ListMessageUIDs(ctx, mb.ID)
	if err != nil || len(uids) != 1 {
		t.Fatalf("uids after delete = %v %v", uids, err)
	}
	// Deleting again (or under the wrong mailbox) is ErrNotFound.
	if err := store.DeleteMessage(ctx, mb.ID, page[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v", err)
	}
	if err := store.DeleteMessage(ctx, 99999, page[1].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("other-mailbox delete: err = %v", err)
	}
}
