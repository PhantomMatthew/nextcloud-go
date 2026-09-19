package sharing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

const tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var (
	errBadShareType   = errors.New("sharing: unsupported share type")
	errBadPermissions = errors.New("sharing: invalid permissions")
	errBadExpire      = errors.New("sharing: invalid expire date")
	errBadShareWith   = errors.New("sharing: unknown sharee")
	errUnauthorized   = errors.New("sharing: unauthorized")
)

// Service creates and serves public-link shares.
type Service struct {
	Store    files.ShareStore
	Files    *files.DAV
	Users    users.Store
	Hasher   auth.PasswordHasher
	Clock    func() time.Time
	NewToken func() string
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) token() (string, error) {
	if s.NewToken != nil {
		return s.NewToken(), nil
	}
	return randomShareToken()
}

func randomShareToken() (string, error) {
	var out [15]byte
	for i := range out {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		out[i] = tokenAlphabet[int(b[0])%len(tokenAlphabet)]
	}
	return string(out[:]), nil
}

func (s *Service) expireIfNeeded(ctx context.Context, sh *files.Share) (bool, error) {
	if sh == nil || sh.ExpireMs == 0 || sh.ExpireMs > s.now().UnixMilli() {
		return false, nil
	}
	if err := s.Store.Delete(ctx, sh.ID); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) ownerOf(ctx context.Context, uid string) (*users.User, error) {
	u, err := s.Users.GetByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, files.ErrNotFound
		}
		return nil, err
	}
	return u, nil
}

func (s *Service) Create(ctx context.Context, uid, path string, shareType, permissions int, shareWith, password, expireDate, label string) (*files.Share, error) {
	switch shareType {
	case files.ShareTypeLink, files.ShareTypeUser, files.ShareTypeGroup:
	default:
		return nil, errBadShareType
	}
	u, err := s.ownerOf(ctx, uid)
	if err != nil {
		return nil, err
	}
	if shareType == files.ShareTypeUser {
		if shareWith == "" || shareWith == uid {
			return nil, errBadShareWith
		}
		if _, err := s.Users.GetByUID(ctx, shareWith); err != nil {
			return nil, errBadShareWith
		}
	}
	if shareType == files.ShareTypeGroup {
		if shareWith == "" {
			return nil, errBadShareWith
		}
		if _, err := s.Users.GetGroupByGID(ctx, shareWith); err != nil {
			return nil, errBadShareWith
		}
	}
	if shareType == files.ShareTypeLink {
		shareWith = ""
	}
	np, err := files.NormalizePath(path)
	if err != nil {
		return nil, err
	}
	ent, err := s.Files.Stat(ctx, uid, np)
	if err != nil {
		if errors.Is(err, webdav.ErrNotFound) {
			return nil, files.ErrNotFound
		}
		return nil, err
	}
	itemType := "file"
	if ent.IsDir {
		itemType = "folder"
	}
	perms := permissions
	if perms == 0 {
		perms = webdav.PermRead
	}
	if !validSharePerms(itemType, perms) {
		return nil, errBadPermissions
	}
	tok, err := s.token()
	if err != nil {
		return nil, err
	}
	hash := ""
	if password != "" {
		if s.Hasher == nil {
			return nil, fmt.Errorf("sharing: hasher required")
		}
		hash, err = s.Hasher.Hash(password)
		if err != nil {
			return nil, err
		}
	}
	expireMs, err := parseExpireDate(expireDate)
	if err != nil {
		return nil, err
	}
	if shareType != files.ShareTypeLink {
		hash = ""
	}
	sh := &files.Share{
		OwnerUserID:  u.ID,
		ShareType:    shareType,
		Path:         np,
		ItemType:     itemType,
		Token:        tok,
		PasswordHash: hash,
		Permissions:  perms,
		Label:        label,
		ExpireMs:     expireMs,
		StimeMs:      s.now().UnixMilli(),
		ShareWith:    shareWith,
		Accepted:     1,
	}
	if err := s.Store.Insert(ctx, sh); err != nil {
		return nil, err
	}
	return sh, nil
}

func validSharePerms(itemType string, perms int) bool {
	if itemType == "file" {
		return perms == webdav.PermRead
	}
	return perms&^(webdav.PermRead|webdav.PermUpdate|webdav.PermCreate|webdav.PermDelete) == 0 && perms != 0
}

func (s *Service) GetForOwner(ctx context.Context, uid string, id int64) (*files.Share, error) {
	u, err := s.ownerOf(ctx, uid)
	if err != nil {
		return nil, err
	}
	sh, err := s.Store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	expired, err := s.expireIfNeeded(ctx, sh)
	if err != nil {
		return nil, err
	}
	if expired || sh.OwnerUserID != u.ID {
		return nil, files.ErrNotFound
	}
	return sh, nil
}

func (s *Service) ListForOwner(ctx context.Context, uid, pathFilter string) ([]files.Share, error) {
	u, err := s.ownerOf(ctx, uid)
	if err != nil {
		return nil, err
	}
	items, err := s.Store.ListByOwner(ctx, u.ID, pathFilter)
	if err != nil {
		return nil, err
	}
	var out []files.Share
	for i := range items {
		expired, err := s.expireIfNeeded(ctx, &items[i])
		if err != nil {
			return nil, err
		}
		if expired {
			continue
		}
		out = append(out, items[i])
	}
	return out, nil
}

func (s *Service) Update(ctx context.Context, uid string, id int64, permissions *int, password, expireDate, label *string) (*files.Share, error) {
	sh, err := s.GetForOwner(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	if permissions != nil {
		if !validSharePerms(sh.ItemType, *permissions) {
			return nil, errBadPermissions
		}
		sh.Permissions = *permissions
	}
	if password != nil {
		if *password == "" {
			sh.PasswordHash = ""
		} else {
			if s.Hasher == nil {
				return nil, fmt.Errorf("sharing: hasher required")
			}
			hash, err := s.Hasher.Hash(*password)
			if err != nil {
				return nil, err
			}
			sh.PasswordHash = hash
		}
	}
	if expireDate != nil {
		ms, err := parseExpireDate(*expireDate)
		if err != nil {
			return nil, err
		}
		sh.ExpireMs = ms
	}
	if label != nil {
		sh.Label = *label
	}
	if err := s.Store.Update(ctx, sh); err != nil {
		return nil, err
	}
	return sh, nil
}

func (s *Service) Delete(ctx context.Context, uid string, id int64) error {
	sh, err := s.GetForOwner(ctx, uid, id)
	if err != nil {
		return err
	}
	return s.Store.Delete(ctx, sh.ID)
}

// LookupValid returns a non-expired share and its owner.
func (s *Service) LookupValid(ctx context.Context, token string) (*files.Share, *users.User, error) {
	sh, err := s.Store.GetByToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	expired, err := s.expireIfNeeded(ctx, sh)
	if err != nil {
		return nil, nil, err
	}
	if expired || sh.ShareType != files.ShareTypeLink {
		return nil, nil, files.ErrNotFound
	}
	owner, err := s.Users.GetByID(ctx, sh.OwnerUserID)
	if err != nil {
		return nil, nil, err
	}
	return sh, owner, nil
}

func (s *Service) ResolvePublic(ctx context.Context, token, password string) (*files.Share, *users.User, error) {
	sh, owner, err := s.LookupValid(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if sh.PasswordHash != "" {
		if s.Hasher == nil {
			return nil, nil, errUnauthorized
		}
		ok, err := s.Hasher.Verify(sh.PasswordHash, password)
		if err != nil || !ok {
			return nil, nil, errUnauthorized
		}
	}
	return sh, owner, nil
}

func (s *Service) SharePayload(ctx context.Context, r *http.Request, sh *files.Share) (any, error) {
	owner, err := s.Users.GetByID(ctx, sh.OwnerUserID)
	if err != nil {
		return nil, err
	}
	ent, err := s.Files.Stat(ctx, owner.UID, sh.Path)
	if err != nil && !errors.Is(err, webdav.ErrNotFound) {
		return nil, err
	}
	mime := "application/octet-stream"
	var fileID int64
	if ent != nil {
		mime = ent.ContentType
		fileID = int64(ent.NumericID & 0x7fffffffffffffff)
		if ent.IsDir {
			mime = "httpd/unix-directory"
		}
	}
	display := owner.DisplayName
	if display == "" {
		display = owner.UID
	}
	expiration := ""
	if sh.ExpireMs > 0 {
		expiration = time.UnixMilli(sh.ExpireMs).UTC().Format("2006-01-02 15:04:05")
	}
	url := ""
	if sh.ShareType == files.ShareTypeLink {
		url = shareURL(r, sh.Token)
	}
	shareWithDisplay := ""
	switch sh.ShareType {
	case files.ShareTypeUser:
		if rec, err := s.Users.GetByUID(ctx, sh.ShareWith); err == nil {
			shareWithDisplay = rec.DisplayName
			if shareWithDisplay == "" {
				shareWithDisplay = rec.UID
			}
		} else {
			shareWithDisplay = sh.ShareWith
		}
	case files.ShareTypeGroup:
		if g, err := s.Users.GetGroupByGID(ctx, sh.ShareWith); err == nil {
			shareWithDisplay = g.DisplayName
			if shareWithDisplay == "" {
				shareWithDisplay = g.GID
			}
		} else {
			shareWithDisplay = sh.ShareWith
		}
	}
	return shareMap(sh, owner.UID, display, mime, fileID, url, expiration, shareWithDisplay), nil
}

// ListIncoming implements files.IncomingLookup.
func (s *Service) ListIncoming(ctx context.Context, shareeUID string) ([]files.IncomingMount, error) {
	if s == nil || s.Store == nil || shareeUID == "" {
		return nil, nil
	}
	if _, err := s.Users.GetByUID(ctx, shareeUID); err != nil {
		if errors.Is(err, users.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	gids, err := s.Users.UserGroupGIDs(ctx, shareeUID)
	if err != nil {
		return nil, err
	}
	items, err := s.Store.ListBySharee(ctx, shareeUID, gids)
	if err != nil {
		return nil, err
	}
	var out []files.IncomingMount
	for i := range items {
		expired, err := s.expireIfNeeded(ctx, &items[i])
		if err != nil {
			return nil, err
		}
		if expired || items[i].Path == "/" {
			continue
		}
		owner, err := s.Users.GetByID(ctx, items[i].OwnerUserID)
		if err != nil {
			continue
		}
		mount := "/" + pathBase(items[i].Path)
		out = append(out, files.IncomingMount{
			OwnerUID:    owner.UID,
			OwnerPath:   items[i].Path,
			Mount:       mount,
			Permissions: items[i].Permissions,
			ItemType:    items[i].ItemType,
		})
	}
	return out, nil
}

func pathBase(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func shareURL(r *http.Request, token string) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS == nil {
		host := r.Host
		if host == "localhost" || strings.HasPrefix(host, "127.") {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host + "/s/" + token
}

func parseExpireDate(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, time.UTC); err == nil {
		return t.UnixMilli(), nil
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return 0, errBadExpire
	}
	return t.Add(23*time.Hour + 59*time.Minute + 59*time.Second).UnixMilli(), nil
}
