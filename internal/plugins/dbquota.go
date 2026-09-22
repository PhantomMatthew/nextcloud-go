package plugins

import (
	"context"
	"log/slog"
)

// defaultDBMaxConcurrentPerPlugin is the fallback for
// HostConfig.DBMaxConcurrentPerPlugin (ADR-0060). The quota bounds statements
// in flight, not throughput: a slot is held only for the duration of one
// Query/Exec/Begin call, and the per-call wall-clock timeout
// (DefaultCallTimeout, host-capped at 30s) bounds how long that can be. 4
// lets one plugin overlap a small statement fan-out across its pooled
// instances without being able to monopolize the shared database/sql pool;
// sequential workloads of any length are unaffected, and sqlite deployments
// serialize writes anyway.
const defaultDBMaxConcurrentPerPlugin = 4

// acquireDBSlot takes one of the plugin's in-flight DB statement slots
// (ADR-0060) and returns the release function; ok is false when the plugin
// is already at DBMaxConcurrentPerPlugin in-flight statements. Take and
// release happen in the same critical section — there is no lock-free state.
func (h *Host) acquireDBSlot(pluginID string) (release func(), ok bool) {
	h.dbConcMu.Lock()
	defer h.dbConcMu.Unlock()
	if h.dbConc[pluginID] >= h.cfg.DBMaxConcurrentPerPlugin {
		return nil, false
	}
	h.dbConc[pluginID]++
	return func() { h.releaseDBSlot(pluginID) }, true
}

// releaseDBSlot returns one slot, deleting the map entry at zero so the map
// stays bounded by plugins with statements actually in flight.
func (h *Host) releaseDBSlot(pluginID string) {
	h.dbConcMu.Lock()
	defer h.dbConcMu.Unlock()
	if n := h.dbConc[pluginID] - 1; n > 0 {
		h.dbConc[pluginID] = n
	} else {
		delete(h.dbConc, pluginID)
	}
}

// acquireDBSlotCtx resolves the calling plugin's id and takes an in-flight
// statement slot. Exhaustion logs a warn naming the plugin (the 4n
// rate-limit posture) and reports ok = false; callers map that to
// ErrCodeQuotaExceeded.
func (h *Host) acquireDBSlotCtx(ctx context.Context) (release func(), ok bool) {
	var pluginID string
	if info := callFromCtx(ctx); info.plugin != nil {
		pluginID = info.plugin.manifest.Plugin.ID
	}
	release, ok = h.acquireDBSlot(pluginID)
	if !ok && h.logger != nil {
		h.logger.WarnContext(ctx, "plugins: db statement concurrency quota exceeded",
			slog.String("plugin", pluginID),
			slog.Int("max_concurrent_per_plugin", h.cfg.DBMaxConcurrentPerPlugin))
	}
	return release, ok
}
