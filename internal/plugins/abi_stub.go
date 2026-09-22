package plugins

import (
	"context"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// This file holds the host functions whose backing subsystems are not yet
// implemented (outbound HTTP, job wiring, plugin config store). Each
// validates the relevant capability so the default-deny posture is exercised
// now, then returns ErrUnsupported until its increment lands.

func (h *Host) configGet(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasConfigRead())
}

func (h *Host) configSet(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasConfigWrite())
}

func (h *Host) httpRequest(ctx context.Context, _ api.Module, _, _ int32) int64 {
	return int64(unsupportedGranted(pluginCaps(ctx).hasHTTPOutbound()))
}

func (h *Host) httpResponseStatus(ctx context.Context, _ api.Module, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasHTTPOutbound())
}

func (h *Host) httpResponseHeader(ctx context.Context, _ api.Module, _, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasHTTPOutbound())
}

func (h *Host) httpResponseBodyRead(ctx context.Context, _ api.Module, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasHTTPOutbound())
}

func (h *Host) httpResponseClose(ctx context.Context, _ api.Module, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasHTTPOutbound())
}

func (h *Host) jobEnqueue(ctx context.Context, _ api.Module, _, _, _, _ int32, _ int64) int32 {
	return unsupportedGranted(pluginCaps(ctx).canRegisterJobs())
}

func (h *Host) webdavRegisterProp(ctx context.Context, mod api.Module, namePtr, nameLen, _, _, _, _ int32) int32 {
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canProvideProp(name))
}
