package plugins

import (
	"context"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// This file holds the host functions whose backing subsystems are not yet
// implemented. Each validates the relevant capability so the default-deny
// posture is exercised now, then returns ErrUnsupported until its increment
// lands.

func (h *Host) webdavRegisterProp(ctx context.Context, mod api.Module, namePtr, nameLen, _, _, _, _ int32) int32 {
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return unsupportedGranted(pluginCaps(ctx).canProvideProp(name))
}
