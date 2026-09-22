package plugins

import (
	"context"
	"log/slog"
	"os"
)

// startOne loads and starts one enabled plugin from its registry row.
// Per-plugin failures are logged and skipped (nil return): a broken plugin
// must not prevent the server from booting or other plugins from
// starting — the reconciler retries on its next pass.
func startOne(ctx context.Context, h *Host, row *RegistryRow, logger *slog.Logger) *Plugin {
	fail := func(stage string, err error) *Plugin {
		logger.ErrorContext(ctx, "plugins: start failed",
			slog.String("plugin.id", row.ID), slog.String("stage", stage), slog.String("error", err.Error()))
		return nil
	}
	raw, err := os.ReadFile(row.ArchivePath)
	if err != nil {
		return fail("read", err)
	}
	a, err := ReadArchive(raw)
	if err != nil {
		return fail("archive", err)
	}
	p, err := h.Load(ctx, a.Manifest, a.Module)
	if err != nil {
		return fail("load", err)
	}
	if err := p.Start(ctx); err != nil {
		_ = p.Close(ctx)
		return fail("start", err)
	}
	h.attach(p)
	if err := h.registerPluginJob(p); err != nil {
		logger.ErrorContext(ctx, "plugins: job registration failed",
			slog.String("plugin.id", row.ID), slog.String("error", err.Error()))
	}
	return p
}
