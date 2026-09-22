package migrations

import (
	"context"
	"log/slog"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

func TestSQLiteUpDownUp(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{
		Driver: database.DialectSQLite,
		DSN:    "file:wp3?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}

	logger := slog.New(slog.DiscardHandler)
	n, err := Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if n != 16 {
		t.Errorf("applied = %d, want 16", n)
	}

	want := []string{
		"users", "groups", "group_members", "sessions",
		"app_passwords", "login_flows", "jobs", "module_config", "files", "uploads", "trash_items", "file_versions", "file_properties", "file_locks", "shares",
		"calendars", "calendar_objects", "addressbooks", "addressbook_objects",
		"notifications", "activities", "ocm_incoming", "calendar_shares", "plugins", "plugin_routes",
	}
	for _, table := range want {
		var name string
		err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}

	v, dirty, err := Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != 16 || dirty {
		t.Errorf("version=%d dirty=%v", v, dirty)
	}

	n, err = Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n != 0 {
		t.Errorf("second up applied = %d, want 0", n)
	}

	if err := Down(ctx, std, database.DialectSQLite, 1, logger); err != nil {
		t.Fatalf("down: %v", err)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after down: %v", err)
	}
	if v != 15 || dirty {
		t.Errorf("after down version=%d dirty=%v", v, dirty)
	}
	var pluginRoutesName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugin_routes").Scan(&pluginRoutesName)
	if err == nil {
		t.Error("table plugin_routes still present after down to v15")
	}
	var pluginName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugins").Scan(&pluginName)
	if err != nil {
		t.Errorf("table plugins missing after down to v15: %v", err)
	}
	var sharesName string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "calendar_shares").Scan(&sharesName); err != nil {
		t.Errorf("table calendar_shares missing after down to v14: %v", err)
	}

	n, err = Up(ctx, std, database.DialectSQLite, logger)
	if err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if n != 1 {
		t.Errorf("re-up applied = %d, want 1", n)
	}
	v, dirty, err = Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version after re-up: %v", err)
	}
	if v != 16 || dirty {
		t.Errorf("after re-up version=%d dirty=%v", v, dirty)
	}
}

func TestUnsupportedDialect(t *testing.T) {
	t.Parallel()
	_, _, err := Version(context.Background(), nil, "oracle")
	if err == nil {
		t.Fatal("expected error")
	}
}
