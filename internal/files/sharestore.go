package files

import (
	"context"
	"io"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const (
	// ShareTypeUser is Nextcloud shareType 0 (user).
	ShareTypeUser = 0
	// ShareTypeGroup is Nextcloud shareType 1 (group).
	ShareTypeGroup = 1
	// ShareTypeLink is Nextcloud shareType 3 (public link).
	ShareTypeLink = 3
	// ShareTypeRemote is Nextcloud shareType 6 (federated / OCM).
	ShareTypeRemote = 6
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
	OwnerUID     string
	OwnerPath    string
	Mount        string
	Permissions  int
	ItemType     string
	Remote       bool
	RemoteOrigin string
	RemoteToken  string
}

// RemoteFile fetches and mutates a federated share's public WebDAV.
type RemoteFile interface {
	Get(ctx context.Context, origin, token, rel string) (io.ReadCloser, *webdav.Entry, error)
	Propfind(ctx context.Context, origin, token, rel string, depth int) ([]*webdav.Entry, error)
	Put(ctx context.Context, origin, token, rel string, body io.Reader) (*webdav.Entry, error)
	Delete(ctx context.Context, origin, token, rel string) error
	Mkcol(ctx context.Context, origin, token, rel string) error
}

// IncomingLookup lists accepted user/group shares for a sharee.
type IncomingLookup interface {
	ListIncoming(ctx context.Context, shareeUID string) ([]IncomingMount, error)
}

// MultiIncoming concatenates several IncomingLookup sources in order.
type MultiIncoming []IncomingLookup

func (m MultiIncoming) ListIncoming(ctx context.Context, shareeUID string) ([]IncomingMount, error) {
	var out []IncomingMount
	for _, l := range m {
		if l == nil {
			continue
		}
		items, err := l.ListIncoming(ctx, shareeUID)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
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
	// DeleteExpired removes shares whose expiry is in the past and returns
	// the deleted ids (the background expire job dismisses their share
	// notifications, ADR-0083).
	DeleteExpired(ctx context.Context, nowMs int64) ([]int64, error)
}
