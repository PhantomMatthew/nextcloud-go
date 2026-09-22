package plugins

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// propNSPrefix is the per-plugin WebDAV namespace URI root: each plugin's
// props live under propNSPrefix + <plugin_id> with the local name being the
// part after ":" in the registered "prefix:local" name. This disambiguates
// same-named props across plugins (ADR-0046).
const propNSPrefix = "http://ncgo.local/ns/plugin/"

// propValueMax caps the getter's out buffer (host-allocated guest memory).
const propValueMax = 4096

// propNamespace returns the namespace URI of one plugin's WebDAV props.
func propNamespace(pluginID string) string { return propNSPrefix + pluginID }

// propLocalName returns the local part of a registered "prefix:local" name.
func propLocalName(name string) string {
	_, local, _ := strings.Cut(name, ":")
	return local
}

// attachedPlugin returns the attached (started) plugin with the given id, or
// nil when the plugin is not currently attached.
func (h *Host) attachedPlugin(id string) *Plugin {
	h.dispMu.RLock()
	defer h.dispMu.RUnlock()
	for p := range h.dispatch {
		if p.manifest.Plugin.ID == id {
			return p
		}
	}
	return nil
}

// PropProvider adapts registered plugin WebDAV properties to the files DAV:
// PROPFIND asks every attached plugin's getter for a live value; PROPPATCH
// routes writes to the plugin's setter. It implements webdav.LivePropProvider.
type PropProvider struct {
	host   *Host
	reg    *Registry
	logger *slog.Logger
}

// NewPropProvider returns a provider over the host's started plugins and the
// registry's persisted prop records.
func NewPropProvider(h *Host, reg *Registry, logger *slog.Logger) *PropProvider {
	return &PropProvider{host: h, reg: reg, logger: logger}
}

var _ webdav.LivePropProvider = (*PropProvider)(nil)

// PropsFor collects every attached plugin's live props for path. A getter
// error, trap, or missing plugin omits that prop — PROPFIND never fails
// because a plugin misbehaved.
func (pp *PropProvider) PropsFor(ctx context.Context, user, path string) []webdav.CustomProp {
	if pp == nil || pp.host == nil || pp.reg == nil {
		return nil
	}
	recs, err := pp.reg.AllProps(ctx)
	if err != nil {
		pp.log(ctx, slog.LevelWarn, "plugins: list webdav props failed", "", err)
		return nil
	}
	var out []webdav.CustomProp
	for _, rec := range recs {
		p := pp.host.attachedPlugin(rec.PluginID)
		if p == nil {
			continue
		}
		value, err := p.callPropGetter(withUserCall(ctx, user), rec.Getter, path)
		if err != nil {
			pp.log(ctx, slog.LevelDebug, "plugins: webdav prop getter failed", rec.PluginID, err)
			continue
		}
		out = append(out, webdav.CustomProp{
			NS:    propNamespace(rec.PluginID),
			Name:  propLocalName(rec.Name),
			Value: value,
		})
	}
	return out
}

// SetProp routes one PROPPATCH op to the owning plugin's setter.
// handled=false means (ns, name) is not a registered plugin prop; the caller
// falls through to its own behavior. A registered prop without a setter is
// read-only (403), a detached plugin answers 403 (the client can do nothing
// about it), and a guest-side failure answers 500. A remove op arrives as an
// empty value and is still delivered to the setter.
func (pp *PropProvider) SetProp(ctx context.Context, user, path, ns, name, value string) (bool, int) {
	if pp == nil || pp.host == nil || pp.reg == nil {
		return false, 0
	}
	pluginID, ok := strings.CutPrefix(ns, propNSPrefix)
	if !ok || pluginID == "" {
		return false, 0
	}
	recs, err := pp.reg.PropsForPlugin(ctx, pluginID)
	if err != nil {
		pp.log(ctx, slog.LevelWarn, "plugins: list webdav props failed", pluginID, err)
		return true, http.StatusInternalServerError
	}
	var rec *PropRecord
	for i := range recs {
		if propLocalName(recs[i].Name) == name {
			rec = &recs[i]
			break
		}
	}
	if rec == nil {
		return false, 0
	}
	if rec.Setter == "" {
		return true, http.StatusForbidden
	}
	p := pp.host.attachedPlugin(pluginID)
	if p == nil {
		pp.log(ctx, slog.LevelDebug, "plugins: webdav prop setter for detached plugin", pluginID, nil)
		return true, http.StatusForbidden
	}
	if err := p.invokeEntry(withUserCall(ctx, user), rec.Setter, []byte(path), []byte(value)); err != nil {
		pp.log(ctx, slog.LevelWarn, "plugins: webdav prop setter failed", pluginID, err)
		return true, http.StatusInternalServerError
	}
	return true, http.StatusOK
}

// withUserCall attaches the PROPFIND/PROPPATCH user as the plugin call
// identity (ctx_user_id) unless the context already carries call metadata.
func withUserCall(ctx context.Context, user string) context.Context {
	if _, ok := ctx.Value(ctxCall).(CallContext); ok {
		return ctx
	}
	return WithCallContext(ctx, CallContext{UserID: user})
}

func (pp *PropProvider) log(ctx context.Context, level slog.Level, msg, pluginID string, err error) {
	if pp.logger == nil {
		return
	}
	attrs := []any{slog.String("plugin.id", pluginID)}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	pp.logger.Log(ctx, level, msg, attrs...)
}

// callPropGetter invokes the guest's prop getter with the convention
// getter(path_ptr, path_len, out_ptr, out_max) -> i32: the guest writes the
// raw value into the host-allocated out buffer and returns its byte count
// (0 = empty value, negative = error code).
func (p *Plugin) callPropGetter(ctx context.Context, entry, path string) (string, error) {
	callCtx, cancel := context.WithTimeout(withCall(ctx, p, false), p.callTimeout())
	defer cancel()

	inst, release, err := p.manager.acquire(callCtx)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("%w: %s", ErrMissingExport, name)
	}

	pathBuf, err := guestWrite(callCtx, inst.mod, alloc, []byte(path))
	if err != nil {
		release(true)
		return "", err
	}
	res, err := alloc.Call(callCtx, uint64(propValueMax))
	if err != nil {
		release(true)
		return "", wrapTrap(err)
	}
	outPtr := int32(res[0]) //nolint:gosec // G115: guest pointer
	if outPtr == 0 {
		release(true)
		return "", errors.New("plugins: guest alloc returned 0")
	}

	results, callErr := fn.Call(callCtx,
		uint64(uint32(pathBuf.ptr)),  //nolint:gosec // G115: u32 bit pattern
		uint64(uint32(pathBuf.size)), //nolint:gosec // G115: bounded by maxPayloadArg
		uint64(uint32(outPtr)),       //nolint:gosec // G115: u32 bit pattern
		uint64(propValueMax),
	)

	// Free both buffers best-effort; a trapping free destroys the instance.
	freeErr := guestFree(callCtx, freeFn, pathBuf)
	if ferr := guestFree(callCtx, freeFn, guestBuf{ptr: outPtr, size: propValueMax}); ferr != nil && freeErr == nil {
		freeErr = ferr
	}
	switch {
	case callErr != nil:
		release(true)
		return "", wrapTrap(callErr)
	case freeErr != nil:
		release(true)
		return "", freeErr
	}
	release(false)

	if len(results) == 0 {
		return "", errors.New("plugins: prop getter returned no result")
	}
	n := int32(results[0]) //nolint:gosec // G115: plugin i32 return
	switch {
	case n < 0:
		return "", &PluginError{Code: n}
	case n == 0:
		return "", nil
	case n > propValueMax:
		return "", fmt.Errorf("plugins: prop getter overflow %d", n)
	}
	b, ok := inst.mod.Memory().Read(uint32(outPtr), uint32(n)) //nolint:gosec // G115: bounds checked above
	if !ok {
		return "", errors.New("plugins: guest memory read failed")
	}
	return string(b), nil
}
