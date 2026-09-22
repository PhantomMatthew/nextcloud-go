package plugins

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/jobs"
)

// pluginJobName is the runner-visible job name for a plugin's adapter job:
// the spec's "job names auto-namespaced" rule realised as plugin.<id>.
func pluginJobName(pluginID string) string { return "plugin." + pluginID }

// pluginJobEnvelope is the MessagePack payload carried by every plugin job
// row: the plugin-local job name plus the opaque payload bytes the guest
// enqueued. The guest's on_job entry point receives name and payload
// verbatim (never the namespaced job name).
type pluginJobEnvelope struct {
	Name    string `msgpack:"name"`
	Payload []byte `msgpack:"payload"`
}

// pluginJob adapts one plugin's queued jobs to its on_job entry point. One
// adapter is registered per plugin at start; all of the plugin's jobs share
// the namespaced job name.
type pluginJob struct {
	host   *Host
	plugin *Plugin
}

func (j *pluginJob) Name() string { return pluginJobName(j.plugin.manifest.Plugin.ID) }

// Run delivers one queued job to the guest. Dropping (nil return) is the
// answer for work that can never succeed: the plugin was detached since
// enqueue, the envelope is a poison message, or the plugin has no on_job
// entry point. A failing delivery (trap or non-zero guest code) returns the
// error so the runner retries.
func (j *pluginJob) Run(ctx context.Context, payload []byte) error {
	p := j.plugin
	if !j.host.isAttached(p) {
		return nil
	}
	var env pluginJobEnvelope
	if err := msgpack.Unmarshal(payload, &env); err != nil {
		j.logWarn(ctx, "plugins: dropping malformed job envelope", err)
		return nil
	}
	entry := p.manifest.EntryPoints.OnJob
	if entry == "" {
		return nil
	}
	return p.invokeEntry(ctx, entry, []byte(env.Name), env.Payload)
}

func (j *pluginJob) logWarn(ctx context.Context, msg string, err error) {
	if j.host.logger != nil {
		j.host.logger.WarnContext(ctx, msg,
			slog.String("plugin.id", j.plugin.manifest.Plugin.ID),
			slog.String("error", err.Error()))
	}
}

// registerPluginJob registers the plugin's adapter job with the configured
// runner. It is a no-op without a runner, without the jobs.register
// capability, or without an on_job entry point (enqueue is rejected in that
// case, so nothing could ever be delivered). A duplicate registration
// (plugin restarted) is tolerated.
func (h *Host) registerPluginJob(p *Plugin) error {
	if h.cfg.Jobs == nil || !p.manifest.Capabilities.canRegisterJobs() || p.manifest.EntryPoints.OnJob == "" {
		return nil
	}
	err := h.cfg.Jobs.Register(&pluginJob{host: h, plugin: p})
	if errors.Is(err, jobs.ErrDuplicateJob) {
		return nil
	}
	return err
}
