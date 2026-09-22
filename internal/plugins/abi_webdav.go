package plugins

import (
	"context"
	"regexp"
	"strings"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

var (
	// propLocalRe bounds the local part of a prop name (after "prefix:").
	propLocalRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	// propFuncRe bounds getter/setter export names (identifier-ish).
	propFuncRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

// maxPropFuncName bounds the getter/setter name arguments (spec §6.3).
const maxPropFuncName = 128

// webdavRegisterProp implements ncgo.webdav_register_prop. Registration is
// lifecycle-hook only (spec §6.3), needs a webdav.props grant covering the
// name, and persists the getter/setter export names for the live-prop
// provider. An empty setter registers a read-only property.
func (h *Host) webdavRegisterProp(ctx context.Context, mod api.Module, namePtr, nameLen, getterPtr, getterLen, setterPtr, setterLen int32) int32 {
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	getter, code := readString(mod, getterPtr, getterLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	setter, code := readString(mod, setterPtr, setterLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}

	info := callFromCtx(ctx)
	if !info.inHook {
		return pluginsdk.ErrCodePermissionDenied
	}
	if !pluginCaps(ctx).canProvideProp(name) || info.plugin == nil {
		return pluginsdk.ErrCodePermissionDenied
	}
	prefix, local, found := strings.Cut(name, ":")
	if !found || prefix == "" || !propLocalRe.MatchString(local) {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if !validPropFunc(getter) || (setter != "" && !validPropFunc(setter)) {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if h.cfg.Registry == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	if err := h.cfg.Registry.UpsertProp(ctx, &PropRecord{
		PluginID: info.plugin.manifest.Plugin.ID,
		Name:     name,
		Getter:   getter,
		Setter:   setter,
	}); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

func validPropFunc(name string) bool {
	return name != "" && len(name) <= maxPropFuncName && propFuncRe.MatchString(name)
}
