package login

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// SQLStore persists login-flow v2 state.
type SQLStore struct {
	db database.DB
}

// NewSQLStore returns a login Store backed by db.
func NewSQLStore(db database.DB) *SQLStore {
	return &SQLStore{db: db}
}

func scanFlow(row database.Row) (*Flow, error) {
	var f Flow
	var created, expires int64
	var server, loginName, appPassword sql.NullString
	if err := row.Scan(&f.PollToken, &f.LoginToken, &f.StateToken, &f.ClientName, &f.State,
		&server, &loginName, &appPassword, &created, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrFlowNotFound
		}
		return nil, err
	}
	f.Server = server.String
	f.LoginName = loginName.String
	f.AppPassword = appPassword.String
	if strings.HasPrefix(f.StateToken, pendingStatePrefix) {
		f.StateToken = ""
	}
	f.CreatedAt = time.UnixMilli(created).UTC()
	f.ExpiresAt = time.UnixMilli(expires).UTC()
	return &f, nil
}

func (s *SQLStore) Insert(ctx context.Context, f *Flow) error {
	if f == nil || f.PollToken == "" || f.LoginToken == "" {
		return errors.New("login: invalid flow")
	}
	st := f.StateToken
	if st == "" {
		st = pendingState(f.PollToken)
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO login_flows (poll_token, login_token, state_token, client_name, state, server, login_name, app_password, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.PollToken, f.LoginToken, st, f.ClientName, int(f.State),
		nullStr(f.Server), nullStr(f.LoginName), nullStr(f.AppPassword),
		f.CreatedAt.UTC().UnixMilli(), f.ExpiresAt.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("login: insert: %w", err)
	}
	return nil
}

func (s *SQLStore) GetByPoll(ctx context.Context, pollToken string) (*Flow, error) {
	return scanFlow(s.db.QueryRow(ctx, loginSelect+" WHERE poll_token = ?", pollToken))
}

func (s *SQLStore) GetByLogin(ctx context.Context, loginToken string) (*Flow, error) {
	return scanFlow(s.db.QueryRow(ctx, loginSelect+" WHERE login_token = ?", loginToken))
}

func (s *SQLStore) GetByState(ctx context.Context, stateToken string) (*Flow, error) {
	return scanFlow(s.db.QueryRow(ctx, loginSelect+" WHERE state_token = ?", stateToken))
}

func (s *SQLStore) Update(ctx context.Context, f *Flow) error {
	if f == nil || f.PollToken == "" {
		return errors.New("login: invalid flow")
	}
	st := f.StateToken
	if st == "" {
		st = pendingState(f.PollToken)
	}
	res, err := s.db.Exec(ctx, `
UPDATE login_flows SET login_token=?, state_token=?, client_name=?, state=?, server=?, login_name=?, app_password=?, expires_at=?
WHERE poll_token=?`,
		f.LoginToken, st, f.ClientName, int(f.State),
		nullStr(f.Server), nullStr(f.LoginName), nullStr(f.AppPassword),
		f.ExpiresAt.UTC().UnixMilli(), f.PollToken)
	if err != nil {
		return fmt.Errorf("login: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrFlowNotFound
	}
	return nil
}

func (s *SQLStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM login_flows WHERE expires_at > 0 AND expires_at < ?`, now.UTC().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("login: delete expired: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

const loginSelect = `SELECT poll_token, login_token, state_token, client_name, state, server, login_name, app_password, created_at, expires_at FROM login_flows`

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const pendingStatePrefix = "pending:"

func pendingState(pollToken string) string {
	return pendingStatePrefix + pollToken
}
