package database

import (
	"context"
	"errors"
	"testing"

	"github.com/Masterminds/squirrel"
)

func TestRebind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		d     Dialect
		query string
		want  string
	}{
		{"sqlite unchanged", DialectSQLite, "SELECT a FROM t WHERE id = ?", "SELECT a FROM t WHERE id = ?"},
		{"mysql unchanged", DialectMySQL, "SELECT a FROM t WHERE id = ?", "SELECT a FROM t WHERE id = ?"},
		{"postgres simple", DialectPostgres, "SELECT a FROM t WHERE id = ?", "SELECT a FROM t WHERE id = $1"},
		{"postgres two", DialectPostgres, "INSERT INTO t (a, b) VALUES (?, ?)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
		{"question in single quotes", DialectPostgres, "SELECT '?' FROM t WHERE id = ?", "SELECT '?' FROM t WHERE id = $1"},
		{"escaped single quotes", DialectPostgres, "SELECT 'it''s ? ok' FROM t WHERE a = ?", "SELECT 'it''s ? ok' FROM t WHERE a = $1"},
		{"question in double quotes", DialectPostgres, `SELECT "?" FROM t WHERE id = ?`, `SELECT "?" FROM t WHERE id = $1`},
		{"no placeholders", DialectPostgres, "SELECT 1", "SELECT 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Rebind(tt.d, tt.query)
			if got != tt.want {
				t.Errorf("Rebind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlaceholder(t *testing.T) {
	t.Parallel()
	sql, args, err := squirrel.Select("*").From("t").Where("id = ?", 1).
		PlaceholderFormat(Placeholder(DialectPostgres)).ToSql()
	if err != nil {
		t.Fatal(err)
	}
	if sql != "SELECT * FROM t WHERE id = $1" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 1 || args[0] != 1 {
		t.Errorf("args = %#v", args)
	}
	sql, _, err = squirrel.Select("*").From("t").Where("id = ?", 1).
		PlaceholderFormat(Placeholder(DialectSQLite)).ToSql()
	if err != nil {
		t.Fatal(err)
	}
	if sql != "SELECT * FROM t WHERE id = ?" {
		t.Errorf("sqlite sql = %q", sql)
	}
}

func TestOpenUnsupportedDriver(t *testing.T) {
	t.Parallel()
	_, err := Open(context.Background(), Config{Driver: "oracle", DSN: "x"})
	if !errors.Is(err, ErrUnsupportedDriver) {
		t.Errorf("err = %v, want ErrUnsupportedDriver", err)
	}
}

func TestSQLite(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, Config{
		Driver: DialectSQLite,
		DSN:    "file:wp2?mode=memory&cache=shared",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	std, ok := Unwrap(db)
	if !ok {
		t.Fatal("Unwrap failed")
	}
	if std.Stats().MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1", std.Stats().MaxOpenConnections)
	}

	if db.Dialect() != DialectSQLite {
		t.Errorf("dialect = %s", db.Dialect())
	}

	var fk int
	if err := db.QueryRow(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}

	exerciseDB(t, db)
}

func TestIsUniqueViolationNil(t *testing.T) {
	t.Parallel()
	if IsUniqueViolation(DialectSQLite, nil) {
		t.Fatal("nil should not be unique violation")
	}
}

func exerciseDB(t *testing.T, db DB) {
	t.Helper()
	ctx := context.Background()
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	if _, err := db.Exec(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, name VARCHAR(255) NOT NULL UNIQUE)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, 1, "alpha"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var name string
	if err := db.QueryRow(ctx, `SELECT name FROM items WHERE id = ?`, 1).Scan(&name); err != nil {
		t.Fatalf("select: %v", err)
	}
	if name != "alpha" {
		t.Errorf("name = %q", name)
	}

	_, err := db.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, 2, "alpha")
	if !IsUniqueViolation(db.Dialect(), err) {
		t.Fatalf("unique err = %v", err)
	}

	err = db.QueryRow(ctx, `SELECT name FROM items WHERE id = ?`, 99).Scan(&name)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("missing row err = %v", err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO items (id, name) VALUES (?, ?)`, 3, "beta"); err != nil {
		t.Fatalf("tx insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	err = db.QueryRow(ctx, `SELECT name FROM items WHERE id = ?`, 3).Scan(&name)
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("rolled back row still visible: %v", err)
	}

	rows, err := db.Query(ctx, `SELECT name FROM items WHERE id = ?`, 1)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("expected a row")
	}
	if err := rows.Scan(&name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "alpha" {
		t.Errorf("query name = %q", name)
	}
	if rows.Next() {
		t.Fatal("unexpected extra row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	res, err := db.Exec(ctx, `UPDATE items SET name = ? WHERE id = ?`, "gamma", 1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d", n)
	}
}
