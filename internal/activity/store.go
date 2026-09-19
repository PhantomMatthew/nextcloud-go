package activity

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("activity: not found")
	ErrInvalid  = errors.New("activity: invalid")
)

// Event is one activity stream row.
type Event struct {
	ID                    int64
	UserID                int64
	ActorUID              string
	App                   string
	Type                  string
	Subject               string
	SubjectRich           string
	SubjectRichParameters string
	Message               string
	ObjectType            string
	ObjectID              int64
	ObjectName            string
	Link                  string
	Icon                  string
	CreatedAt             time.Time
}

// Store persists activity events.
type Store interface {
	List(ctx context.Context, userID, since int64, limit int, sort string) ([]Event, error)
	Insert(ctx context.Context, e *Event) error
}
