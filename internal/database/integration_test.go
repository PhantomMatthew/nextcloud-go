//go:build integration

package database

import (
	"context"
	"os"
	"testing"
)

func TestIntegrationPostgres(t *testing.T) {
	dsn := os.Getenv("NCGO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NCGO_TEST_POSTGRES_DSN not set")
	}
	runIntegration(t, DialectPostgres, dsn)
}

func TestIntegrationMySQL(t *testing.T) {
	dsn := os.Getenv("NCGO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("NCGO_TEST_MYSQL_DSN not set")
	}
	runIntegration(t, DialectMySQL, dsn)
}

func runIntegration(t *testing.T, d Dialect, dsn string) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, Config{Driver: d, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS items`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	exerciseDB(t, db)
}
