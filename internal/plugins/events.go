package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
)

// attach registers a started plugin for event delivery.
func (h *Host) attach(p *Plugin) {
	h.dispMu.Lock()
	defer h.dispMu.Unlock()
	h.dispatch[p] = struct{}{}
}

// detach unregisters a plugin from event delivery.
func (h *Host) detach(p *Plugin) {
	h.dispMu.Lock()
	defer h.dispMu.Unlock()
	delete(h.dispatch, p)
}

// isAttached reports whether p is currently registered for event delivery;
// the job adapter uses it to drop queued work for plugins that went away.
func (h *Host) isAttached(p *Plugin) bool {
	h.dispMu.RLock()
	defer h.dispMu.RUnlock()
	_, ok := h.dispatch[p]
	return ok
}

// dispatchEvent is the bus handler subscribed in NewHost: it fans one event
// out to every attached plugin whose manifest subscribes to the topic. The
// publishing plugin is skipped (self-skip prevents a singleton re-entrancy
// deadlock). Delivery failures are logged, never propagated.
func (h *Host) dispatchEvent(ctx context.Context, ev events.Event) {
	h.dispMu.RLock()
	plugins := make([]*Plugin, 0, len(h.dispatch))
	for p := range h.dispatch {
		plugins = append(plugins, p)
	}
	h.dispMu.RUnlock()
	for _, p := range plugins {
		if p.manifest.EntryPoints.OnEvent == "" {
			continue
		}
		if !p.manifest.Capabilities.canSubscribeEvent(ev.Topic) {
			continue
		}
		if ev.Source == "plugin:"+p.manifest.Plugin.ID {
			continue
		}
		// The event's user becomes the delivery's call identity unless the
		// publish context already carries explicit call metadata.
		deliverCtx := ctx
		if _, ok := ctx.Value(ctxCall).(CallContext); !ok && ev.UserID != "" {
			deliverCtx = WithCallContext(ctx, CallContext{UserID: ev.UserID})
		}
		if err := p.deliverEvent(deliverCtx, ev.Topic, ev.Payload); err != nil && h.logger != nil {
			h.logger.WarnContext(ctx, "plugins: event delivery failed",
				slog.String("plugin.id", p.manifest.Plugin.ID),
				slog.String("topic", ev.Topic),
				slog.String("error", err.Error()))
		}
	}
}

// guestBuf is one region allocated inside the guest's linear memory.
type guestBuf struct {
	ptr, size int32
}

// deliverEvent invokes the plugin's on_event entry point with topic and
// payload copied into guest memory (alloc/write/call/free).
func (p *Plugin) deliverEvent(ctx context.Context, topic string, payload []byte) error {
	return p.invokeEntry(ctx, p.manifest.EntryPoints.OnEvent, []byte(topic), payload)
}

// invokeEntry invokes entry with args copied into guest memory
// (alloc/write/call/free). A trap anywhere destroys the instance via
// release(true); a non-zero i32 result comes back as a *PluginError.
func (p *Plugin) invokeEntry(ctx context.Context, entry string, args ...[]byte) error {
	return p.invokeEntryMode(ctx, entry, false, args...)
}

// invokeHookEntry is invokeEntry in lifecycle-hook context: on_upgrade runs
// with inHook set so DDL and hook-only registrations are permitted, exactly
// like on_install.
func (p *Plugin) invokeHookEntry(ctx context.Context, entry string, args ...[]byte) error {
	return p.invokeEntryMode(ctx, entry, true, args...)
}

func (p *Plugin) invokeEntryMode(ctx context.Context, entry string, inHook bool, args ...[]byte) error {
	callCtx, cancel := context.WithTimeout(withCall(ctx, p, inHook), p.callTimeout())
	defer cancel()

	inst, release, err := p.manager.acquire(callCtx)
	if err != nil {
		return err
	}
	fn := inst.mod.ExportedFunction(entry)
	alloc := inst.mod.ExportedFunction("ncgo_alloc")
	freeFn := inst.mod.ExportedFunction("ncgo_free")
	if fn == nil || alloc == nil || freeFn == nil {
		release(false)
		name := entry
		if fn != nil {
			name = "ncgo_alloc/ncgo_free"
		}
		return fmt.Errorf("%w: %s", ErrMissingExport, name)
	}

	allocBuf := func(data []byte) (guestBuf, error) {
		if len(data) == 0 {
			return guestBuf{}, nil
		}
		res, err := alloc.Call(callCtx, uint64(len(data)))
		if err != nil {
			return guestBuf{}, wrapTrap(err)
		}
		ptr := int32(res[0]) //nolint:gosec // G115: guest pointer
		if ptr == 0 {
			return guestBuf{}, errors.New("plugins: guest alloc returned 0")
		}
		if !inst.mod.Memory().Write(uint32(ptr), data) { //nolint:gosec // G115: ptr checked positive above
			return guestBuf{}, errors.New("plugins: guest memory write failed")
		}
		return guestBuf{ptr: ptr, size: int32(len(data))}, nil //nolint:gosec // G115: bounded by maxPayloadArg
	}

	bufs := make([]guestBuf, len(args))
	for i, data := range args {
		bufs[i], err = allocBuf(data)
		if err != nil {
			release(true)
			return err
		}
	}

	// Guest ABI args are u32 bit patterns; widened to u64 for the call.
	callArgs := make([]uint64, 0, 2*len(bufs))
	for _, b := range bufs {
		callArgs = append(callArgs,
			uint64(uint32(b.ptr)),  //nolint:gosec // G115: u32 bit pattern
			uint64(uint32(b.size)), //nolint:gosec // G115: bounded by maxPayloadArg
		)
	}
	results, callErr := fn.Call(callCtx, callArgs...)

	// Free all buffers best-effort; a trapping free destroys the instance.
	var freeErr error
	for _, b := range bufs {
		if b.ptr == 0 {
			continue
		}
		if _, ferr := freeFn.Call(callCtx, uint64(uint32(b.ptr)), uint64(uint32(b.size))); ferr != nil && freeErr == nil { //nolint:gosec // G115: guest ABI args are u32 bit patterns
			freeErr = wrapTrap(ferr)
		}
	}

	switch {
	case callErr != nil:
		release(true)
		return wrapTrap(callErr)
	case freeErr != nil:
		release(true)
		return freeErr
	}
	release(false)
	if len(results) > 0 {
		if code := int32(results[0]); code != 0 { //nolint:gosec // G115: plugin i32 return
			return &PluginError{Code: code}
		}
	}
	return nil
}
