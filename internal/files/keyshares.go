package files

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// KeyWrapper wraps and unwraps per-user file keys for share recipients
// (ADR-0098). *encrypt.SQLResolver satisfies it; the narrow interface keeps
// the files package free of an encrypt dependency.
type KeyWrapper interface {
	// WrapKeyFor wraps the file key named by keyUUID for uid, lazily
	// creating the recipient's user key; re-wrapping is a no-op.
	WrapKeyFor(ctx context.Context, keyUUID [16]byte, uid string) error
	// UnwrapKeyFor deletes uid's wrap row for keyUUID; a missing row is a
	// no-op and the file's owner is never unwrapped.
	UnwrapKeyFor(ctx context.Context, keyUUID [16]byte, uid string) error
	// ReWrapSharees carries oldUUID's non-owner recipient wraps to newUUID
	// and deletes the old rows (overwrite continuity).
	ReWrapSharees(ctx context.Context, oldUUID, newUUID [16]byte) error
}

// KeyShareMeta is the filecache seam KeySharer needs; *SQLStore satisfies
// it.
type KeyShareMeta interface {
	GetByPath(ctx context.Context, userID int64, p string) (*File, error)
	ListSealedSubtree(ctx context.Context, userID int64, p string) ([]File, error)
}

// ShareKeyLookup is the share-store seam KeySharer needs.
// *sharing.SQLShareStore satisfies it — files cannot import sharing (sharing
// imports files), so the seam is declared here structurally.
type ShareKeyLookup interface {
	// Covering returns the user/group shares of an owner whose target is
	// the path or an ancestor of it.
	Covering(ctx context.Context, ownerUserID int64, path string) ([]*Share, error)
	// ForGroup returns every share granted to a group.
	ForGroup(ctx context.Context, gid string) ([]*Share, error)
}

// GroupMemberLookup lists group member uids; users.Store satisfies it.
type GroupMemberLookup interface {
	GroupMembers(ctx context.Context, gid string, limit int) ([]string, error)
}

// KeySharer keeps file_keys wrap rows in step with shares (ADR-0098): share
// grants wrap the file key for each recipient, revokes unwrap, group
// membership changes wrap/unwrap per member, and the write path wraps
// covering shares and carries recipients across overwrites. Every method is
// best-effort by contract: it returns errors for the CALLER to log, and
// recipient rows are the phase-4 substrate plus an audit record — the
// server-side read path never depends on them, so a hook failure must never
// fail the share/write/membership operation that triggered it. Files
// without a v3 key UUID (plaintext, v1/v2) are skipped everywhere.
type KeySharer struct {
	Meta    KeyShareMeta
	Wrapper KeyWrapper
	Shares  ShareKeyLookup
	Users   GroupMemberLookup
	Logger  *slog.Logger // nil-ok
}

// WrapForShare wraps the share target's file keys for the share's
// recipients: the sharee for user shares, every current member for group
// shares. Link and OCM-remote shares have no wrap able recipient (no user
// key exists) and are skipped.
func (k *KeySharer) WrapForShare(ctx context.Context, sh *Share) error {
	if sh == nil {
		return nil
	}
	switch sh.ShareType {
	case ShareTypeUser:
		return k.wrapShareTarget(ctx, sh, sh.ShareWith, k.Wrapper.WrapKeyFor)
	case ShareTypeGroup:
		members, err := k.Users.GroupMembers(ctx, sh.ShareWith, 0)
		if err != nil {
			return err
		}
		var errs []error
		for _, uid := range members {
			if err := k.wrapShareTarget(ctx, sh, uid, k.Wrapper.WrapKeyFor); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	default:
		k.debug(ctx, "files: key share: share type has no wrappable recipient, skipping",
			slog.Int64("share", sh.ID), slog.Int("share_type", sh.ShareType))
		return nil
	}
}

// UnwrapForShare mirrors WrapForShare on revoke.
func (k *KeySharer) UnwrapForShare(ctx context.Context, sh *Share) error {
	if sh == nil {
		return nil
	}
	switch sh.ShareType {
	case ShareTypeUser:
		return k.wrapShareTarget(ctx, sh, sh.ShareWith, k.Wrapper.UnwrapKeyFor)
	case ShareTypeGroup:
		members, err := k.Users.GroupMembers(ctx, sh.ShareWith, 0)
		if err != nil {
			return err
		}
		var errs []error
		for _, uid := range members {
			if err := k.wrapShareTarget(ctx, sh, uid, k.Wrapper.UnwrapKeyFor); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	default:
		return nil
	}
}

// OnGroupMemberAdded wraps every share granted to gid for the joining
// member.
func (k *KeySharer) OnGroupMemberAdded(ctx context.Context, gid, uid string) error {
	return k.forGroupShares(ctx, gid, uid, k.Wrapper.WrapKeyFor)
}

// OnGroupMemberRemoved unwraps every share granted to gid for the leaving
// member.
func (k *KeySharer) OnGroupMemberRemoved(ctx context.Context, gid, uid string) error {
	return k.forGroupShares(ctx, gid, uid, k.Wrapper.UnwrapKeyFor)
}

func (k *KeySharer) forGroupShares(ctx context.Context, gid, uid string, op func(context.Context, [16]byte, string) error) error {
	shares, err := k.Shares.ForGroup(ctx, gid)
	if err != nil {
		return err
	}
	var errs []error
	for _, sh := range shares {
		if err := k.wrapShareTarget(ctx, sh, uid, op); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WrapForWrite wraps a freshly written file's key for every recipient of
// the shares covering it (the file's own share or an ancestor folder's).
// After wrapping, the covering set is re-read and recipients whose share
// vanished mid-write are unwrapped again — that closes the race against a
// concurrent unshare (ADR-0094 serializes same-path writes, but share
// deletes do not take that lock): the last actor to touch a row leaves it
// consistent with the share table.
func (k *KeySharer) WrapForWrite(ctx context.Context, ownerUserID int64, p string, keyUUID [16]byte) error {
	shares, err := k.Shares.Covering(ctx, ownerUserID, p)
	if err != nil {
		return err
	}
	if len(shares) == 0 {
		return nil
	}
	wrapped := map[string]bool{}
	var errs []error
	for _, sh := range shares {
		uids, err := k.shareeUIDs(ctx, sh)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, uid := range uids {
			if err := k.Wrapper.WrapKeyFor(ctx, keyUUID, uid); err != nil {
				errs = append(errs, err)
				continue
			}
			wrapped[uid] = true
		}
	}
	if len(wrapped) == 0 {
		return errors.Join(errs...)
	}
	// Reconciliation re-read: unwrap wrapped recipients no longer covered.
	live, err := k.coveringUIDs(ctx, ownerUserID, p)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for uid := range wrapped {
		if live[uid] {
			continue
		}
		if err := k.Wrapper.UnwrapKeyFor(ctx, keyUUID, uid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// coveringUIDs is the union of recipient uids over the shares covering a
// path — the set that legitimately holds a wrap of the path's key.
func (k *KeySharer) coveringUIDs(ctx context.Context, ownerUserID int64, p string) (map[string]bool, error) {
	shares, err := k.Shares.Covering(ctx, ownerUserID, p)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, sh := range shares {
		uids, err := k.shareeUIDs(ctx, sh)
		if err != nil {
			return nil, err
		}
		for _, uid := range uids {
			out[uid] = true
		}
	}
	return out, nil
}

// ReWrapForOverwrite carries a shared file's recipient wraps from the
// superseded key UUID to the fresh one an overwrite minted. A zero old UUID
// (the file had no v3 key) or an unchanged UUID is a no-op.
func (k *KeySharer) ReWrapForOverwrite(ctx context.Context, oldUUID, newUUID [16]byte) error {
	var zero [16]byte
	if oldUUID == zero || oldUUID == newUUID {
		return nil
	}
	return k.Wrapper.ReWrapSharees(ctx, oldUUID, newUUID)
}

// wrapShareTarget applies op (wrap or unwrap) for uid to every sealed file
// named by the share: the file itself, or the sealed subtree for a folder
// share. Per-file errors are collected, not fatal, so one bad row never
// blocks the rest; the wrapped/unwrapped count is debug-logged.
func (k *KeySharer) wrapShareTarget(ctx context.Context, sh *Share, uid string, op func(context.Context, [16]byte, string) error) error {
	if uid == "" {
		return nil
	}
	f, err := k.Meta.GetByPath(ctx, sh.OwnerUserID, sh.Path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The target is gone (deleted between share and hook): nothing
			// to wrap, and unwrap needs no row either.
			return nil
		}
		return err
	}
	if !f.IsDir {
		return k.opOnFile(ctx, f, uid, op)
	}
	subtree, err := k.Meta.ListSealedSubtree(ctx, sh.OwnerUserID, sh.Path)
	if err != nil {
		return err
	}
	var errs []error
	done := 0
	for i := range subtree {
		if err := k.opOnFile(ctx, &subtree[i], uid, op); err != nil {
			errs = append(errs, err)
			continue
		}
		done++
	}
	k.debug(ctx, "files: key share: subtree processed",
		slog.Int64("share", sh.ID), slog.String("path", sh.Path), slog.String("uid", uid),
		slog.Int("files", done), slog.Int("failed", len(errs)))
	return errors.Join(errs...)
}

// opOnFile applies op to a single file's key UUID; files without a v3 key
// (plaintext, v1/v2) are skipped.
func (k *KeySharer) opOnFile(ctx context.Context, f *File, uid string, op func(context.Context, [16]byte, string) error) error {
	if len(f.KeyUUID) != 16 {
		return nil
	}
	var keyUUID [16]byte
	copy(keyUUID[:], f.KeyUUID)
	if err := op(ctx, keyUUID, uid); err != nil {
		return fmt.Errorf("files: key share: %s: %w", f.Path, err)
	}
	return nil
}

// shareeUIDs expands a share to recipient uids: the sharee for user shares,
// every group member for group shares; nil for link/remote shares.
func (k *KeySharer) shareeUIDs(ctx context.Context, sh *Share) ([]string, error) {
	switch sh.ShareType {
	case ShareTypeUser:
		if sh.ShareWith == "" {
			return nil, nil
		}
		return []string{sh.ShareWith}, nil
	case ShareTypeGroup:
		return k.Users.GroupMembers(ctx, sh.ShareWith, 0)
	default:
		return nil, nil
	}
}

func (k *KeySharer) debug(ctx context.Context, msg string, args ...any) {
	if k.Logger != nil {
		k.Logger.DebugContext(ctx, msg, args...)
	}
}
