package files

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

type uploadedPayload struct {
	User    string `msgpack:"user"`
	Path    string `msgpack:"path"`
	Size    int64  `msgpack:"size"`
	Created bool   `msgpack:"created"`
}

func TestDAVWriteEmitsUploaded(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:" + t.Name() + "?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	if _, err := migrations.Up(ctx, std, database.DialectSQLite, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := NewDAV(st, NewSQLStore(db), us)

	bus := events.NewBus(nil)
	var got []events.Event
	bus.Subscribe(func(_ context.Context, ev events.Event) { got = append(got, ev) })
	dav.Events = bus

	if _, err := dav.Mkdir(ctx, "alice", "/docs"); err != nil {
		t.Fatal(err)
	}
	if _, created, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("hello")), nil); err != nil || !created {
		t.Fatalf("write created=%v err=%v", created, err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	ev := got[0]
	if ev.Topic != "files.uploaded" || ev.Source != "host" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.UserID != "alice" {
		t.Fatalf("event UserID = %q, want alice", ev.UserID)
	}
	var p uploadedPayload
	if err := msgpack.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.User != "alice" || p.Path != "/docs/a.txt" || p.Size != 5 || !p.Created {
		t.Fatalf("payload = %+v", p)
	}

	// An overwrite emits created=false.
	if _, created, err := dav.Write(ctx, "alice", "/docs/a.txt", bytes.NewReader([]byte("hello!")), nil); err != nil || created {
		t.Fatalf("rewrite created=%v err=%v", created, err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if err := msgpack.Unmarshal(got[1].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Created || p.Size != 6 {
		t.Fatalf("rewrite payload = %+v", p)
	}
}
