package plugins

import (
	"context"
	"log/slog"
	"os"
)

// StartEnabled loads and starts every enabled plugin from the registry.
// Per-plugin failures are logged and skipped: a broken plugin must not
// prevent the server from booting.
func StartEnabled(ctx context.Context, h *Host, reg *Registry, logger *slog.Logger) []*Plugin {
	rows, err := reg.ListEnabled(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "plugins: list enabled failed", slog.String("error", err.Error()))
		return nil
	}
	var out []*Plugin
	for _, row := range rows {
		p := startOne(ctx, h, &row, logger)
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

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
	return p
}
