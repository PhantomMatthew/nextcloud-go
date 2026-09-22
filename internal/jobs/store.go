package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// Row is one jobs table record.
type Row struct {
	ID          int64
	Name        string
	Payload     []byte
	RunAt       int64
	StartedAt   int64
	CompletedAt int64
	LastError   string
	Attempts    int
	CreatedAt   int64
}

// Store persists queued jobs.
type Store interface {
	Insert(ctx context.Context, r *Row) error
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]Row, error)
	Complete(ctx context.Context, id int64) error
	Fail(ctx context.Context, id int64, errText string, retryAt time.Time) error
	DeleteByName(ctx context.Context, name string) error
}

// SQLStore is a Store backed by database.DB.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a Store.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) Insert(ctx context.Context, r *Row) error {
	if s == nil || r == nil || r.Name == "" {
		return fmt.Errorf("jobs: invalid row")
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UTC().UnixMilli()
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO jobs (name, payload, run_at, last_error, attempts, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, r.Name, r.Payload, r.RunAt, r.LastError, r.Attempts, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("jobs: insert: %w", err)
	}
	return nil
}

func (s *SQLStore) ClaimDue(ctx context.Context, now time.Time, limit int) (claimed []Row, err error) {
	if s == nil {
		return nil, fmt.Errorf("jobs: nil store")
	}
	if limit <= 0 {
		limit = 1
	}
	nowMs := now.UTC().UnixMilli()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rerr := tx.Rollback(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	rows, qerr := tx.Query(ctx, `
SELECT id, name, payload, run_at, started_at, completed_at, last_error, attempts, created_at
FROM jobs
WHERE started_at IS NULL AND completed_at IS NULL AND run_at <= ?
ORDER BY run_at, id
LIMIT ?`, nowMs, limit)
	if qerr != nil {
		return nil, fmt.Errorf("jobs: claim select: %w", qerr)
	}
	candidates, scanErr := scanRows(rows)
	if cerr := rows.Close(); cerr != nil && scanErr == nil {
		scanErr = fmt.Errorf("jobs: claim close: %w", cerr)
	}
	if scanErr != nil {
		return nil, scanErr
	}
	for i := range candidates {
		res, uerr := tx.Exec(ctx, `UPDATE jobs SET started_at = ? WHERE id = ? AND started_at IS NULL AND completed_at IS NULL`, nowMs, candidates[i].ID)
		if uerr != nil {
			return nil, fmt.Errorf("jobs: claim update: %w", uerr)
		}
		n, nerr := res.RowsAffected()
		if nerr != nil {
			return nil, fmt.Errorf("jobs: claim rows: %w", nerr)
		}
		if n == 1 {
			candidates[i].StartedAt = nowMs
			claimed = append(claimed, candidates[i])
		}
	}
	if cerr := tx.Commit(); cerr != nil {
		return nil, fmt.Errorf("jobs: claim commit: %w", cerr)
	}
	committed = true
	return claimed, nil
}

func (s *SQLStore) Complete(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	nowMs := time.Now().UTC().UnixMilli()
	_, err := s.db.Exec(ctx, `UPDATE jobs SET completed_at = ? WHERE id = ?`, nowMs, id)
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}
	return nil
}

func (s *SQLStore) Fail(ctx context.Context, id int64, errText string, retryAt time.Time) error {
	if id == 0 {
		return nil
	}
	_, err := s.db.Exec(ctx, `
UPDATE jobs SET started_at = NULL, last_error = ?, attempts = attempts + 1, run_at = ?
WHERE id = ?`, errText, retryAt.UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("jobs: fail: %w", err)
	}
	return nil
}

// DeleteByName removes every row for a job name, pending or finished; a
// name without rows is not an error. Plugin uninstall uses it to drop the
// plugin.<id> rows a removed plugin left behind.
func (s *SQLStore) DeleteByName(ctx context.Context, name string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM jobs WHERE name = ?`, name); err != nil {
		return fmt.Errorf("jobs: delete by name: %w", err)
	}
	return nil
}

func scanRows(rows database.Rows) ([]Row, error) {
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: scan: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRow(row rowScanner) (*Row, error) {
	var r Row
	var started, completed sql.NullInt64
	var last sql.NullString
	err := row.Scan(&r.ID, &r.Name, &r.Payload, &r.RunAt, &started, &completed, &last, &r.Attempts, &r.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrUnknownJob
		}
		return nil, fmt.Errorf("jobs: scan: %w", err)
	}
	if started.Valid {
		r.StartedAt = started.Int64
	}
	if completed.Valid {
		r.CompletedAt = completed.Int64
	}
	if last.Valid {
		r.LastError = last.String
	}
	return &r, nil
}
