package plugins

import (
	"context"
	"log/slog"
	"regexp"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// Job-name policy: plugin-local names only (the namespacing prefix is added
// by the host), 1-128 bytes of [A-Za-z0-9_.-].
const (
	maxJobNameLen  = 128
	maxJobRunAhead = 10 * 365 * 24 * time.Hour
)

var jobNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// jobEnqueue schedules a plugin-local job: the row goes in under the
// plugin's namespaced adapter job (plugin.<id>) with a MessagePack envelope
// carrying the plugin-local name and payload verbatim. A run_at in the past
// (or unset) means now; beyond maxJobRunAhead is rejected. Enqueueing is
// rejected when the plugin declares no on_job entry point — the job could
// never be delivered.
func (h *Host) jobEnqueue(ctx context.Context, mod api.Module, namePtr, nameLen, payloadPtr, payloadLen int32, runAtUnixMS int64) int32 {
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if name == "" || len(name) > maxJobNameLen || !jobNameRe.MatchString(name) {
		return pluginsdk.ErrCodeInvalidArgument
	}
	payload, code := readBytes(mod, payloadPtr, payloadLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if !pluginCaps(ctx).canRegisterJobs() {
		return pluginsdk.ErrCodePermissionDenied
	}
	if h.cfg.Jobs == nil {
		return pluginsdk.ErrCodeUnavailable
	}
	info := callFromCtx(ctx)
	if info.plugin == nil || info.plugin.manifest.EntryPoints.OnJob == "" {
		return pluginsdk.ErrCodeInvalidArgument
	}
	now := time.Now().UTC()
	runAt := now
	if runAtUnixMS > 0 {
		runAt = time.UnixMilli(runAtUnixMS).UTC()
		if runAt.Before(now) {
			runAt = now
		} else if runAt.After(now.Add(maxJobRunAhead)) {
			return pluginsdk.ErrCodeInvalidArgument
		}
	}
	envelope, err := msgpack.Marshal(pluginJobEnvelope{Name: name, Payload: payload})
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	if err := h.cfg.Jobs.Enqueue(ctx, pluginJobName(info.plugin.manifest.Plugin.ID), envelope, runAt); err != nil {
		if h.logger != nil {
			h.logger.WarnContext(ctx, "plugins: job enqueue failed",
				slog.String("plugin.id", info.plugin.manifest.Plugin.ID),
				slog.String("job", name),
				slog.String("error", err.Error()))
		}
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}
