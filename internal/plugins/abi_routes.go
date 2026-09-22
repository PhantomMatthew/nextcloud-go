package plugins

import (
	"context"
	"regexp"
	"strings"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

var (
	// routeHandlerRe bounds handler_name to an identifier-ish export name.
	routeHandlerRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	routeMethods   = map[string]struct{}{
		"GET": {}, "HEAD": {}, "POST": {}, "PUT": {},
		"DELETE": {}, "PATCH": {}, "OPTIONS": {},
	}
)

// maxHandlerName bounds the handler_name argument (spec §6.3).
const maxHandlerName = 128

// routeRegister implements ncgo.route_register (kind "route").
func (h *Host) routeRegister(ctx context.Context, mod api.Module, methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen int32) int32 {
	return h.registerRoute(ctx, mod, "route", methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen)
}

// ocsRegister implements ncgo.ocs_register (kind "ocs").
func (h *Host) ocsRegister(ctx context.Context, mod api.Module, methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen int32) int32 {
	return h.registerRoute(ctx, mod, "ocs", methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen)
}

// registerRoute persists one route record. Registration is lifecycle-hook
// only (spec §6.3), needs a routes.register/ocs.register grant covering the
// path, and the path must live under the plugin's own /apps/<id>/ namespace.
func (h *Host) registerRoute(ctx context.Context, mod api.Module, kind string, methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen int32) int32 {
	method, code := readString(mod, methodPtr, methodLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	routePath, code := readString(mod, pathPtr, pathLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	handler, code := readString(mod, handlerPtr, handlerLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}

	info := callFromCtx(ctx)
	if !info.inHook {
		return pluginsdk.ErrCodePermissionDenied
	}
	caps := pluginCaps(ctx)
	granted := caps.canRegisterRoute(routePath)
	if kind == "ocs" {
		granted = caps.canRegisterOCS(routePath)
	}
	if !granted || info.plugin == nil {
		return pluginsdk.ErrCodePermissionDenied
	}
	pluginID := info.plugin.manifest.Plugin.ID
	if !strings.HasPrefix(routePath, "/apps/"+pluginID+"/") {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if _, ok := routeMethods[method]; !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if handler == "" || len(handler) > maxHandlerName || !routeHandlerRe.MatchString(handler) {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if h.cfg.Registry == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	if err := h.cfg.Registry.UpsertRoute(ctx, &RouteRecord{
		PluginID:    pluginID,
		Kind:        kind,
		Method:      method,
		Path:        routePath,
		HandlerName: handler,
	}); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}
