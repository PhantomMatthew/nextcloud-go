package files

import (
	"errors"
	"testing"
	"time"
)

func TestSQLVersionStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLVersionStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)

	v := &FileVersion{
		UserID:   uid,
		Path:     "/a.txt",
		Revision: "1746100800",
		Size:     5,
		Checksum: "SHA256:ab",
		Created:  now,
	}
	if err := store.Insert(ctx, v); err != nil {
		t.Fatal(err)
	}
	if v.ID == 0 {
		t.Fatal("id not assigned")
	}
	got, err := store.Get(ctx, uid, "/a.txt", "1746100800")
	if err != nil || got.Size != 5 {
		t.Fatalf("got = %+v err=%v", got, err)
	}
	dup := &FileVersion{UserID: uid, Path: "/a.txt", Revision: "1746100800", Created: now}
	if err := store.Insert(ctx, dup); !errors.Is(err, ErrExists) {
		t.Fatalf("dup = %v", err)
	}
	listed, err := store.ListByPath(ctx, uid, "/a.txt")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}
	if err := store.RenamePath(ctx, uid, "/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/b.txt", "1746100800"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, uid, "/b.txt", "1746100800"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/b.txt", "1746100800"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestSQLVersionStoreDeleteExpired(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLVersionStore(db)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, &FileVersion{UserID: uid, Path: "/a.txt", Revision: "1577836800", Created: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, &FileVersion{UserID: uid, Path: "/a.txt", Revision: "1746100800", Created: fresh}); err != nil {
		t.Fatal(err)
	}
	expired, err := store.DeleteExpired(ctx, uid, "/a.txt", time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired = %v %v", expired, err)
	}
	if _, err := store.Get(ctx, uid, "/a.txt", "1577836800"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old still present: %v", err)
	}
}

func TestValidRevision(t *testing.T) {
	t.Parallel()
	if !ValidRevision("1746100800") || !ValidRevision("1746100800-1") {
		t.Fatal("want valid")
	}
	for _, rev := range []string{"", "abc", "../1", "1/2"} {
		if ValidRevision(rev) {
			t.Errorf("%q want invalid", rev)
		}
	}
}
