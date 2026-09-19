package ocm

import (
	"context"
	"errors"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/files"
)

var (
	ErrNotFound    = errors.New("ocm: not found")
	ErrInvalid     = errors.New("ocm: invalid")
	ErrConflict    = errors.New("ocm: conflict")
	ErrUnsupported = errors.New("ocm: unsupported")
)

// Incoming is one federated share received from a remote server.
type Incoming struct {
	ID          int64
	UserID      int64
	UserUID     string
	Name        string
	Remote      string
	RemoteID    string
	Owner       string
	Token       string
	ItemType    string
	Permissions int
	Accepted    int
	CreatedAt   time.Time
}

// Store persists inbound OCM shares.
type Store interface {
	Insert(ctx context.Context, in *Incoming) error
	List(ctx context.Context, userID int64) ([]Incoming, error)
	Get(ctx context.Context, userID, id int64) (*Incoming, error)
	Delete(ctx context.Context, userID, id int64) error
	files.IncomingLookup
}
