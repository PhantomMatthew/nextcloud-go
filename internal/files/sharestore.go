package files

import "context"

// ShareTypeLink is Nextcloud shareType 3 (public link).
const ShareTypeLink = 3

// Share is one path-keyed public link.
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
}

// ShareStore persists public-link shares.
type ShareStore interface {
	Insert(ctx context.Context, s *Share) error
	GetByID(ctx context.Context, id int64) (*Share, error)
	GetByToken(ctx context.Context, token string) (*Share, error)
	ListByOwner(ctx context.Context, ownerUserID int64, pathFilter string) ([]Share, error)
	Update(ctx context.Context, s *Share) error
	Delete(ctx context.Context, id int64) error
	DeleteByPath(ctx context.Context, ownerUserID int64, filePath string) error
	RenamePath(ctx context.Context, ownerUserID int64, srcPath, dstPath string) error
	DeleteExpired(ctx context.Context, nowMs int64) error
}
