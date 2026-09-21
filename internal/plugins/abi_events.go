package plugins

import (
	"context"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// eventPublish validates the events.publish grant and fans the event out on
// the host bus with Source "plugin:<id>" (the dispatcher skips the publisher
// on delivery). The payload is copied out of guest memory before publishing.
func (h *Host) eventPublish(ctx context.Context, mod api.Module, topicPtr, topicLen, payloadPtr, payloadLen int32) int32 {
	topic, code := readString(mod, topicPtr, topicLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	raw, code := readBytes(mod, payloadPtr, payloadLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if !pluginCaps(ctx).canPublishEvent(topic) {
		return pluginsdk.ErrCodePermissionDenied
	}
	if h.cfg.Bus == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	info := callFromCtx(ctx)
	source := "host"
	if info.plugin != nil {
		source = "plugin:" + info.plugin.manifest.Plugin.ID
	}
	h.cfg.Bus.Publish(ctx, events.Event{
		Topic:   topic,
		Payload: append([]byte(nil), raw...),
		Source:  source,
	})
	return pluginsdk.ErrCodeOK
}
