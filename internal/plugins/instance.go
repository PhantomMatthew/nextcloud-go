package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/observability"
)

// ErrHandleLimit is returned when an instance exceeds its open-handle budget.
var ErrHandleLimit = errors.New("plugins: handle limit reached")

// Handle budgets from the ABI spec §8. Since ADR-0068 the budgets are per
// plugin, shared across the plugin's live instances (see handleAggregate);
// the per-instance table keeps the same-value check, which can only fire
// first for a single-instance fill and keeps Host-less tables bounded.
const (
	maxStreamHandles = 64
	maxRowsHandles   = 16
	maxHTTPHandles   = 16
)

type handleKind int

const (
	handleStream handleKind = iota
	handleRows
	handleHTTP
)

var handleLimits = map[handleKind]int32{
	handleStream: maxStreamHandles,
	handleRows:   maxRowsHandles,
	handleHTTP:   maxHTTPHandles,
}

type handleEntry struct {
	kind handleKind
	val  any
}

// handleTable tracks opaque handles owned by one instance. Handles are
// per-instance: an id handed to one instance is meaningless to another.
// When agg is set, add/remove/closeAll also draw against the plugin's
// cross-instance aggregate budget (ADR-0068).
type handleTable struct {
	mu       sync.Mutex
	next     int32
	counts   map[handleKind]int32
	items    map[int32]handleEntry
	agg      *handleAggregate
	pluginID string
}

func newHandleTable() *handleTable {
	return &handleTable{
		next:   1,
		counts: make(map[handleKind]int32),
		items:  make(map[int32]handleEntry),
	}
}

// newSharedHandleTable builds a table whose adds also acquire a slot from
// the plugin's aggregate budget and whose removes/closeAll return it.
func newSharedHandleTable(agg *handleAggregate, pluginID string) *handleTable {
	t := newHandleTable()
	t.agg = agg
	t.pluginID = pluginID
	return t
}

func (t *handleTable) add(kind handleKind, val any) (int32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.counts[kind] >= handleLimits[kind] {
		return 0, fmt.Errorf("%w: kind %d", ErrHandleLimit, kind)
	}
	// The table check runs first so a table-full refusal never touches the
	// aggregate; an aggregate refusal here consumes nothing, so neither
	// failure path leaks a slot.
	if t.agg != nil && !t.agg.acquire(t.pluginID, kind) {
		return 0, fmt.Errorf("%w: kind %d: plugin aggregate", ErrHandleLimit, kind)
	}
	id := t.next
	t.next++
	t.items[id] = handleEntry{kind: kind, val: val}
	t.counts[kind]++
	return id, nil
}

func (t *handleTable) get(id int32, kind handleKind) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.items[id]
	if !ok || e.kind != kind {
		return nil, false
	}
	return e.val, true
}

// getAny returns the raw entry for id regardless of kind, for host functions
// that must distinguish "no such id" from "id of another handle type".
func (t *handleTable) getAny(id int32) (handleEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.items[id]
	return e, ok
}

func (t *handleTable) remove(id int32, kind handleKind) (any, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.items[id]
	if !ok || e.kind != kind {
		return nil, false
	}
	delete(t.items, id)
	t.counts[kind]--
	if t.agg != nil {
		t.agg.release(t.pluginID, kind)
	}
	return e.val, true
}

// closeAll releases every open handle: database rows are closed, open
// transactions are rolled back. Each entry also returns its aggregate slot,
// so a destroyed (trapped) instance leaves no count behind. Cleanup errors
// are joined and returned.
func (t *handleTable) closeAll() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var errs []error
	for id, e := range t.items {
		if tx, ok := e.val.(interface{ Rollback() error }); ok {
			if err := tx.Rollback(); err != nil {
				errs = append(errs, err)
			}
		} else if closer, ok := e.val.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		delete(t.items, id)
		if t.agg != nil {
			t.agg.release(t.pluginID, e.kind)
		}
	}
	t.counts = make(map[handleKind]int32)
	return errors.Join(errs...)
}

// instance is one instantiated module plus its per-instance state.
type instance struct {
	mod     api.Module
	handles *handleTable
	stdout  *stdioLogWriter
	stderr  *stdioLogWriter
}

// close releases leftover handles and closes the module.
func (in *instance) close(h *Host) {
	if err := in.handles.closeAll(); err != nil {
		h.logHandleCleanup(err)
	}
	h.unregisterHandles(in.mod)
	_ = in.mod.Close(context.Background())
	// No writes can arrive after module close; flush any partial stdio line.
	if in.stdout != nil {
		in.stdout.flush()
	}
	if in.stderr != nil {
		in.stderr.flush()
	}
}

// observeMemory records the instance's current linear-memory size into the
// per-plugin high-water gauge and soft-enforces the manifest's
// runtime.memory_limit_mb (ADR-0095): over the limit it reports true so the
// caller destroys the instance — enforcement is retrospective, the completed
// call's result is not failed. A nil metrics registry disables measurement
// entirely (zero overhead).
func (im *instanceManager) observeMemory(in *instance) (overLimit bool) {
	reg := im.host.cfg.Metrics
	if reg == nil {
		return false
	}
	size := int64(in.mod.Memory().Size())
	id := im.manifest.Plugin.ID
	reg.SetGaugeMax(observability.MetricPluginMemoryHighWaterBytes, size,
		observability.Label{Name: "plugin", Value: id})
	if limit := im.manifest.Runtime.MemoryLimitMB; limit > 0 && size > int64(limit)<<20 {
		reg.IncCounter(observability.MetricPluginMemoryLimitExceededTotal,
			observability.Label{Name: "plugin", Value: id})
		if im.host.logger != nil {
			im.host.logger.WarnContext(context.Background(), "plugins: memory limit exceeded — destroying instance",
				slog.String("plugin.id", id),
				slog.Int64("memory.bytes", size),
				slog.Int("memory_limit_mb", limit))
		}
		return true
	}
	return false
}

// instanceManager owns the instance lifecycle for one plugin according to its
// manifest's instance_model.
type instanceManager struct {
	host     *Host
	manifest *Manifest
	compiled wazero.CompiledModule

	seq      atomic.Uint64
	pool     chan *instance // pooled model
	single   *instance      // singleton model
	singleMu sync.Mutex
}

func newInstanceManager(host *Host, m *Manifest, compiled wazero.CompiledModule) *instanceManager {
	im := &instanceManager{host: host, manifest: m, compiled: compiled}
	if m.Runtime.InstanceModel == "pooled" {
		im.pool = make(chan *instance, m.Runtime.PoolSize)
	}
	return im
}

func (im *instanceManager) instantiate(ctx context.Context) (*instance, error) {
	// Instance names must be unique within the runtime (wazero rejects
	// duplicates); the plugin id alone is not enough for pooled instances.
	name := fmt.Sprintf("%s#%d", im.manifest.Plugin.ID, im.seq.Add(1))
	// Wire the WASI stdio fds into the host log stream (ADR-0093) before
	// instantiation so _initialize output is captured too.
	stdout := newStdioLogWriter(im.host.logger, im.manifest.Plugin.ID, im.manifest.Plugin.Version, "stdout")
	stderr := newStdioLogWriter(im.host.logger, im.manifest.Plugin.ID, im.manifest.Plugin.Version, "stderr")
	cfg := wazero.NewModuleConfig().WithName(name).WithStartFunctions().WithStdout(stdout).WithStderr(stderr)
	mod, err := im.host.rt.InstantiateModule(ctx, im.compiled, cfg)
	if err != nil {
		return nil, wrapTrap(err)
	}
	// Reactor-convention initialisation (ADR-0092): real compiler
	// toolchains (TinyGo, wasi-libc) export _initialize to set up runtime
	// state per instance; wasmgen probe modules do not. Call it once per
	// instance before any other export when present.
	if init := mod.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			_ = mod.Close(ctx)
			return nil, wrapTrap(err)
		}
	}
	inst := &instance{mod: mod, handles: newSharedHandleTable(im.host.handleAgg, im.manifest.Plugin.ID), stdout: stdout, stderr: stderr}
	im.host.registerHandles(mod, inst.handles)
	return inst, nil
}

// warm pre-instantiates the pool for the pooled model.
func (im *instanceManager) warm(ctx context.Context) error {
	if im.pool == nil {
		return nil
	}
	for i := 0; i < cap(im.pool); i++ {
		inst, err := im.instantiate(ctx)
		if err != nil {
			return err
		}
		im.pool <- inst
	}
	return nil
}

// acquire hands out an instance per the model. The returned release must be
// called with broken=true when the call trapped, which destroys the instance
// (spec: any trap destroys the instance).
func (im *instanceManager) acquire(ctx context.Context) (inst *instance, release func(broken bool), err error) {
	switch im.manifest.Runtime.InstanceModel {
	case "pooled":
		select {
		case inst := <-im.pool:
			return inst, func(broken bool) { im.releasePooled(ctx, inst, broken) }, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	case "singleton":
		im.singleMu.Lock()
		if im.single == nil {
			inst, err := im.instantiate(ctx)
			if err != nil {
				im.singleMu.Unlock()
				return nil, nil, err
			}
			im.single = inst
		}
		inst := im.single
		return inst, func(broken bool) {
			// A clean release over the memory limit destroys the singleton
			// exactly like a trap does (ADR-0095).
			if broken || im.observeMemory(inst) {
				inst.close(im.host)
				im.single = nil
			} else if err := inst.handles.closeAll(); err != nil {
				im.host.logHandleCleanup(err)
			}
			im.singleMu.Unlock()
		}, nil
	default: // per_request
		inst, err := im.instantiate(ctx)
		if err != nil {
			return nil, nil, err
		}
		return inst, func(bool) {
			im.observeMemory(inst)
			inst.close(im.host)
		}, nil
	}
}

func (im *instanceManager) releasePooled(_ context.Context, inst *instance, broken bool) {
	// Measure on every release: a clean release under the memory limit keeps
	// the instance for reuse; a trap or a breach destroys it (ADR-0095).
	if !broken && !im.observeMemory(inst) {
		// Clean release: close leftover handles (plugin forgot to), keep the
		// instance alive for reuse.
		if err := inst.handles.closeAll(); err != nil {
			im.host.logHandleCleanup(err)
		}
		im.pool <- inst
		return
	}
	inst.close(im.host)
	// Replenish with a fresh instance; use a detached context because the
	// call context that broke the instance is typically already expired.
	fresh, err := im.instantiate(context.Background())
	if err != nil {
		im.host.logger.WarnContext(context.Background(), "plugins: pool replenish failed",
			slog.String("plugin.id", im.manifest.Plugin.ID), slog.String("error", err.Error()))
		return
	}
	im.pool <- fresh
}

// closeAll releases every instance owned by the manager.
func (im *instanceManager) closeAll() {
	if im.pool != nil {
		close(im.pool)
		for inst := range im.pool {
			im.observeMemory(inst)
			inst.close(im.host)
		}
	}
	if im.single != nil {
		im.observeMemory(im.single)
		im.single.close(im.host)
		im.single = nil
	}
}
