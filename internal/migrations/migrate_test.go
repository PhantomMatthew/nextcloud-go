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
	if n != 7 {
		t.Errorf("applied = %d, want 7", n)
	}

	want := []string{
		"users", "groups", "group_members", "sessions",
		"app_passwords", "login_flows", "jobs", "module_config", "files", "uploads", "trash_items", "file_versions", "file_properties", "file_locks",
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
	if v != 7 || dirty {
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
	if v != 6 || dirty {
		t.Errorf("after down version=%d dirty=%v", v, dirty)
	}
	var locksName string
	err = db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "file_locks").Scan(&locksName)
	if err == nil {
		t.Error("table file_locks still present after down to v6")
	}
	var propsName string
	if err := db.QueryRow(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "file_properties").Scan(&propsName); err != nil {
		t.Errorf("table file_properties missing after down to v6: %v", err)
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
	if v != 7 || dirty {
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
