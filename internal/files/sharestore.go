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
	// ShareTypeEmail is Nextcloud shareType 4 (share by mail). The M6
	// sharees exact-email bucket advertises it; creating such shares is not
	// implemented (mail_send stays 0).
	ShareTypeEmail = 4
	// ShareTypeRemote is Nextcloud shareType 6 (federated / OCM).
	ShareTypeRemote = 6
)

// Share is one path-keyed share (link, user, or group). With filename
// encryption on (ADR-0104 phase 3a), Path carries the owner's
// ciphertext-materialized path and the two enc fields hold the share-scoped
// metadata copies sealed under the share target's own key: MountNameEnc is
// the NCGOFN1 token of the mount basename (the basename's tree token lives
// under the share root's PARENT directory key, which sharees never hold),
// and AbsPathEnc is the plaintext absolute owner path sealed by SealPath
// (content ops through the mount need it for storage-key derivation when the
// owner's ancestor chain is unresolvable to the sharee). Both are empty for
// plaintext (scheme-0) shares.
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
	// MountNameEnc is the share-root basename sealed under the share
	// target's own key (NCGOFN1 token form); empty for plaintext shares.
	MountNameEnc string
	// AbsPathEnc is the plaintext absolute owner path sealed under the
	// share target's own key (NCGOSP1); empty for plaintext shares.
	AbsPathEnc string
}

// IncomingMount is a share visible inside a sharee's files jail.
type IncomingMount struct {
	OwnerUID  string
	OwnerPath string
	// OwnerCipherPath is the owner tree's ciphertext path of the share root
	// (ADR-0104 phase 3a): non-empty when the owner's tree is scheme 1. DAV
	// then anchors name resolution at the share root (the sharee's wraps
	// cover every in-subtree directory key) instead of walking ancestors the
	// sharee cannot resolve. Empty for plaintext shares and remote mounts.
	OwnerCipherPath string
	Mount           string
	Permissions     int
	ItemType        string
	Remote          bool
	RemoteOrigin    string
	RemoteToken     string
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

// ShareMetaResealer re-seals share rows' sealed metadata (mount_name_enc,
// abs_path_enc) after a subtree rename rewrote their ciphertext file_path
// prefix (ADR-0104 phase 3a). *sharing.Service satisfies it — files cannot
// import sharing (sharing imports files), so the seam is declared here
// structurally. Nil-ok on DAV: the never-enable carve-out has no sealed
// share rows.
type ShareMetaResealer interface {
	ResealShareMeta(ctx context.Context, ownerUserID int64, srcPlain, dstPlain string) error
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
