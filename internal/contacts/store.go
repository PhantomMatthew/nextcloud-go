package contacts

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound     = errors.New("contacts: not found")
	ErrExists       = errors.New("contacts: exists")
	ErrConflict     = errors.New("contacts: conflict")
	ErrInvalid      = errors.New("contacts: invalid")
	ErrForbidden    = errors.New("contacts: forbidden")
	ErrNotSupported = errors.New("contacts: method not allowed")
)

const (
	DefaultBookURI       = "contacts"
	DefaultDisplayName   = "Contacts"
	ShareAccessRead      = "read"
	ShareAccessReadWrite = "read-write"
)

// Addressbook is a CardDAV addressbook collection.
type Addressbook struct {
	ID          int64
	UserID      int64
	URI         string
	DisplayName string
	Description string
	Enabled     bool
	CTag        int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Object is a vCard object.
type Object struct {
	ID            int64
	AddressbookID int64
	URI           string
	UID           string
	FN            string
	ETag          string
	Size          int64
	Data          []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Store persists addressbooks and objects.
type Store interface {
	EnsureHome(ctx context.Context, userID int64) error
	ListBooks(ctx context.Context, userID int64) ([]Addressbook, error)
	GetBookByURI(ctx context.Context, userID int64, uri string) (*Addressbook, error)
	CreateBook(ctx context.Context, b *Addressbook) error
	UpdateBook(ctx context.Context, b *Addressbook) error
	DeleteBook(ctx context.Context, userID int64, uri string) error
	PutObject(ctx context.Context, userID int64, bookURI string, obj *Object) (created bool, err error)
	GetObject(ctx context.Context, userID int64, bookURI, uri string) (*Object, error)
	DeleteObject(ctx context.Context, userID int64, bookURI, uri string) error
	ListObjects(ctx context.Context, userID int64, bookURI string) ([]Object, error)
	GetByUID(ctx context.Context, bookID int64, uid string) (*Object, error)
	UpsertAddressbookShare(ctx context.Context, bookID, targetUserID int64, access string) error
	DeleteAddressbookShare(ctx context.Context, bookID, targetUserID int64) error
	ListSharedAddressbooks(ctx context.Context, userID int64) ([]SharedAddressbook, error)
	GetSharedAddressbook(ctx context.Context, userID int64, uri string) (*SharedAddressbook, error)
}

// Share grants a target user access to an addressbook.
type Share struct {
	ID            int64
	AddressbookID int64
	TargetUserID  int64
	Access        string // ShareAccessRead | ShareAccessReadWrite
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SharedAddressbook is an addressbook visible to a sharee, with the owner UID
// and the granted access level.
type SharedAddressbook struct {
	Addressbook
	OwnerUID string
	Access   string
}
