package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	sqlite "modernc.org/sqlite"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const sqlitePragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

// Open pings a new pool for cfg. SQLite DSNs have busy_timeout, WAL, and
// foreign_keys pragmas appended. A DSN containing mode=memory forces
// MaxOpenConns=1.
func Open(ctx context.Context, cfg Config) (DB, error) {
	name, dsn, err := driverName(cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, err
	}
	std, err := sql.Open(name, dsn)
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}
	applyPool(std, cfg)
	if err := std.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("database: ping: %w", err), std.Close())
	}
	return &sqlDB{db: std, d: cfg.Driver}, nil
}

func driverName(d Dialect, dsn string) (string, string, error) {
	switch d {
	case DialectPostgres:
		return "pgx", dsn, nil
	case DialectMySQL:
		return "mysql", dsn, nil
	case DialectSQLite:
		return "sqlite", withSQLitePragmas(dsn), nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrUnsupportedDriver, d)
	}
}

func withSQLitePragmas(dsn string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + sqlitePragmas
	}
	return dsn + "?" + sqlitePragmas
}

func applyPool(std *sql.DB, cfg Config) {
	if cfg.MaxOpenConns > 0 {
		std.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if strings.Contains(cfg.DSN, "mode=memory") {
		std.SetMaxOpenConns(1)
	}
	if cfg.MaxIdleConns > 0 {
		std.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if cfg.ConnMaxLifetime > 0 {
		std.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
}

// Unwrap returns the underlying *sql.DB when db is the stdlib-backed pool.
func Unwrap(db DB) (*sql.DB, bool) {
	s, ok := db.(*sqlDB)
	if !ok || s == nil {
		return nil, false
	}
	return s.db, true
}

// IsUniqueViolation reports whether err is a unique-constraint failure for d.
func IsUniqueViolation(d Dialect, err error) bool {
	if err == nil {
		return false
	}
	switch d {
	case DialectPostgres:
		var pg *pgconn.PgError
		return errors.As(err, &pg) && pg.Code == "23505"
	case DialectMySQL:
		var my *mysql.MySQLError
		return errors.As(err, &my) && my.Number == 1062
	case DialectSQLite:
		var se *sqlite.Error
		if !errors.As(err, &se) {
			return false
		}
		switch se.Code() {
		case 2067, 1555:
			return true
		}
		return false
	default:
		return false
	}
}
