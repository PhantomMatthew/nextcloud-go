package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// defaultPluginSystemQuotaBytes is the fallback for
// HostConfig.PluginSystemQuotaBytes (ADR-0061). 1 GiB matches the per-file
// spool cap (defaultMaxSpoolBytes): a plugin can always hold one
// maximum-size file, while the tree cap closes the "unbounded file count"
// hole the per-file cap leaves open. Plugin trees hold config/cache-shaped
// data, not user content, so 1 GiB is generous headroom, and the operator
// knob (plugin.system_storage_quota_mb) covers plugins with legitimate
// larger footprints.
const defaultPluginSystemQuotaBytes int64 = 1 << 30

// systemTreeUsage returns the total byte size of the storage subtree at
// root (0 when the root does not exist yet) and the size of the file at
// target (0 when absent or a directory) — the two inputs to the
// system-scope quota arithmetic. storage.Storage.List is one level deep, so
// the walk recurses deleteTree-style under the same safety bounds. Plugin
// trees are quota-bounded and small, so recomputing per create is
// acceptable.
func systemTreeUsage(ctx context.Context, st storage.Storage, root, target string) (tree, old int64, err error) {
	entries := 0
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxStorageTreeDepth {
			return fmt.Errorf("plugins: storage quota: depth cap (%d) at %s", maxStorageTreeDepth, dir)
		}
		infos, err := st.List(ctx, dir)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("plugins: storage quota: list %s: %w", dir, err)
		}
		for _, fi := range infos {
			entries++
			if entries > maxStorageTreeEntries {
				return fmt.Errorf("plugins: storage quota: entry cap (%d)", maxStorageTreeEntries)
			}
			if fi.IsDir {
				if err := walk(fi.Path, depth+1); err != nil {
					return err
				}
			} else {
				tree += fi.Size
			}
		}
		return nil
	}
	if err := walk(root, 1); err != nil {
		return 0, 0, err
	}
	if info, err := st.Stat(ctx, target); err == nil {
		if !info.IsDir {
			old = info.Size
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return 0, 0, fmt.Errorf("plugins: storage quota: stat %s: %w", target, err)
	}
	return tree, old, nil
}

// warnStorageQuota logs a quota refusal naming the calling plugin (empty id
// when no call context is present — defensive; exported entries always
// carry one). The 4n/4o posture: warn log plus ErrCodeQuotaExceeded, no new
// metric family.
func (h *Host) warnStorageQuota(ctx context.Context, scope, p string, quota, usage, writeBytes int64) {
	if h.logger == nil {
		return
	}
	var pluginID string
	if info := callFromCtx(ctx); info.plugin != nil {
		pluginID = info.plugin.manifest.Plugin.ID
	}
	h.logger.WarnContext(ctx, "plugins: storage quota exceeded",
		slog.String("plugin", pluginID),
		slog.String("scope", scope),
		slog.String("path", p),
		slog.Int64("quota_bytes", quota),
		slog.Int64("usage_bytes", usage),
		slog.Int64("write_bytes", writeBytes))
}

// checkUserStorageQuota enforces the calling user's quota for a user-scope
// write of newBytes to t.path (ADR-0061). A nil user quota means unlimited.
// Overwrites pay only the delta: usage - oldSize + newBytes > quota refuses
// with ErrCodeQuotaExceeded; oldSize is 0 when the target does not exist.
// Lookup failures map through mapStorageErr — a quota *denial* is never
// confused with a backend error.
func (h *Host) checkUserStorageQuota(ctx context.Context, t storageTarget, newBytes int64) int32 {
	usage, quota, err := h.cfg.Files.Usage(ctx, t.user)
	if err != nil {
		return h.mapStorageErr(ctx, "quota_usage", err)
	}
	if quota == nil {
		return pluginsdk.ErrCodeOK
	}
	var old int64
	if ent, err := h.cfg.Files.Stat(ctx, t.user, t.path); err == nil {
		old = ent.Size
	} else if !errors.Is(err, webdav.ErrNotFound) {
		return h.mapStorageErr(ctx, "quota_stat", err)
	}
	if usage-old+newBytes > *quota {
		h.warnStorageQuota(ctx, "user", t.path, *quota, usage, newBytes)
		return pluginsdk.ErrCodeQuotaExceeded
	}
	return pluginsdk.ErrCodeOK
}
