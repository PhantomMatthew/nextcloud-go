package database

import (
	"context"
	"database/sql"
	"time"

	"github.com/Masterminds/squirrel"
)

// Querier is the subset of database operations available on both DB and Tx.
type Querier interface {
	Query(ctx context.Context, q string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, q string, args ...any) Row
	Exec(ctx context.Context, q string, args ...any) (Result, error)
}

// DB is a dialect-aware connection pool.
type DB interface {
	Querier
	Begin(ctx context.Context) (Tx, error)
	Close() error
	Ping(ctx context.Context) error
	Dialect() Dialect
}

// Tx is a dialect-aware transaction.
type Tx interface {
	Querier
	Commit() error
	Rollback() error
}

// Rows is an iterator over a query result set.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Close() error
	Err() error
}

// Row is a single-row query result.
type Row interface {
	Scan(dest ...any) error
}

// Result is the outcome of an Exec.
type Result interface {
	RowsAffected() (int64, error)
}

// Config is the connection-pool configuration passed to Open.
type Config struct {
	Driver          Dialect
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Placeholder returns the squirrel placeholder format for d.
func Placeholder(d Dialect) squirrel.PlaceholderFormat {
	if d == DialectPostgres {
		return squirrel.Dollar
	}
	return squirrel.Question
}

var (
	_ DB     = (*sqlDB)(nil)
	_ Tx     = (*sqlTx)(nil)
	_ Rows   = (*sql.Rows)(nil)
	_ Row    = (*sql.Row)(nil)
	_ Result = sql.Result(nil)
)
