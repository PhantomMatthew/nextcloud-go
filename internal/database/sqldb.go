package database

import (
	"context"
	"database/sql"
)

type sqlDB struct {
	db *sql.DB
	d  Dialect
}

func (s *sqlDB) Dialect() Dialect { return s.d }

func (s *sqlDB) Close() error { return s.db.Close() }

func (s *sqlDB) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *sqlDB) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	return s.db.QueryContext(ctx, Rebind(s.d, q), args...)
}

func (s *sqlDB) QueryRow(ctx context.Context, q string, args ...any) Row {
	return s.db.QueryRowContext(ctx, Rebind(s.d, q), args...)
}

func (s *sqlDB) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	return s.db.ExecContext(ctx, Rebind(s.d, q), args...)
}

func (s *sqlDB) Begin(ctx context.Context) (Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlTx{tx: tx, d: s.d}, nil
}

type sqlTx struct {
	tx *sql.Tx
	d  Dialect
}

func (t *sqlTx) Query(ctx context.Context, q string, args ...any) (Rows, error) {
	return t.tx.QueryContext(ctx, Rebind(t.d, q), args...)
}

func (t *sqlTx) QueryRow(ctx context.Context, q string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, Rebind(t.d, q), args...)
}

func (t *sqlTx) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	return t.tx.ExecContext(ctx, Rebind(t.d, q), args...)
}

func (t *sqlTx) Commit() error { return t.tx.Commit() }

func (t *sqlTx) Rollback() error { return t.tx.Rollback() }
