package users

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/migrations"
)

func testDB(t *testing.T) database.DB {
	t.Helper()
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
	return db
}

func hasher() *auth.Argon2id {
	return auth.NewArgon2id(auth.Argon2idParams{MemoryKB: 8, Iterations: 1, Parallelism: 1, SaltLen: 8, KeyLen: 16})
}

func TestSQLStoreUsers(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	hash, err := h.Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	u := &User{UID: "alice", DisplayName: "Alice", PasswordHash: hash, Enabled: true}
	if err := store.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 {
		t.Error("expected last insert id")
	}
	got, err := store.GetByUID(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Alice" || !got.Enabled {
		t.Errorf("%+v", got)
	}
	byID, err := store.GetByID(ctx, got.ID)
	if err != nil || byID.UID != "alice" {
		t.Fatalf("GetByID = %+v %v", byID, err)
	}
	if err := store.Create(ctx, u); !errors.Is(err, ErrExists) {
		t.Errorf("dup = %v", err)
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Errorf("count = %d %v", n, err)
	}
	newHash, err := h.Hash("other")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePasswordHash(ctx, got.ID, newHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByUID(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
}

func TestSQLStoreGroups(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	alice := &User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	bob := &User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}
	if err := store.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	g := &Group{GID: "team", DisplayName: "Team"}
	if err := store.CreateGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	if g.ID == 0 {
		t.Fatal("group id")
	}
	if err := store.AddGroupMember(ctx, "team", "alice"); err != nil {
		t.Fatal(err)
	}
	gids, err := store.UserGroupGIDs(ctx, "alice")
	if err != nil || len(gids) != 1 || gids[0] != "team" {
		t.Fatalf("alice gids = %v %v", gids, err)
	}
	bobGIDs, err := store.UserGroupGIDs(ctx, "bob")
	if err != nil || len(bobGIDs) != 0 {
		t.Fatalf("bob gids = %v %v", bobGIDs, err)
	}
}

func TestSQLStoreSetEnabledListDelete(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	for _, uid := range []string{"alice", "bob", "carol"} {
		if err := store.Create(ctx, &User{UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetEnabled(ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetByUID(ctx, "bob")
	if err != nil || got.Enabled {
		t.Fatalf("after disable = %+v %v", got, err)
	}
	if err := store.SetEnabled(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("set enabled missing = %v", err)
	}

	all, err := store.List(ctx, 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("list all = %v %v", all, err)
	}
	if all[0].UID != "alice" || all[2].UID != "carol" {
		t.Errorf("list order = %v", all)
	}
	page, err := store.List(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].UID != "bob" {
		t.Fatalf("list page = %v %v", page, err)
	}

	if err := store.CreateGroup(ctx, &Group{GID: "team"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddGroupMember(ctx, "team", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetByUID(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete = %v", err)
	}
	members, err := store.GroupMembers(ctx, "team", 0)
	if err != nil || len(members) != 0 {
		t.Fatalf("members after delete = %v %v", members, err)
	}
	if err := store.Delete(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing = %v", err)
	}
}

func TestSQLStoreGroupOps(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	for _, uid := range []string{"alice", "bob", "carol"} {
		if err := store.Create(ctx, &User{UID: uid, DisplayName: uid, PasswordHash: "x", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateGroup(ctx, &Group{GID: "team", DisplayName: "Team"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateGroup(ctx, &Group{GID: "ops"}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"carol", "alice", "bob"} {
		if err := store.AddGroupMember(ctx, "team", uid); err != nil {
			t.Fatal(err)
		}
	}
	members, err := store.GroupMembers(ctx, "team", 0)
	if err != nil || len(members) != 3 || members[0] != "alice" || members[2] != "carol" {
		t.Fatalf("members = %v %v", members, err)
	}
	limited, err := store.GroupMembers(ctx, "team", 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited members = %v %v", limited, err)
	}
	if _, err := store.GroupMembers(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("members of missing group = %v", err)
	}

	if err := store.RemoveGroupMember(ctx, "team", "carol"); err != nil {
		t.Fatal(err)
	}
	members, err = store.GroupMembers(ctx, "team", 0)
	if err != nil || len(members) != 2 {
		t.Fatalf("members after remove = %v %v", members, err)
	}
	if err := store.RemoveGroupMember(ctx, "team", "carol"); err != nil {
		t.Errorf("removing non-member = %v", err)
	}
	if err := store.RemoveGroupMember(ctx, "missing", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove from missing group = %v", err)
	}

	groups, err := store.ListGroups(ctx, 0, 0)
	if err != nil || len(groups) != 2 || groups[0].GID != "ops" || groups[1].GID != "team" {
		t.Fatalf("list groups = %v %v", groups, err)
	}
	page, err := store.ListGroups(ctx, 1, 1)
	if err != nil || len(page) != 1 || page[0].GID != "team" {
		t.Fatalf("list groups page = %v %v", page, err)
	}

	if err := store.DeleteGroup(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	gids, err := store.UserGroupGIDs(ctx, "alice")
	if err != nil || len(gids) != 0 {
		t.Fatalf("alice gids after group delete = %v %v", gids, err)
	}
	if err := store.DeleteGroup(ctx, "team"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing group = %v", err)
	}
}

func TestPasswordVerifier(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	hash, err := h.Hash("secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &User{UID: "alice", DisplayName: "Alice", PasswordHash: hash, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &User{UID: "bob", DisplayName: "Bob", PasswordHash: hash, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	v := NewPasswordVerifier(store, h)
	if _, err := v.Verify(ctx, "", "secret"); !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("empty: %v", err)
	}
	p, err := v.Verify(ctx, "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "alice" || p.DisplayName != "Alice" {
		t.Errorf("%+v", p)
	}
	if _, err := v.Verify(ctx, "alice", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong pw: %v", err)
	}
	if _, err := v.Verify(ctx, "nobody", "secret"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("missing: %v", err)
	}
	if _, err := v.Verify(ctx, "bob", "secret"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("disabled: %v", err)
	}
}

func TestEnsureBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	store := NewSQLStore(testDB(t))
	h := hasher()
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{}, slog.New(slog.DiscardHandler)); !errors.Is(err, ErrNoUsers) {
		t.Errorf("empty bootstrap: %v", err)
	}
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{UID: "admin", Password: "admin", DisplayName: "Admin"}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	n, err := store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("count = %d %v", n, err)
	}
	if err := EnsureBootstrapAdmin(ctx, store, h, BootstrapAdmin{UID: "other", Password: "x"}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	n, err = store.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("second bootstrap count = %d %v", n, err)
	}
}

func TestSearchAndSearchGroups(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	s := NewSQLStore(db)
	for _, u := range []*User{
		{UID: "alice", DisplayName: "Alice A", PasswordHash: "x", Enabled: true},
		{UID: "bob", DisplayName: "Bob B", PasswordHash: "x", Enabled: true},
		{UID: "bobby", DisplayName: "Bobby C", PasswordHash: "x", Enabled: false},
	} {
		if err := s.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateGroup(ctx, &Group{GID: "engineers", DisplayName: "Engineers"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateGroup(ctx, &Group{GID: "sales", DisplayName: "Sales"}); err != nil {
		t.Fatal(err)
	}

	found, err := s.Search(ctx, "bob", 20)
	if err != nil || len(found) != 1 || found[0].UID != "bob" {
		t.Fatalf("search bob = %v %v (disabled bobby must be excluded)", found, err)
	}
	found, err = s.Search(ctx, "a", 20)
	if err != nil || len(found) != 1 {
		t.Fatalf("search a = %v %v (disabled bobby must be excluded)", found, err)
	}
	if got, _ := s.Search(ctx, "", 20); got != nil {
		t.Fatalf("empty term = %v", got)
	}
	groups, err := s.SearchGroups(ctx, "eng", 20)
	if err != nil || len(groups) != 1 || groups[0].GID != "engineers" {
		t.Fatalf("groups = %v %v", groups, err)
	}
	if got, _ := s.SearchGroups(ctx, "zzz", 20); len(got) != 0 {
		t.Fatalf("no match = %v", got)
	}
}
