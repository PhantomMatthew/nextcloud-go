package files

import (
	"errors"
	"testing"
	"time"
)

func TestSQLUploadStoreCRUD(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLUploadStore(db)
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	sess := &UploadSession{
		UserID:      uid,
		TransferID:  "3847562910",
		Destination: "/path/to/file.bin",
		TotalLength: 12,
		Created:     now,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if sess.ID == 0 {
		t.Fatal("id not assigned")
	}

	got, err := store.Get(ctx, uid, "3847562910")
	if err != nil {
		t.Fatal(err)
	}
	if got.Destination != "/path/to/file.bin" || got.TotalLength != 12 {
		t.Fatalf("got = %+v", got)
	}

	dup := &UploadSession{UserID: uid, TransferID: "3847562910", Created: now}
	if err := store.Create(ctx, dup); !errors.Is(err, ErrExists) {
		t.Fatalf("dup = %v, want ErrExists", err)
	}

	if err := store.UpdateDest(ctx, uid, "3847562910", "/other.bin", 99); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, uid, "3847562910")
	if err != nil {
		t.Fatal(err)
	}
	if got.Destination != "/other.bin" || got.TotalLength != 99 {
		t.Fatalf("after update = %+v", got)
	}

	if err := store.Delete(ctx, uid, "3847562910"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, uid, "3847562910"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestSQLUploadStoreInvalidTransferID(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	uid := seedUser(t, db)
	store := NewSQLUploadStore(db)
	sess := &UploadSession{UserID: uid, TransferID: "../evil"}
	if err := store.Create(ctx, sess); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("err = %v, want ErrInvalidPath", err)
	}
}
