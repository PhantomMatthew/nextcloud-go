package files

import (
	"errors"
	"testing"
	"time"
)

func TestSQLLockStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLLockStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

	l := &FileLock{
		UserID:    uid,
		Path:      "/a.txt",
		Token:     "opaquelocktoken:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Owner:     "alice",
		TimeoutMs: now + 1800_000,
		CreatedMs: now,
	}
	if err := store.Insert(ctx, l); err != nil {
		t.Fatal(err)
	}
	if l.ID == 0 {
		t.Fatal("id not assigned")
	}
	got, err := store.GetByPath(ctx, uid, "/a.txt")
	if err != nil || got.Token != l.Token {
		t.Fatalf("get path = %+v err=%v", got, err)
	}
	byTok, err := store.GetByToken(ctx, l.Token)
	if err != nil || byTok.ID != l.ID {
		t.Fatalf("get token = %+v err=%v", byTok, err)
	}
	dup := *l
	dup.ID = 0
	dup.Token = "opaquelocktoken:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := store.Insert(ctx, &dup); !errors.Is(err, ErrExists) {
		t.Fatalf("unique path = %v", err)
	}
	if err := store.UpdateTimeout(ctx, l.ID, now+3600_000); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetByPath(ctx, uid, "/a.txt")
	if err != nil || got.TimeoutMs != now+3600_000 {
		t.Fatalf("timeout = %+v err=%v", got, err)
	}
	if err := store.RenamePath(ctx, uid, "/a.txt", "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renamed source still present: %v", err)
	}
	if _, err := store.GetByPath(ctx, uid, "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteByPath(ctx, uid, "/b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestSQLLockStoreDeleteExpired(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLLockStore(db)
	now := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	dead := &FileLock{
		UserID: uid, Path: "/dead.txt", Token: "opaquelocktoken:deaddeaddeaddeaddeaddeaddeaddead",
		Owner: "alice", TimeoutMs: now - 1, CreatedMs: now,
	}
	live := &FileLock{
		UserID: uid, Path: "/live.txt", Token: "opaquelocktoken:livelivelivelivelivelivelivelive",
		Owner: "alice", TimeoutMs: now + 1800_000, CreatedMs: now,
	}
	if err := store.Insert(ctx, dead); err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteExpired(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByPath(ctx, uid, "/dead.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired still present: %v", err)
	}
	if _, err := store.GetByPath(ctx, uid, "/live.txt"); err != nil {
		t.Fatal(err)
	}
}
