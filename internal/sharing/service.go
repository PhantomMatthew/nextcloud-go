package sharing

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/notifications"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
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
	errFederate       = errors.New("sharing: cannot federate share")
)

// ShareNotifier is the notification sink for file-share bells (ADR-0082).
// *notifications.SQLStore satisfies it.
type ShareNotifier interface {
	Insert(ctx context.Context, n *notifications.Notification) error
	DeleteByObject(ctx context.Context, objectType, objectID string) error
}

// Service creates and serves public-link shares.
type Service struct {
	Store    files.ShareStore
	Files    *files.DAV
	Users    users.Store
	Hasher   auth.PasswordHasher
	Clock    func() time.Time
	NewToken func() string
	OCM      *ocm.Client
	Notifs   ShareNotifier
	Logger   *slog.Logger
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
	s.dismissShareNotifications(ctx, sh.ID)
	return true, nil
}

func (s *Service) warn(msg string, args ...any) {
	if s.Logger != nil {
		s.Logger.Warn(msg, args...)
	}
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

func (s *Service) Create(ctx context.Context, uid, pathName string, shareType, permissions int, shareWith, password, expireDate, label, origin string) (*files.Share, error) {
	switch shareType {
	case files.ShareTypeLink, files.ShareTypeUser, files.ShareTypeGroup, files.ShareTypeRemote:
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
	if shareType == files.ShareTypeRemote {
		remoteUID, remoteHost := ocm.SplitCloudID(shareWith)
		if remoteUID == "" || remoteHost == "" {
			return nil, errBadShareWith
		}
		remoteOrigin := ocm.NormalizeOrigin(remoteHost)
		if origin != "" && sameHTTPHost(origin, remoteOrigin) {
			return nil, errBadShareWith
		}
	}
	if shareType == files.ShareTypeLink {
		shareWith = ""
	}
	np, err := files.NormalizePath(pathName)
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
	if shareType == files.ShareTypeRemote {
		if err := s.notifyRemote(ctx, u, sh, origin); err != nil {
			if delErr := s.Store.Delete(ctx, sh.ID); delErr != nil {
				return nil, errors.Join(err, delErr)
			}
			return nil, err
		}
	}
	if shareType == files.ShareTypeUser || shareType == files.ShareTypeGroup {
		s.notifyShareCreated(ctx, u, sh)
	}
	return sh, nil
}

func (s *Service) notifyRemote(ctx context.Context, owner *users.User, sh *files.Share, origin string) error {
	if s.OCM == nil {
		return errFederate
	}
	_, remoteHost := ocm.SplitCloudID(sh.ShareWith)
	endPoint, err := s.OCM.Discover(ctx, ocm.NormalizeOrigin(remoteHost))
	if err != nil {
		return errFederate
	}
	ownerCloud := owner.UID
	if origin != "" {
		ownerCloud = owner.UID + "@" + origin
	}
	resType := sh.ItemType
	if resType == "" {
		resType = "file"
	}
	if err := s.OCM.NotifyOutgoing(ctx, endPoint, ocm.OutgoingNotice{
		ShareWith:    sh.ShareWith,
		Name:         path.Base(sh.Path),
		ProviderID:   strconv.FormatInt(sh.ID, 10),
		Owner:        ownerCloud,
		Sender:       ownerCloud,
		ResourceType: resType,
		Token:        sh.Token,
	}); err != nil {
		return errFederate
	}
	return nil
}

// shareNotifObjectID is Nextcloud's full notification object id for a share
// (providerId:id, provider `ocinternal`).
func shareNotifObjectID(id int64) string { return "ocinternal:" + strconv.FormatInt(id, 10) }

// richParam is one NC rich-object parameter (type/id/name).
type richParam struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// notifyShareCreated sends the incoming-share bell for user and group shares
// (ADR-0082). A notification failure never fails the share: it is Warn-logged
// and share creation continues.
func (s *Service) notifyShareCreated(ctx context.Context, owner *users.User, sh *files.Share) {
	if s.Notifs == nil {
		return
	}
	objectID := shareNotifObjectID(sh.ID)
	sharerName := owner.DisplayName
	if sharerName == "" {
		sharerName = owner.UID
	}
	params := map[string]richParam{
		"share": {Type: "highlight", ID: objectID, Name: sh.Path},
		"user":  {Type: "user", ID: owner.UID, Name: sharerName},
	}
	var subject, template string
	var recipients []*users.User
	switch sh.ShareType {
	case files.ShareTypeUser:
		sharee, err := s.Users.GetByUID(ctx, sh.ShareWith)
		if err != nil {
			s.warn("sharing: share notification: sharee lookup failed", slog.String("sharee", sh.ShareWith), slog.Any("err", err))
			return
		}
		subject = fmt.Sprintf("You received %s as a share by %s", sh.Path, sharerName)
		template = "You received {share} as a share by {user}"
		recipients = append(recipients, sharee)
	case files.ShareTypeGroup:
		gid := sh.ShareWith
		groupName := gid
		if g, err := s.Users.GetGroupByGID(ctx, gid); err == nil && g.DisplayName != "" {
			groupName = g.DisplayName
		}
		params["group"] = richParam{Type: "user-group", ID: gid, Name: groupName}
		subject = fmt.Sprintf("You received %s to group %s as a share by %s", sh.Path, gid, sharerName)
		template = "You received {share} to group {group} as a share by {user}"
		members, err := s.Users.GroupMembers(ctx, gid, 0)
		if err != nil {
			s.warn("sharing: share notification: group members lookup failed", slog.String("gid", gid), slog.Any("err", err))
			return
		}
		for _, m := range members {
			if m == owner.UID {
				continue
			}
			sharee, err := s.Users.GetByUID(ctx, m)
			if err != nil {
				s.warn("sharing: share notification: member lookup failed", slog.String("uid", m), slog.Any("err", err))
				continue
			}
			recipients = append(recipients, sharee)
		}
	default:
		return
	}
	rich, err := json.Marshal(params)
	if err != nil {
		s.warn("sharing: share notification: marshal rich parameters failed", slog.Any("err", err))
		return
	}
	for _, r := range recipients {
		n := &notifications.Notification{
			UserID:                r.ID,
			App:                   "files_sharing",
			UserUID:               r.UID,
			ObjectType:            "share",
			ObjectID:              objectID,
			Subject:               subject,
			SubjectRich:           template,
			SubjectRichParameters: string(rich),
			ShouldNotify:          true,
			CreatedAt:             s.now(),
		}
		if err := s.Notifs.Insert(ctx, n); err != nil {
			s.warn("sharing: share notification insert failed", slog.String("uid", r.UID), slog.Int64("share", sh.ID), slog.Any("err", err))
		}
	}
}

// dismissShareNotifications drops the bells of every recipient when a share
// is deleted (owner unshare or expiry). Best-effort: failures are Warn-logged
// and never change the delete's outcome.
func (s *Service) dismissShareNotifications(ctx context.Context, id int64) {
	if s.Notifs == nil {
		return
	}
	if err := s.Notifs.DeleteByObject(ctx, "share", shareNotifObjectID(id)); err != nil {
		s.warn("sharing: share notification dismiss failed", slog.Int64("share", id), slog.Any("err", err))
	}
}

func sameHTTPHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return false
	}
	return strings.EqualFold(ua.Host, ub.Host)
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
	if sh.ShareType == files.ShareTypeRemote {
		ignoreUnshareErr(s.notifyUnshare(ctx, sh))
	}
	if err := s.Store.Delete(ctx, sh.ID); err != nil {
		return err
	}
	s.dismissShareNotifications(ctx, sh.ID)
	return nil
}

func ignoreUnshareErr(err error) {
	if err == nil {
		return
	}
}

func (s *Service) notifyUnshare(ctx context.Context, sh *files.Share) error {
	if s.OCM == nil || sh == nil {
		return errFederate
	}
	_, remoteHost := ocm.SplitCloudID(sh.ShareWith)
	endPoint, err := s.OCM.Discover(ctx, ocm.NormalizeOrigin(remoteHost))
	if err != nil {
		return err
	}
	return s.OCM.NotifyUnshare(ctx, endPoint, ocm.UnshareNotice{
		ProviderID: strconv.FormatInt(sh.ID, 10),
		Token:      sh.Token,
	})
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
	case files.ShareTypeRemote:
		shareWithDisplay = sh.ShareWith
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
