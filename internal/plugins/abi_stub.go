package plugins

import (
	"context"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// This file holds the host functions whose backing subsystems are not yet
// implemented (storage proxying, outbound HTTP, event bus, job wiring,
// route mounting, plugin config store). Each validates the relevant
// capability so the default-deny posture is exercised now, then returns
// ErrUnsupported until its increment lands.

func (h *Host) configGet(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasConfigRead())
}

func (h *Host) configSet(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasConfigWrite())
}

func (h *Host) storageStat(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageRead())
}

func (h *Host) storageOpen(ctx context.Context, _ api.Module, _, _ int32) int64 {
	return int64(unsupportedGranted(pluginCaps(ctx).hasStorageRead()))
}

func (h *Host) storageCreate(ctx context.Context, _ api.Module, _, _ int32, _ int64) int64 {
	return int64(unsupportedGranted(pluginCaps(ctx).hasStorageWrite()))
}

func (h *Host) storageStreamRead(ctx context.Context, _ api.Module, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageRead())
}

func (h *Host) storageStreamWrite(ctx context.Context, _ api.Module, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageWrite())
}

func (h *Host) storageStreamClose(ctx context.Context, _ api.Module, _ int32) int32 {
	caps := pluginCaps(ctx)
	return unsupportedGranted(caps.hasStorageRead() || caps.hasStorageWrite())
}

func (h *Host) storageDelete(ctx context.Context, _ api.Module, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageWrite())
}

func (h *Host) storageList(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageRead())
}

func (h *Host) storageRename(ctx context.Context, _ api.Module, _, _, _, _ int32) int32 {
	return unsupportedGranted(pluginCaps(ctx).hasStorageWrite())
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

func (h *Host) eventPublish(ctx context.Context, mod api.Module, topicPtr, topicLen, _, _ int32) int32 {
	topic, code := readString(mod, topicPtr, topicLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canPublishEvent(topic))
}

func (h *Host) jobEnqueue(ctx context.Context, _ api.Module, _, _, _, _ int32, _ int64) int32 {
	return unsupportedGranted(pluginCaps(ctx).canRegisterJobs())
}

func (h *Host) routeRegister(ctx context.Context, mod api.Module, _, _, pathPtr, pathLen, _, _ int32) int32 {
	routePath, code := readString(mod, pathPtr, pathLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canRegisterRoute(routePath))
}

func (h *Host) ocsRegister(ctx context.Context, mod api.Module, _, _, pathPtr, pathLen, _, _ int32) int32 {
	routePath, code := readString(mod, pathPtr, pathLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canRegisterOCS(routePath))
}

func (h *Host) webdavRegisterProp(ctx context.Context, mod api.Module, namePtr, nameLen, _, _, _, _ int32) int32 {
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canProvideProp(name))
}
