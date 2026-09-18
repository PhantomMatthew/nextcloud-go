//go:build integration

package migrations

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

func TestIntegrationPostgres(t *testing.T) {
	run(t, database.DialectPostgres, os.Getenv("NCGO_TEST_POSTGRES_DSN"))
}

func TestIntegrationMySQL(t *testing.T) {
	run(t, database.DialectMySQL, os.Getenv("NCGO_TEST_MYSQL_DSN"))
}

func run(t *testing.T, d database.Dialect, dsn string) {
	t.Helper()
	if dsn == "" {
		t.Skip("integration DSN not set")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, database.Config{Driver: d, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	std, ok := database.Unwrap(db)
	if !ok {
		t.Fatal("unwrap")
	}
	logger := slog.New(slog.DiscardHandler)
	if err := Down(ctx, std, d, 1, logger); err != nil {
		t.Logf("down (maybe empty): %v", err)
	}
	n, err := Up(ctx, std, d, logger)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if n < 1 {
		t.Fatalf("applied = %d", n)
	}
	v, dirty, err := Version(ctx, std, d)
	if err != nil {
		t.Fatal(err)
	}
	if v != 2 || dirty {
		t.Errorf("version=%d dirty=%v", v, dirty)
	}
}
