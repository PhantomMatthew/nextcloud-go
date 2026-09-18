package files

import (
	"errors"
	"testing"
)

func TestSQLPropertyStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLPropertyStore(db)

	p := &FileProperty{UserID: uid, Path: "/a.txt", NS: PropNSOwnCloud, Name: PropFavorite, Value: "1"}
	if err := store.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	if p.ID == 0 {
		t.Fatal("id not assigned")
	}
	got, err := store.Get(ctx, uid, "/a.txt", PropNSOwnCloud, PropFavorite)
	if err != nil || got.Value != "1" {
		t.Fatalf("got = %+v err=%v", got, err)
	}
	p.Value = "0"
	if err := store.Set(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, uid, "/a.txt", PropNSOwnCloud, PropFavorite)
	if err != nil || got.Value != "0" {
		t.Fatalf("update = %+v err=%v", got, err)
	}
	listed, err := store.ListByPath(ctx, uid, "/a.txt")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list = %v %v", listed, err)
	}
	if err := store.CopyPath(ctx, uid, "/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/b.txt", PropNSOwnCloud, PropFavorite); err != nil {
		t.Fatal(err)
	}
	if err := store.RenamePath(ctx, uid, "/a.txt", "/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/c.txt", PropNSOwnCloud, PropFavorite); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/a.txt", PropNSOwnCloud, PropFavorite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed source still present: %v", err)
	}
	if err := store.DeleteByPath(ctx, uid, "/c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/c.txt", PropNSOwnCloud, PropFavorite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	if err := store.Remove(ctx, uid, "/b.txt", PropNSOwnCloud, PropFavorite); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "/b.txt", PropNSOwnCloud, PropFavorite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after remove = %v", err)
	}
}
