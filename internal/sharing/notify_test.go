package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

type recordNotifier struct {
	inserted []notifications.Notification
	deleted  [][2]string
}

func (r *recordNotifier) Insert(_ context.Context, n *notifications.Notification) error {
	r.inserted = append(r.inserted, *n)
	return nil
}

func (r *recordNotifier) DeleteByObject(_ context.Context, objectType, objectID string) error {
	r.deleted = append(r.deleted, [2]string{objectType, objectID})
	return nil
}

type failNotifier struct{}

func (failNotifier) Insert(context.Context, *notifications.Notification) error {
	return errors.New("notifications down")
}

func (failNotifier) DeleteByObject(context.Context, string, string) error {
	return errors.New("notifications down")
}

func richParams(t *testing.T, n notifications.Notification) map[string]map[string]string {
	t.Helper()
	var params map[string]map[string]string
	if err := json.Unmarshal([]byte(n.SubjectRichParameters), &params); err != nil {
		t.Fatalf("subject rich parameters = %q: %v", n.SubjectRichParameters, err)
	}
	return params
}

func TestCreateUserShareNotifiesSharee(t *testing.T) {
	svc := testService(t)
	rec := &recordNotifier{}
	svc.Notifs = rec
	ctx := t.Context()

	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.inserted) != 1 {
		t.Fatalf("inserted = %+v", rec.inserted)
	}
	n := rec.inserted[0]
	bob, err := svc.Users.GetByUID(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	objectID := "ocinternal:" + strconv.FormatInt(sh.ID, 10)
	if n.UserID != bob.ID || n.UserUID != "bob" {
		t.Errorf("recipient = %d/%q, want %d/bob", n.UserID, n.UserUID, bob.ID)
	}
	if n.App != "files_sharing" || n.ObjectType != "share" || n.ObjectID != objectID {
		t.Errorf("app/object = %q %q %q, want files_sharing/share/%s", n.App, n.ObjectType, n.ObjectID, objectID)
	}
	if n.Subject != "You received /a.txt as a share by Alice" {
		t.Errorf("subject = %q", n.Subject)
	}
	if n.SubjectRich != "You received {share} as a share by {user}" {
		t.Errorf("subjectRich = %q", n.SubjectRich)
	}
	params := richParams(t, n)
	if len(params) != 2 {
		t.Errorf("params = %v", params)
	}
	if p := params["share"]; p["type"] != "highlight" || p["id"] != objectID || p["name"] != "/a.txt" {
		t.Errorf("share param = %v", p)
	}
	if p := params["user"]; p["type"] != "user" || p["id"] != "alice" || p["name"] != "Alice" {
		t.Errorf("user param = %v", p)
	}
	if !n.ShouldNotify {
		t.Error("ShouldNotify must be true")
	}
	if n.Message != "" || n.MessageRich != "" || n.Link != "" || n.Icon != "" {
		t.Errorf("message/link/icon must be empty: %+v", n)
	}
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if !n.CreatedAt.Equal(freeze) {
		t.Errorf("CreatedAt = %v, want %v", n.CreatedAt, freeze)
	}
}

func TestCreateGroupShareNotifiesMembersExceptActor(t *testing.T) {
	svc := testService(t)
	rec := &recordNotifier{}
	svc.Notifs = rec
	ctx := t.Context()
	if err := svc.Users.Create(ctx, &users.User{UID: "carol", DisplayName: "Carol", PasswordHash: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Users.CreateGroup(ctx, &users.Group{GID: "eng", DisplayName: "Engineers"}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"alice", "bob", "carol"} {
		if err := svc.Users.AddGroupMember(ctx, "eng", uid); err != nil {
			t.Fatal(err)
		}
	}

	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeGroup, 0, "eng", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.inserted) != 2 {
		t.Fatalf("inserted = %+v", rec.inserted)
	}
	objectID := "ocinternal:" + strconv.FormatInt(sh.ID, 10)
	for i, wantUID := range []string{"bob", "carol"} {
		n := rec.inserted[i]
		if n.UserUID != wantUID {
			t.Errorf("recipient[%d] = %q, want %q", i, n.UserUID, wantUID)
		}
		if n.App != "files_sharing" || n.ObjectType != "share" || n.ObjectID != objectID {
			t.Errorf("app/object = %q %q %q", n.App, n.ObjectType, n.ObjectID)
		}
		if n.Subject != "You received /a.txt to group eng as a share by Alice" {
			t.Errorf("subject = %q", n.Subject)
		}
		if n.SubjectRich != "You received {share} to group {group} as a share by {user}" {
			t.Errorf("subjectRich = %q", n.SubjectRich)
		}
		params := richParams(t, n)
		if len(params) != 3 {
			t.Errorf("params = %v", params)
		}
		if p := params["group"]; p["type"] != "user-group" || p["id"] != "eng" || p["name"] != "Engineers" {
			t.Errorf("group param = %v", p)
		}
		if p := params["share"]; p["type"] != "highlight" || p["id"] != objectID || p["name"] != "/a.txt" {
			t.Errorf("share param = %v", p)
		}
		if p := params["user"]; p["type"] != "user" || p["id"] != "alice" || p["name"] != "Alice" {
			t.Errorf("user param = %v", p)
		}
		if !n.ShouldNotify {
			t.Error("ShouldNotify must be true")
		}
	}
}

func TestCreateLinkAndRemoteSharesSilent(t *testing.T) {
	svc := testService(t)
	svc.OCM = &ocm.Client{HTTP: &http.Client{Transport: remoteOCMTransport{}}}
	n := 0
	svc.NewToken = func() string {
		n++
		return "ncgopublic" + strconv.Itoa(n)
	}
	rec := &recordNotifier{}
	svc.Notifs = rec
	ctx := t.Context()
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeRemote, 0, "bob@https://remote.example.com", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.inserted) != 0 {
		t.Fatalf("link/remote must not notify: %+v", rec.inserted)
	}
}

func TestNotifFailureDoesNotFailCreateOrDelete(t *testing.T) {
	svc := testService(t)
	svc.Notifs = failNotifier{}
	ctx := t.Context()
	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatalf("create must survive notification failure: %v", err)
	}
	alice, err := svc.Users.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.Store.ListByOwner(ctx, alice.ID, "")
	if err != nil || len(listed) != 1 {
		t.Fatalf("share must be inserted: %+v %v", listed, err)
	}
	if err := svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatalf("delete must survive notification failure: %v", err)
	}
	listed, err = svc.Store.ListByOwner(ctx, alice.ID, "")
	if err != nil || len(listed) != 0 {
		t.Fatalf("share must be deleted: %+v %v", listed, err)
	}
}

func TestDeleteDismissesShareNotifications(t *testing.T) {
	svc := testService(t)
	rec := &recordNotifier{}
	svc.Notifs = rec
	ctx := t.Context()
	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "alice", sh.ID); err != nil {
		t.Fatal(err)
	}
	want := [2]string{"share", "ocinternal:" + strconv.FormatInt(sh.ID, 10)}
	if len(rec.deleted) != 1 || rec.deleted[0] != want {
		t.Fatalf("deleted = %v, want [%v]", rec.deleted, want)
	}
}

func TestExpireDismissesShareNotifications(t *testing.T) {
	svc := testService(t)
	rec := &recordNotifier{}
	svc.Notifs = rec
	ctx := t.Context()
	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeUser, 0, "bob", "", "2020-01-01", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.inserted) != 1 {
		t.Fatalf("inserted = %+v", rec.inserted)
	}
	if _, err := svc.GetForOwner(ctx, "alice", sh.ID); err == nil {
		t.Fatal("expected expired share gone")
	}
	want := [2]string{"share", "ocinternal:" + strconv.FormatInt(sh.ID, 10)}
	if len(rec.deleted) != 1 || rec.deleted[0] != want {
		t.Fatalf("deleted = %v, want [%v]", rec.deleted, want)
	}
}
