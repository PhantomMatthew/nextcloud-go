package notifications

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("notifications: not found")
	ErrInvalid  = errors.New("notifications: invalid")
)

// Notification is one OCS notification row.
type Notification struct {
	ID                    int64
	UserID                int64
	App                   string
	UserUID               string
	ObjectType            string
	ObjectID              string
	Subject               string
	SubjectRich           string
	SubjectRichParameters string
	Message               string
	MessageRich           string
	MessageRichParameters string
	Link                  string
	Icon                  string
	ShouldNotify          bool
	CreatedAt             time.Time
}

// Store persists notifications.
type Store interface {
	List(ctx context.Context, userID int64) ([]Notification, error)
	Get(ctx context.Context, userID, id int64) (*Notification, error)
	Delete(ctx context.Context, userID, id int64) error
	DeleteAll(ctx context.Context, userID int64) error
	Insert(ctx context.Context, n *Notification) error
	ListETag(ctx context.Context, userID int64) (string, error)
}
