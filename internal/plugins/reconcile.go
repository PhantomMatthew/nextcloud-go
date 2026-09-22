package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
)

// trackedPlugin is the reconciler's record of one running plugin: the
// instance to stop, the registry version it was started from (a version
// change means upgrade = stop + start), and the exact route keys it
// mounted. Removal works from this list because by the time an uninstall
// is reconciled the CLI has already deleted the registry's route rows.
type trackedPlugin struct {
	plugin  *Plugin
	version string
	routes  []routeKey
}

// Reconciler keeps the running plugin set in sync with the registry:
// install / enable / disable / upgrade performed through ncgo-cli take
// effect on a running server within one refresh interval, without a
// restart (ADR-0062). Sync is the unit of work; Run simply polls it.
//
// Per-plugin failures are isolated the startOne way (log and skip, retried
// on the next Sync); a registry read failure aborts the pass and keeps the
// current state until the next tick.
type Reconciler struct {
	host    *Host
	reg     *Registry
	router  *httpx.Router
	routeMw httpx.Middleware
	ocsV1Mw httpx.Middleware
	ocsV2Mw httpx.Middleware
	logger  *slog.Logger

	mu      sync.Mutex
	tracked map[string]*trackedPlugin
	closed  bool
}

// NewReconciler wires the reconciler. The middlewares are the ones plugin
// routes were always mounted with: routeMw wraps plain routes, the OCS
// pair wraps the /ocs/v1.php and /ocs/v2.php mounts of an OCS record. All
// arguments except the middlewares must be non-nil.
func NewReconciler(h *Host, reg *Registry, router *httpx.Router, routeMw, ocsV1Mw, ocsV2Mw httpx.Middleware, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		host: h, reg: reg, router: router,
		routeMw: routeMw, ocsV1Mw: ocsV1Mw, ocsV2Mw: ocsV2Mw,
		logger:  logger,
		tracked: make(map[string]*trackedPlugin),
	}
}

// Sync runs one idempotent reconcile pass: plugins enabled in the registry
// but not running are started and mounted, running plugins no longer
// enabled (or whose registry version changed) are unmounted and stopped.
// A registry read failure keeps the current state and returns the error so
// the next tick retries.
func (r *Reconciler) Sync(ctx context.Context) error {
	rows, err := r.reg.ListEnabled(ctx)
	if err != nil {
		return fmt.Errorf("plugins: reconcile: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	desired := make(map[string]string, len(rows)) // id -> version
	for _, row := range rows {
		desired[row.ID] = row.Version
	}
	for id, tr := range r.tracked {
		version, ok := desired[id]
		if !ok || version != tr.version {
			r.stopOne(ctx, id, tr)
		}
	}
	for i := range rows {
		row := &rows[i]
		if _, ok := r.tracked[row.ID]; ok {
			continue
		}
		r.startOne(ctx, row)
	}
	return nil
}

// Run polls Sync every interval until ctx is done; it blocks, so callers
// run it in a goroutine. interval <= 0 disables polling (the boot-time
// Sync the caller already performed still stands) and Run returns
// immediately.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A Sync failing because ctx was cancelled is shutdown, not a
			// reconcile problem — don't log it as one.
			if err := r.Sync(ctx); err != nil && ctx.Err() == nil && r.logger != nil {
				r.logger.WarnContext(ctx, "plugins: reconcile failed", slog.String("error", err.Error()))
			}
		}
	}
}

// Close stops every tracked plugin (routes first, then job adapter, then
// the plugin itself) and disables the reconciler. App.Close uses it; Sync
// becomes a no-op afterwards.
func (r *Reconciler) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	var err error
	for id, tr := range r.tracked {
		r.unmount(id, tr)
		if cerr := tr.plugin.Close(ctx); cerr != nil {
			err = errors.Join(err, cerr)
		}
		delete(r.tracked, id)
	}
	return err
}

// startOneLocked starts and mounts one plugin, recording it on success. A
// start failure is logged by startOne and simply not tracked, so the next
// Sync retries; a route-listing failure closes the just-started plugin
// (startOne already attached it for event delivery) so nothing
// half-running leaks between ticks.
func (r *Reconciler) startOne(ctx context.Context, row *RegistryRow) {
	p := startOne(ctx, r.host, row, r.logger)
	if p == nil {
		return
	}
	recs, err := r.reg.RoutesForPlugin(ctx, row.ID)
	if err != nil {
		if r.logger != nil {
			r.logger.ErrorContext(ctx, "plugins: list routes failed",
				slog.String("plugin.id", row.ID), slog.String("error", err.Error()))
		}
		if cerr := p.Close(ctx); cerr != nil && r.logger != nil {
			r.logger.WarnContext(ctx, "plugins: close failed",
				slog.String("plugin.id", row.ID), slog.String("error", cerr.Error()))
		}
		return
	}
	keys := mountRoutes(ctx, r.router, p, recs, r.routeMw, r.ocsV1Mw, r.ocsV2Mw, r.logger)
	r.tracked[row.ID] = &trackedPlugin{plugin: p, version: row.Version, routes: keys}
}

// stopOne unmounts and stops one tracked plugin.
func (r *Reconciler) stopOne(ctx context.Context, id string, tr *trackedPlugin) {
	r.unmount(id, tr)
	if err := tr.plugin.Close(ctx); err != nil && r.logger != nil {
		r.logger.WarnContext(ctx, "plugins: close failed",
			slog.String("plugin.id", id), slog.String("error", err.Error()))
	}
	delete(r.tracked, id)
	r.purgeCacheIfUninstalled(ctx, id)
}

// purgeCacheIfUninstalled drops the plugin's plugin:<id>: cache keys when the
// stop is an uninstall (the registry row is gone) rather than a disable (the
// row survives, and so does the plugin's state — matching 4j's
// jobs/appconfig/storage semantics). Without this, hot reload (ADR-0062)
// breaks 4m's "memory L1 residue dies with the restart" argument: a
// same-process uninstall+reinstall would read the previous generation's
// keys (ADR-0063). The CLI only purges Redis-backed caches (ADR-0058), so
// this is the only purge for memory deployments. A registry read failure
// skips the purge conservatively; a purge failure is logged and does not
// interrupt the stop — the keys are inert data.
func (r *Reconciler) purgeCacheIfUninstalled(ctx context.Context, id string) {
	if r.host.cfg.Cache == nil {
		return
	}
	_, err := r.reg.Get(ctx, id)
	switch {
	case err == nil:
		return // disabled or upgraded, not uninstalled
	case !errors.Is(err, ErrPluginNotFound):
		if r.logger != nil {
			r.logger.WarnContext(ctx, "plugins: reconcile registry check failed, cache purge skipped",
				slog.String("plugin.id", id), slog.String("error", err.Error()))
		}
		return
	}
	n, perr := r.host.cfg.Cache.DeleteByPrefix(ctx, cacheKeyPrefix(id))
	switch {
	case perr != nil && r.logger != nil:
		r.logger.WarnContext(ctx, "plugins: uninstall cache purge failed",
			slog.String("plugin.id", id), slog.String("error", perr.Error()))
	case perr == nil && n > 0 && r.logger != nil:
		r.logger.InfoContext(ctx, "plugins: uninstall purged cache keys",
			slog.String("plugin.id", id), slog.Int64("cache.deleted", n))
	}
}

// unmount removes the plugin's tracked routes and unregisters its job
// adapter. Routes come off first so no new request dispatches into a
// plugin that is about to close; requests already dispatched run to
// completion against the still-open plugin. Unregister frees the
// plugin.<id> job name for the next start (Register would otherwise hit
// ErrDuplicateJob and leave the dead adapter holding the old *Plugin);
// rows queued while the plugin is down are retried and dropped by the
// runner's unknown-job three-strikes rule.
func (r *Reconciler) unmount(id string, tr *trackedPlugin) {
	for _, k := range tr.routes {
		r.router.Remove(k.method, k.path)
	}
	if r.host.cfg.Jobs != nil {
		r.host.cfg.Jobs.Unregister(pluginJobName(id))
	}
}
