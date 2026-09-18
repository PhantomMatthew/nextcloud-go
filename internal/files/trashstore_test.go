package files

import (
	"errors"
	"testing"
	"time"
)

func TestSQLTrashStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLTrashStore(db)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	item := &TrashItem{
		UserID:       uid,
		OriginalPath: "/docs/a.txt",
		LocationID:   "a.txt.d1746100800",
		Name:         "a.txt",
		Size:         5,
		Deleted:      now,
		DeletedBy:    "alice",
	}
	if err := store.Insert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if item.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := store.GetByLocation(ctx, uid, "a.txt.d1746100800")
	if err != nil {
		t.Fatal(err)
	}
	if got.OriginalPath != "/docs/a.txt" || got.Size != 5 || got.IsDir {
		t.Fatalf("got = %+v", got)
	}

	dup := &TrashItem{UserID: uid, LocationID: "a.txt.d1746100800", OriginalPath: "/x", Deleted: now}
	if err := store.Insert(ctx, dup); !errors.Is(err, ErrExists) {
		t.Fatalf("dup = %v, want ErrExists", err)
	}

	listed, err := store.List(ctx, uid)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}

	if err := store.Delete(ctx, uid, "a.txt.d1746100800"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByLocation(ctx, uid, "a.txt.d1746100800"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestSQLTrashStoreDeleteExpired(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLTrashStore(db)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Insert(ctx, &TrashItem{UserID: uid, OriginalPath: "/old.txt", LocationID: "old.txt.d1577836800", Deleted: old}); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, &TrashItem{UserID: uid, OriginalPath: "/new.txt", LocationID: "new.txt.d1746100800", Deleted: fresh}); err != nil {
		t.Fatal(err)
	}
	n, err := store.DeleteExpired(ctx, uid, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 1 {
		t.Fatalf("expired n=%d err=%v", n, err)
	}
	if _, err := store.GetByLocation(ctx, uid, "old.txt.d1577836800"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old still present: %v", err)
	}
	if _, err := store.GetByLocation(ctx, uid, "new.txt.d1746100800"); err != nil {
		t.Fatalf("new missing: %v", err)
	}
}

func TestValidLocationID(t *testing.T) {
	t.Parallel()
	ok := []string{"a.txt.d1746100800", "assembled.bin.d1746100800-1"}
	for _, id := range ok {
		if !ValidLocationID(id) {
			t.Errorf("%q want valid", id)
		}
	}
	bad := []string{"", "../x.d1", "foo/bar.d1", "nodot", "a.d", "..d1"}
	for _, id := range bad {
		if ValidLocationID(id) {
			t.Errorf("%q want invalid", id)
		}
	}
}
