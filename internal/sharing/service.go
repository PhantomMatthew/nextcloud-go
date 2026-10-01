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

// ShareKeys is the per-user file-key wrap hook (ADR-0098):
// *files.KeySharer satisfies it. Nil disables key wrapping.
type ShareKeys interface {
	WrapForShare(ctx context.Context, sh *files.Share) error
	UnwrapForShare(ctx context.Context, sh *files.Share) error
}

// NameCodec is the ADR-0104 share-metadata cipher seam: share rows carry the
// ciphertext owner path plus the sealed mount name / absolute path copies
// (phase 3a), and share-notification subjects embed the mount-name token the
// OCS render opens in the viewer's ctx (phase 3b). *files.NameTranslator
// satisfies it; nil keeps every share row plaintext (the filename-encryption
// flag off — bit-identical behavior).
type NameCodec interface {
	// ShareCipherPath maps a plaintext owner path to the ciphertext form
	// stored in shares.file_path, reporting whether the user is scheme 1.
	ShareCipherPath(ctx context.Context, userID int64, plainPath string) (ct string, encrypted bool, err error)
	// SealShareMeta computes the grant-time ciphertext path and sealed
	// metadata copies for a plaintext owner path.
	SealShareMeta(ctx context.Context, userID int64, plainPath string) (ctPath, mountNameEnc, absPathEnc string, err error)
	// OpenShareMeta resolves a share row to its plaintext absolute owner
	// path and mount basename in the caller's ctx.
	OpenShareMeta(ctx context.Context, ownerUserID int64, sh *files.Share) (plainAbsPath, mountName string, err error)
	// ShareSubjectMeta computes the phase-3b notification subject token for a
	// share row (ADR-0104 §9): the mount basename's NCGOFN1 token under the
	// share target's own key — byte-identical to the row's MountNameEnc —
	// plus the sealing key's UUID hex for the list-time render.
	ShareSubjectMeta(ctx context.Context, ownerUserID int64, ctPath string) (nameToken, keyUUIDHex string, err error)
}

// shareMetaStore is the rename re-seal seam over the share store
// (ADR-0104 phase 3a): *SQLShareStore satisfies it. ResealShareMeta reports
// an error when the wired store lacks it and the codec is live.
type shareMetaStore interface {
	ListByPrefix(ctx context.Context, ownerUserID int64, filePath string) ([]files.Share, error)
	UpdateEncFields(ctx context.Context, id int64, mountNameEnc, absPathEnc string) error
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
	Keys     ShareKeys
	// NameCodec, when set (filename encryption on, ADR-0104 phase 3a), seals
	// share metadata at grant and opens it for owner/sharee-facing views; it
	// also supplies the phase-3b notification subject token. Nil keeps
	// plaintext share rows (flag off — bit-identical).
	NameCodec NameCodec
	Logger    *slog.Logger
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
	s.unwrapShareKeys(ctx, sh)
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
	// ADR-0104 phase 3a: a scheme-1 owner's share row carries the ciphertext
	// path plus the sealed mount-name/absolute-path copies, so sharees anchor
	// name resolution at the share root. A seal failure is loud (500) — never
	// a plaintext fallback. Scheme 0 (or no codec wired) stores today's exact
	// plaintext row.
	storePath, mountNameEnc, absPathEnc := np, "", ""
	if s.NameCodec != nil {
		ctPath, mEnc, aEnc, err := s.NameCodec.SealShareMeta(ctx, u.ID, np)
		if err != nil {
			return nil, err
		}
		storePath, mountNameEnc, absPathEnc = ctPath, mEnc, aEnc
	}
	sh := &files.Share{
		OwnerUserID:  u.ID,
		ShareType:    shareType,
		Path:         storePath,
		ItemType:     itemType,
		Token:        tok,
		PasswordHash: hash,
		Permissions:  perms,
		Label:        label,
		ExpireMs:     expireMs,
		StimeMs:      s.now().UnixMilli(),
		ShareWith:    shareWith,
		Accepted:     1,
		MountNameEnc: mountNameEnc,
		AbsPathEnc:   absPathEnc,
	}
	if err := s.Store.Insert(ctx, sh); err != nil {
		return nil, err
	}
	if shareType == files.ShareTypeRemote {
		if err := s.notifyRemote(ctx, u, sh, np, origin); err != nil {
			if delErr := s.Store.Delete(ctx, sh.ID); delErr != nil {
				return nil, errors.Join(err, delErr)
			}
			return nil, err
		}
	}
	if shareType == files.ShareTypeUser || shareType == files.ShareTypeGroup {
		s.notifyShareCreated(ctx, u, sh, np)
		s.wrapShareKeys(ctx, sh)
	}
	return sh, nil
}

func (s *Service) notifyRemote(ctx context.Context, owner *users.User, sh *files.Share, plainPath, origin string) error {
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
		Name:         path.Base(plainPath),
		ProviderID:   strconv.FormatInt(sh.ID, 10),
		Owner:        ownerCloud,
		Sender:       ownerCloud,
		ResourceType: resType,
		Token:        sh.Token,
		Permissions:  sh.Permissions,
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
// (ADR-0082). plainPath is the plaintext owner path (the row's Path is
// ciphertext for scheme-1 owners, ADR-0104 phase 3a). For a scheme-1 owner
// (MountNameEnc set) the stored subject embeds the mount-name TOKEN under the
// share target's own key and params carry the "ncgoNameScheme" marker with
// the sealing key's UUID hex (ADR-0104 §9, phase 3b): a sharee cannot resolve
// ancestor names, so the rendered display name is the mount BASENAME, never
// the full path — a documented display change for encrypted trees. The OCS
// render (list and get) decrypts the token in the viewer's ctx and rebuilds
// the subject from the rich template. Scheme-0 rows stay today's exact
// plaintext rows. A
// notification failure never fails the share: it is Warn-logged and share
// creation continues; a token failure skips the notification entirely —
// never a plaintext fallback.
func (s *Service) notifyShareCreated(ctx context.Context, owner *users.User, sh *files.Share, plainPath string) {
	if s.Notifs == nil {
		return
	}
	objectID := shareNotifObjectID(sh.ID)
	sharerName := owner.DisplayName
	if sharerName == "" {
		sharerName = owner.UID
	}
	subjectName := plainPath
	keyUUIDHex := ""
	if s.NameCodec != nil && sh.MountNameEnc != "" {
		token, hexID, err := s.NameCodec.ShareSubjectMeta(ctx, sh.OwnerUserID, sh.Path)
		if err != nil {
			s.warn("sharing: share notification: subject token failed", slog.Int64("share", sh.ID), slog.Any("err", err))
			return
		}
		subjectName, keyUUIDHex = token, hexID
	}
	params := map[string]richParam{
		"share": {Type: "highlight", ID: objectID, Name: subjectName},
		"user":  {Type: "user", ID: owner.UID, Name: sharerName},
	}
	if keyUUIDHex != "" {
		params["ncgoNameScheme"] = richParam{Type: "ncgo", ID: keyUUIDHex, Name: "1"}
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
		subject = fmt.Sprintf("You received %s as a share by %s", subjectName, sharerName)
		template = "You received {share} as a share by {user}"
		recipients = append(recipients, sharee)
	case files.ShareTypeGroup:
		gid := sh.ShareWith
		groupName := gid
		if g, err := s.Users.GetGroupByGID(ctx, gid); err == nil && g.DisplayName != "" {
			groupName = g.DisplayName
		}
		params["group"] = richParam{Type: "user-group", ID: gid, Name: groupName}
		subject = fmt.Sprintf("You received %s to group %s as a share by %s", subjectName, gid, sharerName)
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

// wrapShareKeys wraps the share's file keys for its recipients after a
// grant (ADR-0098). Best-effort like the notification hooks: recipient rows
// are the phase-4 substrate, not a read-path dependency, so a wrap failure
// is Warn-logged and never fails the share.
func (s *Service) wrapShareKeys(ctx context.Context, sh *files.Share) {
	if s.Keys == nil {
		return
	}
	if err := s.Keys.WrapForShare(ctx, sh); err != nil {
		s.warn("sharing: share key wrap failed", slog.Int64("share", sh.ID), slog.Any("err", err))
	}
}

// unwrapShareKeys deletes the recipients' wrap rows after a revoke (owner
// unshare, lazy expiry, or the expire sweep — ADR-0098); best-effort like
// wrapShareKeys.
func (s *Service) unwrapShareKeys(ctx context.Context, sh *files.Share) {
	if s.Keys == nil {
		return
	}
	if err := s.Keys.UnwrapForShare(ctx, sh); err != nil {
		s.warn("sharing: share key unwrap failed", slog.Int64("share", sh.ID), slog.Any("err", err))
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
	// ADR-0104 phase 3a: shares.file_path is ciphertext for scheme-1 owners,
	// so the (plaintext) OCS path filter translates before the exact match. A
	// filter whose path no longer resolves has no live shares by construction
	// (deleting a subtree deletes its shares) — empty, not an error.
	if s.NameCodec != nil && pathFilter != "" {
		ct, _, err := s.NameCodec.ShareCipherPath(ctx, u.ID, pathFilter)
		if err != nil {
			if errors.Is(err, files.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		pathFilter = ct
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
	s.unwrapShareKeys(ctx, sh)
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

// lookupValidShare returns a non-expired share and its owner; the share-type
// filter is the caller's job.
func (s *Service) lookupValidShare(ctx context.Context, token string) (*files.Share, *users.User, error) {
	sh, err := s.Store.GetByToken(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	expired, err := s.expireIfNeeded(ctx, sh)
	if err != nil {
		return nil, nil, err
	}
	if expired {
		return nil, nil, files.ErrNotFound
	}
	owner, err := s.Users.GetByID(ctx, sh.OwnerUserID)
	if err != nil {
		return nil, nil, err
	}
	return sh, owner, nil
}

// LookupValid returns a non-expired public-link share and its owner.
func (s *Service) LookupValid(ctx context.Context, token string) (*files.Share, *users.User, error) {
	sh, owner, err := s.lookupValidShare(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if sh.ShareType != files.ShareTypeLink {
		return nil, nil, files.ErrNotFound
	}
	return sh, owner, nil
}

// LookupValidPublicDAV is the /public.php/webdav resolver: public-link shares
// AND OCM federated shares (shareType 6) — a remote server pulls share
// content with Basic(token, "") per the OCM webdav protocol our discovery
// advertises (ADR-0023). No other share type ever resolves here.
func (s *Service) LookupValidPublicDAV(ctx context.Context, token string) (*files.Share, *users.User, error) {
	sh, owner, err := s.lookupValidShare(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if sh.ShareType != files.ShareTypeLink && sh.ShareType != files.ShareTypeRemote {
		return nil, nil, files.ErrNotFound
	}
	return sh, owner, nil
}

func (s *Service) resolveWithPassword(sh *files.Share, owner *users.User, password string) (*files.Share, *users.User, error) {
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

func (s *Service) ResolvePublic(ctx context.Context, token, password string) (*files.Share, *users.User, error) {
	sh, owner, err := s.LookupValid(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	return s.resolveWithPassword(sh, owner, password)
}

// ResolvePublicDAV authenticates /public.php/webdav basic auth: link shares
// (password honored) and OCM remote shares — Create clears PasswordHash for
// non-link shares, so the remote's empty password passes by construction.
func (s *Service) ResolvePublicDAV(ctx context.Context, token, password string) (*files.Share, *users.User, error) {
	sh, owner, err := s.LookupValidPublicDAV(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	return s.resolveWithPassword(sh, owner, password)
}

func (s *Service) SharePayload(ctx context.Context, r *http.Request, sh *files.Share) (any, error) {
	owner, err := s.Users.GetByID(ctx, sh.OwnerUserID)
	if err != nil {
		return nil, err
	}
	// ADR-0104 phase 3a: the OCS payload speaks plaintext paths. Open the
	// sealed share metadata in the owner's ctx — the owner's own keys always
	// resolve for them, so a failure here is loud (no ciphertext on the wire,
	// no silent fallback).
	plainPath := sh.Path
	if s.NameCodec != nil && sh.AbsPathEnc != "" {
		plain, _, err := s.NameCodec.OpenShareMeta(ctx, sh.OwnerUserID, sh)
		if err != nil {
			return nil, err
		}
		plainPath = plain
	}
	ent, err := s.Files.Stat(ctx, owner.UID, plainPath)
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
	view := *sh
	view.Path = plainPath
	return shareMap(&view, owner.UID, display, mime, fileID, url, expiration, shareWithDisplay), nil
}

// ListIncoming implements files.IncomingLookup.
//
// ADR-0104 phase 3a: a scheme-1 owner's share row carries the ciphertext
// owner path plus the sealed mount metadata. Each such share is opened in
// the SHAREE's ctx — the sharee resolves the share root's key through their
// own wrap row (master-wrapped owners resolve in any ctx), so the returned
// mount carries the plaintext OwnerPath (for storage-key derivation), the
// OwnerCipherPath (for share-root-anchored name resolution), and the
// decrypted Mount basename. An enrolled sharee without an unlocked session
// surfaces ErrKeyLocked — the ADR-0101 boundary, loud by design; plaintext
// shares pass through byte-identically.
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
		sh := &items[i]
		ownerPath, mount := sh.Path, "/"+pathBase(sh.Path)
		ownerCipher := ""
		if s.NameCodec != nil && sh.AbsPathEnc != "" {
			plain, mountName, err := s.NameCodec.OpenShareMeta(ctx, sh.OwnerUserID, sh)
			if err != nil {
				return nil, err
			}
			ownerPath, mount, ownerCipher = plain, "/"+mountName, sh.Path
		}
		out = append(out, files.IncomingMount{
			OwnerUID:        owner.UID,
			OwnerPath:       ownerPath,
			OwnerCipherPath: ownerCipher,
			Mount:           mount,
			Permissions:     sh.Permissions,
			ItemType:        sh.ItemType,
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

// ResealShareMeta refreshes the sealed share metadata (mount_name_enc,
// abs_path_enc) after a rename/move rewrote the shares' ciphertext file_path
// prefix (ADR-0104 phase 3a): every share row at or below the renamed root
// gets its sealed absolute path string-rewritten to the new prefix and
// re-sealed under the (moved) target's key — a rename does not change
// directory keys (ADR-0104 §5), so the old seals still open — and a share
// whose OWN root was renamed gets its mount basename re-sealed too (the
// basename changed; re-sealing an unchanged basename is a deterministic
// no-op). Runs in the renamer's ctx (the owner — their keys always resolve
// for them). Best-effort by contract: the DAV move path calls this after
// Shares.RenamePath and Warn-logs a failure, mirroring the KeySharer hooks.
// No-op without a NameCodec or for scheme-0 owners (plaintext rows carry no
// sealed fields).
func (s *Service) ResealShareMeta(ctx context.Context, ownerUserID int64, srcPlain, dstPlain string) error {
	if s.NameCodec == nil {
		return nil
	}
	ms, ok := s.Store.(shareMetaStore)
	if !ok {
		return fmt.Errorf("sharing: reseal: store does not expose the share-meta seam")
	}
	src, err := files.NormalizePath(srcPlain)
	if err != nil {
		return err
	}
	dst, err := files.NormalizePath(dstPlain)
	if err != nil {
		return err
	}
	ctDst, encrypted, err := s.NameCodec.ShareCipherPath(ctx, ownerUserID, dst)
	if err != nil {
		return err
	}
	if !encrypted {
		return nil
	}
	rows, err := ms.ListByPrefix(ctx, ownerUserID, ctDst)
	if err != nil {
		return err
	}
	var errs []error
	for i := range rows {
		sh := &rows[i]
		if sh.AbsPathEnc == "" {
			continue // plaintext row (defensive: a ciphertext prefix never matches one)
		}
		oldAbs, _, err := s.NameCodec.OpenShareMeta(ctx, ownerUserID, sh)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var newAbs string
		switch {
		case oldAbs == src:
			newAbs = dst
		case strings.HasPrefix(oldAbs, src+"/"):
			newAbs = dst + strings.TrimPrefix(oldAbs, src)
		default:
			// The row was selected by the new ciphertext prefix, so its
			// sealed path must carry the old plaintext prefix; a mismatch is
			// an inconsistent row — collect it, never silently rewrite.
			errs = append(errs, fmt.Errorf("sharing: reseal: share %d sealed path %q is not under renamed %q", sh.ID, oldAbs, src))
			continue
		}
		_, mountEnc, absEnc, err := s.NameCodec.SealShareMeta(ctx, ownerUserID, newAbs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := ms.UpdateEncFields(ctx, sh.ID, mountEnc, absEnc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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
