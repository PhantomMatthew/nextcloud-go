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
	if n != 20 {
		t.Errorf("applied = %d, want 20", n)
	}

	want := []string{
		"users", "groups", "group_members", "sessions",
		"app_passwords", "login_flows", "jobs", "module_config", "files", "uploads", "trash_items", "file_versions", "file_properties", "file_locks", "shares",
		"calendars", "calendar_objects", "addressbooks", "addressbook_objects",
		"notifications", "activities", "ocm_incoming", "calendar_shares", "plugins", "plugin_routes", "appconfig", "plugin_webdav_props", "addressbook_shares",
		"user_keys", "file_keys",
	}
	for _, table := range want {
		var name string
		err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
	// files gained the nullable key_uuid column.
	var keyUUIDCol string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='key_uuid'`).Scan(&keyUUIDCol)
	if err != nil {
		t.Errorf("files.key_uuid column: %v", err)
	}

	v, dirty, err := Version(ctx, std, database.DialectSQLite)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != 20 || dirty {
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
	if v != 19 || dirty {
		t.Errorf("after down version=%d dirty=%v", v, dirty)
	}
	var userKeysName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "user_keys").Scan(&userKeysName)
	if err == nil {
		t.Error("table user_keys still present after down to v19")
	}
	var fileKeysName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "file_keys").Scan(&fileKeysName)
	if err == nil {
		t.Error("table file_keys still present after down to v19")
	}
	var keyUUIDColAfter string
	err = db.QueryRow(ctx, `SELECT name FROM pragma_table_info('files') WHERE name='key_uuid'`).Scan(&keyUUIDColAfter)
	if err == nil {
		t.Error("files.key_uuid column still present after down to v19")
	}
	var bookSharesName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "addressbook_shares").Scan(&bookSharesName)
	if err != nil {
		t.Errorf("table addressbook_shares missing after down to v19: %v", err)
	}
	var propsName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugin_webdav_props").Scan(&propsName)
	if err != nil {
		t.Errorf("table plugin_webdav_props missing after down to v19: %v", err)
	}
	var appconfigName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "appconfig").Scan(&appconfigName)
	if err != nil {
		t.Errorf("table appconfig missing after down to v19: %v", err)
	}
	var pluginRoutesName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugin_routes").Scan(&pluginRoutesName)
	if err != nil {
		t.Errorf("table plugin_routes missing after down to v19: %v", err)
	}
	var pluginName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "plugins").Scan(&pluginName)
	if err != nil {
		t.Errorf("table plugins missing after down to v19: %v", err)
	}
	var sharesName string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "calendar_shares").Scan(&sharesName); err != nil {
		t.Errorf("table calendar_shares missing after down to v19: %v", err)
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
	if v != 20 || dirty {
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
