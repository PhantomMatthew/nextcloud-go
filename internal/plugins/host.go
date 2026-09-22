package plugins

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/appconfig"
	"github.com/PhantomMatthew/nextcloud-go/internal/cache"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
)

// HostConfig sizes the wazero runtime and wires host services.
type HostConfig struct {
	DefaultMemoryLimitMB int
	DefaultCallTimeout   time.Duration
	// Cache backs the cache_* host functions. Nil makes them return
	// ErrUnavailable.
	Cache cache.Cache
	// DB backs the db_* host functions. Nil makes them return ErrUnavailable.
	DB database.DB
	// Bus backs event_publish and host-to-plugin event delivery. Nil makes
	// event_publish return ErrUnavailable and disables delivery.
	Bus *events.Bus
	// Registry backs route_register/ocs_register persistence. Nil makes them
	// return ErrUnavailable.
	Registry *Registry
	// Files backs user-scope storage_* calls (one-shot commits keep the
	// filecache consistent). Nil makes user-scope calls return
	// ErrUnavailable.
	Files *files.DAV
	// SystemStorage backs system-scope storage_* calls. Nil makes
	// system-scope calls return ErrUnavailable.
	SystemStorage storage.Storage
	// SystemPrefix namespaces system-scope paths:
	// <SystemPrefix>/<plugin_id>/<path> (Nextcloud's appdata_<instanceid>
	// convention).
	SystemPrefix string
	// MaxSpoolBytes caps user-scope write spools; <= 0 means 1 GiB.
	MaxSpoolBytes int64
	// HTTPClient backs the http_* host functions. Nil means a default client
	// with a 30s timeout; per-request redirects are always re-validated
	// against the calling plugin's allowlist on a shallow copy, never on the
	// shared client.
	HTTPClient *http.Client
	// AppConfig backs the config_* host functions. Nil makes them return
	// ErrUnavailable.
	AppConfig *appconfig.Store
}

// Host is a wazero-backed plugin runtime.
type Host struct {
	rt     wazero.Runtime
	logger *slog.Logger
	cfg    HostConfig

	// handleTabs maps a live module instance to its handle table so host
	// functions (which receive api.Module, not *instance) can find it.
	handleTabsMu sync.RWMutex
	handleTabs   map[api.Module]*handleTable

	// dispatch tracks started plugins eligible for event delivery.
	dispMu      sync.RWMutex
	dispatch    map[*Plugin]struct{}
	unsubEvents func()
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
	if cfg.MaxSpoolBytes <= 0 {
		cfg.MaxSpoolBytes = defaultMaxSpoolBytes
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: maxHTTPTimeout}
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
	h := &Host{rt: rt, logger: logger, cfg: cfg, handleTabs: make(map[api.Module]*handleTable), dispatch: make(map[*Plugin]struct{})}
	if cfg.Bus != nil {
		h.unsubEvents = cfg.Bus.Subscribe(h.dispatchEvent)
	}
	if err := h.registerHostModule(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("plugins: host module: %w", err)
	}
	return h, nil
}

// registerHandles associates an instance's handle table with its module.
func (h *Host) registerHandles(mod api.Module, t *handleTable) {
	h.handleTabsMu.Lock()
	defer h.handleTabsMu.Unlock()
	h.handleTabs[mod] = t
}

// unregisterHandles drops the association when an instance closes.
func (h *Host) unregisterHandles(mod api.Module) {
	h.handleTabsMu.Lock()
	defer h.handleTabsMu.Unlock()
	delete(h.handleTabs, mod)
}

// handlesFor returns the handle table for the calling module instance.
func (h *Host) handlesFor(mod api.Module) *handleTable {
	h.handleTabsMu.RLock()
	defer h.handleTabsMu.RUnlock()
	return h.handleTabs[mod]
}

// logHandleCleanup reports leftover-handle cleanup failures.
func (h *Host) logHandleCleanup(err error) {
	if h.logger != nil {
		h.logger.WarnContext(context.Background(), "plugins: handle cleanup failed",
			slog.String("error", err.Error()))
	}
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
	if h.unsubEvents != nil {
		h.unsubEvents()
		h.unsubEvents = nil
	}
	return h.rt.Close(ctx)
}
