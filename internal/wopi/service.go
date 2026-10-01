package wopi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// errNotFound is the 404 sentinel: minting for a file the user cannot see
// and resolving a callback both collapse to it, so a WOPI caller never
// learns whether a file id exists (no existence oracle).
var errNotFound = errors.New("wopi: file not found")

// errIsDir rejects directory file ids: mint maps it to 400, the callbacks
// map it to 404 (a dir has no WOPI document).
var errIsDir = errors.New("wopi: file id is a directory")

// IncomingLister lists a sharee's accepted incoming shares. *sharing.Service
// satisfies it structurally; wopi must not import sharing (sharing imports
// files, and wopi imports files), so the seam is declared here. A nil Shares
// disables sharee resolution (owner-only minting).
type IncomingLister interface {
	ListIncoming(ctx context.Context, uid string) ([]files.IncomingMount, error)
}

// TokenKeyWrapper mirrors ADR-0102's app-token wraps for WOPI tokens
// (satisfied structurally by *encrypt.SQLResolver).
type TokenKeyWrapper interface {
	WrapKeyForWOPIToken(ctx context.Context, tokenRaw string, fileID int64, priv []byte) error
	UnlockForWOPIToken(ctx context.Context, tokenRaw string, fileID int64) ([]byte, error)
}

// Service mints and redeems WOPI access tokens.
type Service struct {
	Store Store
	Files *files.DAV
	Users users.Store
	// Shares, when non-nil, resolves files shared INTO the minter's jail.
	Shares IncomingLister
	// Keys, when non-nil, seals the minter's unlocked key under the new
	// token at mint and opens it on the anonymous callbacks (ADR-0107);
	// nil disables wrap support (encryption off).
	Keys TokenKeyWrapper
	// NewToken, when non-nil, generates the bearer token string.
	NewToken func() string
	TTL      time.Duration
	Clock    func() time.Time
}

// defaultTokenTTL mirrors the office.token_ttl config default.
const defaultTokenTTL = 10 * time.Hour

func (s *Service) clock() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return defaultTokenTTL
}

// Mint issues a WOPI access token for fileID in the CALLER's session ctx:
// for an enrolled password-wrapped user (ADR-0100) that ctx carries the
// unlocked key, so name resolution succeeds here. When Keys is wired and the
// session principal holds a 32-byte unlocked key, the key is also sealed
// under the new token (ADR-0107, wrap-at-mint mirroring ADR-0102), so the
// later anonymous WOPI callback opens it instead of hitting the ErrKeyLocked
// → 403 boundary. A wrap failure fails the mint LOUDLY (mirroring
// login_v2.go's grant): the token row is rolled back and the error returned
// — a silently unwrapped token would 403 every callback. With no Keys or no
// usable unlocked key (app-password-auth minter, unenrolled user) the wrap
// is skipped silently: the token authenticates but its callbacks keep the
// documented 403 boundary (ADR-0102 §3's pinned no-wrap behavior).
func (s *Service) Mint(ctx context.Context, uid string, fileID int64) (string, time.Time, bool, error) {
	_, _, perms, err := s.resolve(ctx, uid, fileID)
	if err != nil {
		return "", time.Time{}, false, err
	}
	tok, err := s.newToken()
	if err != nil {
		return "", time.Time{}, false, err
	}
	expiresAt := s.clock().Add(s.ttl())
	row := &Token{
		Token:     tok,
		UID:       uid,
		FileID:    fileID,
		CanWrite:  perms&webdav.PermUpdate != 0,
		ExpiresAt: expiresAt,
	}
	if err := s.Store.Insert(ctx, row); err != nil {
		return "", time.Time{}, false, err
	}
	if s.Keys != nil {
		if p, ok := auth.UserFromContext(ctx); ok && len(p.UnlockedKey) == 32 {
			if err := s.Keys.WrapKeyForWOPIToken(ctx, tok, fileID, p.UnlockedKey); err != nil {
				if derr := s.Store.Delete(ctx, tok); derr != nil {
					return "", time.Time{}, false, errors.Join(err, derr)
				}
				return "", time.Time{}, false, err
			}
		}
	}
	return tok, expiresAt, row.CanWrite, nil
}

// resolve maps (uid, fileID) to the DAV coordinates the WOPI callbacks
// operate on: the owner's own path for the owner, the sharee-side mount
// path for a sharee (DAV read/write/lock dispatch handles incoming mounts
// natively). Anything the user may not see is errNotFound — the WOPI
// surface exposes no existence oracle.
func (s *Service) resolve(ctx context.Context, uid string, fileID int64) (fsUser, fsPath string, perms int, err error) {
	// Meta is the app-wired TranslatingStore (never the raw store): GetByID
	// decrypts the row in ctx, which is what makes an enrolled user hit the
	// ErrKeyLocked → 403 boundary in the anonymous callback ctx.
	row, err := s.Files.Meta.GetByID(ctx, fileID)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			return "", "", 0, errNotFound
		}
		return "", "", 0, err
	}
	if row.IsDir {
		return "", "", 0, errIsDir
	}
	owner, err := s.Users.GetByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return "", "", 0, errNotFound
		}
		return "", "", 0, err
	}
	if owner.UID == uid {
		return uid, row.Path, webdav.PermRead | webdav.PermUpdate, nil
	}
	if s.Shares == nil {
		return "", "", 0, errNotFound
	}
	mounts, err := s.Shares.ListIncoming(ctx, uid)
	if err != nil {
		// An enrolled sharee without an unlocked session surfaces
		// ErrKeyLocked here — propagate it: the callback maps it to 403
		// (the ADR-0101 boundary), never to a misleading 404.
		return "", "", 0, err
	}
	for i := range mounts {
		m := mounts[i]
		if row.Path != m.OwnerPath && !strings.HasPrefix(row.Path, m.OwnerPath+"/") {
			continue
		}
		return uid, m.Mount + strings.TrimPrefix(row.Path, m.OwnerPath), m.Permissions, nil
	}
	return "", "", 0, errNotFound
}

// ownerUID resolves the owning user's UID for CheckFileInfo's OwnerId. It
// runs only after resolve succeeded, so a file the token's user cannot see
// never reaches it (no oracle).
func (s *Service) ownerUID(ctx context.Context, fileID int64) (string, error) {
	row, err := s.Files.Meta.GetByID(ctx, fileID)
	if err != nil {
		if errors.Is(err, files.ErrNotFound) {
			return "", errNotFound
		}
		return "", err
	}
	owner, err := s.Users.GetByID(ctx, row.UserID)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return "", errNotFound
		}
		return "", err
	}
	return owner.UID, nil
}

// friendlyName is the minter's display name, falling back to the UID.
func (s *Service) friendlyName(ctx context.Context, uid string) string {
	u, err := s.Users.GetByUID(ctx, uid)
	if err != nil || u.DisplayName == "" {
		return uid
	}
	return u.DisplayName
}

// Authenticate redeems an access_token: missing and expired rows both yield
// ErrTokenNotFound (the handler maps it to 401).
func (s *Service) Authenticate(ctx context.Context, accessToken string) (*Token, error) {
	t, err := s.Store.GetByToken(ctx, accessToken, s.clock())
	if err != nil {
		return nil, err
	}
	return t, nil
}

// newToken returns a random bearer token: the injected generator when set
// (tests), else 32 crypto/rand bytes hex-encoded. The token is a
// machine-generated bearer credential (never user-typed), so it is stored
// plaintext like login-flow and public-share tokens (ADR-0106).
func (s *Service) newToken() (string, error) {
	if s.NewToken != nil {
		t := s.NewToken()
		if t == "" {
			return "", errors.New("wopi: token generator returned empty token")
		}
		return t, nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// keyLocked reports whether err is the encrypted-key-locked boundary
// (dual-matched via files' keyLockedError mechanism: encrypt.ErrKeyLocked
// or webdav.ErrForbidden produced by that wrapper). The WOPI boundary maps
// it to 403.
func keyLocked(err error) bool {
	return errors.Is(err, encrypt.ErrKeyLocked)
}
