package files

import "context"

const (
	// ShareTypeUser is Nextcloud shareType 0 (user).
	ShareTypeUser = 0
	// ShareTypeGroup is Nextcloud shareType 1 (group).
	ShareTypeGroup = 1
	// ShareTypeLink is Nextcloud shareType 3 (public link).
	ShareTypeLink = 3
)

// Share is one path-keyed share (link, user, or group).
type Share struct {
	ID           int64
	OwnerUserID  int64
	ShareType    int
	Path         string
	ItemType     string
	Token        string
	PasswordHash string
	Permissions  int
	Label        string
	ExpireMs     int64
	StimeMs      int64
	ShareWith    string
	Accepted     int
}

// IncomingMount is a share visible inside a sharee's files jail.
type IncomingMount struct {
	OwnerUID    string
	OwnerPath   string
	Mount       string
	Permissions int
	ItemType    string
}

// IncomingLookup lists accepted user/group shares for a sharee.
type IncomingLookup interface {
	ListIncoming(ctx context.Context, shareeUID string) ([]IncomingMount, error)
}

// ShareStore persists shares.
type ShareStore interface {
	Insert(ctx context.Context, s *Share) error
	GetByID(ctx context.Context, id int64) (*Share, error)
	GetByToken(ctx context.Context, token string) (*Share, error)
	ListByOwner(ctx context.Context, ownerUserID int64, pathFilter string) ([]Share, error)
	ListBySharee(ctx context.Context, shareWith string, groupGIDs []string) ([]Share, error)
	Update(ctx context.Context, s *Share) error
	Delete(ctx context.Context, id int64) error
	DeleteByPath(ctx context.Context, ownerUserID int64, filePath string) error
	RenamePath(ctx context.Context, ownerUserID int64, srcPath, dstPath string) error
	DeleteExpired(ctx context.Context, nowMs int64) error
}
