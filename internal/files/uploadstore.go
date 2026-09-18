package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// UploadSession is one chunked-upload v2 transfer folder.
type UploadSession struct {
	ID          int64
	UserID      int64
	TransferID  string
	Destination string
	TotalLength int64
	Created     time.Time
}

// UploadStore persists upload sessions.
type UploadStore interface {
	Create(ctx context.Context, s *UploadSession) error
	Get(ctx context.Context, userID int64, transferID string) (*UploadSession, error)
	UpdateDest(ctx context.Context, userID int64, transferID, dest string, total int64) error
	Delete(ctx context.Context, userID int64, transferID string) error
}

// SQLUploadStore is an UploadStore backed by database.DB.
type SQLUploadStore struct {
	db database.DB
}

// NewSQLUploadStore returns an UploadStore.
func NewSQLUploadStore(db database.DB) *SQLUploadStore {
	return &SQLUploadStore{db: db}
}

func (s *SQLUploadStore) Create(ctx context.Context, sess *UploadSession) error {
	if sess == nil || sess.UserID == 0 || sess.TransferID == "" {
		return fmt.Errorf("files: invalid upload session")
	}
	if !ValidTransferID(sess.TransferID) {
		return ErrInvalidPath
	}
	if sess.Created.IsZero() {
		sess.Created = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO uploads (user_id, transfer_id, destination, total_length, created_ms)
VALUES (?, ?, ?, ?, ?)`,
		sess.UserID, sess.TransferID, sess.Destination, sess.TotalLength, sess.Created.UTC().UnixMilli())
	if err != nil {
		if database.IsUniqueViolation(s.db.Dialect(), err) {
			return ErrExists
		}
		return fmt.Errorf("files: upload create: %w", err)
	}
	got, err := s.Get(ctx, sess.UserID, sess.TransferID)
	if err != nil {
		return err
	}
	*sess = *got
	return nil
}

func (s *SQLUploadStore) Get(ctx context.Context, userID int64, transferID string) (*UploadSession, error) {
	if userID == 0 || transferID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `
SELECT id, user_id, transfer_id, destination, total_length, created_ms
FROM uploads WHERE user_id = ? AND transfer_id = ?`, userID, transferID)
	var sess UploadSession
	var createdMs int64
	if err := row.Scan(&sess.ID, &sess.UserID, &sess.TransferID, &sess.Destination, &sess.TotalLength, &createdMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, database.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("files: upload get: %w", err)
	}
	sess.Created = time.UnixMilli(createdMs).UTC()
	return &sess, nil
}

func (s *SQLUploadStore) UpdateDest(ctx context.Context, userID int64, transferID, dest string, total int64) error {
	res, err := s.db.Exec(ctx, `
UPDATE uploads SET destination = ?, total_length = ? WHERE user_id = ? AND transfer_id = ?`,
		dest, total, userID, transferID)
	if err != nil {
		return fmt.Errorf("files: upload update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files: upload update rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLUploadStore) Delete(ctx context.Context, userID int64, transferID string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM uploads WHERE user_id = ? AND transfer_id = ?`, userID, transferID)
	if err != nil {
		return fmt.Errorf("files: upload delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("files: upload delete rows: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
