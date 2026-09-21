package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tetratelabs/wazero"

	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
)

// HostConfig sizes the wazero runtime and wires host services.
type HostConfig struct {
	DefaultMemoryLimitMB int
	DefaultCallTimeout   time.Duration
	// Cache backs the cache_* host functions. Nil makes them return
	// ErrUnavailable.
	Cache cache.Cache
}

// Host is a wazero-backed plugin runtime.
type Host struct {
	rt     wazero.Runtime
	logger *slog.Logger
	cfg    HostConfig
}

// NewHost constructs a runtime with the full ncgo host module surface. WASI
// is not instantiated.
func NewHost(ctx context.Context, cfg HostConfig, logger *slog.Logger) (*Host, error) {
	if cfg.DefaultMemoryLimitMB <= 0 {
		cfg.DefaultMemoryLimitMB = 32
	}
	if cfg.DefaultCallTimeout <= 0 {
		cfg.DefaultCallTimeout = 5 * time.Second
	}
	mb := cfg.DefaultMemoryLimitMB
	if mb < 1 {
		mb = 32
	}
	if mb > 256 {
		mb = 256
	}
	pages := uint32(mb) * 16 // 1MiB = 16 pages of 64KiB
	if pages == 0 {
		pages = 1
	}
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(pages))
	h := &Host{rt: rt, logger: logger, cfg: cfg}
	if err := h.registerHostModule(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("plugins: host module: %w", err)
	}
	return h, nil
}

// Load compiles wasm and rejects forbidden imports / missing exports.
func (h *Host) Load(ctx context.Context, m *Manifest, wasm []byte) (*Plugin, error) {
	if m == nil {
		return nil, ErrManifestInvalid
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	compiled, err := h.rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("plugins: compile: %w", err)
	}
	for _, imp := range compiled.ImportedFunctions() {
		mod, _, isImport := imp.Import()
		if isImport && mod != "ncgo" {
			_ = compiled.Close(ctx)
			return nil, fmt.Errorf("%w: %s", ErrForbiddenImport, mod)
		}
	}
	for _, mem := range compiled.ImportedMemories() {
		mod, _, isImport := mem.Import()
		if isImport && mod != "ncgo" {
			_ = compiled.Close(ctx)
			return nil, fmt.Errorf("%w: %s", ErrForbiddenImport, mod)
		}
	}
	exps := compiled.ExportedFunctions()
	for _, name := range []string{"ncgo_abi_version", "ncgo_alloc", "ncgo_free"} {
		if _, ok := exps[name]; !ok {
			_ = compiled.Close(ctx)
			return nil, fmt.Errorf("%w: %s", ErrMissingExport, name)
		}
	}
	p := &Plugin{host: h, manifest: m, compiled: compiled}
	p.manager = newInstanceManager(h, m, compiled)
	return p, nil
}

// Close shuts the runtime.
func (h *Host) Close(ctx context.Context) error {
	if h == nil || h.rt == nil {
		return nil
	}
	return h.rt.Close(ctx)
}
