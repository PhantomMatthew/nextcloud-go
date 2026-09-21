package plugins

import (
	"context"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/events"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func eventManifest(id string, caps EventsCapabilities) *Manifest {
	m := probeManifest()
	m.Plugin.ID = id
	m.EntryPoints.OnEvent = "ncgo_on_event"
	m.Capabilities = Capabilities{Events: caps}
	return m
}

func attachModule(t *testing.T, h *Host, m *Manifest, wasm []byte) *Plugin {
	t.Helper()
	p, err := h.Load(context.Background(), m, wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	h.attach(p)
	return p
}

func TestEventDeliveredToSubscriber(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	attachModule(t, h, eventManifest("com.example.sub", EventsCapabilities{Subscribe: []string{"demo.*"}}),
		wasmgen.EventModule("", ""))
	bus.Publish(context.Background(), events.Event{Topic: "demo.hello", Payload: []byte("world"), Source: "host"})
	if !strings.Contains(buf.String(), "event demo.hello world") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventTopicMismatchNotDelivered(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	attachModule(t, h, eventManifest("com.example.sub", EventsCapabilities{Subscribe: []string{"other.*"}}),
		wasmgen.EventModule("", ""))
	bus.Publish(context.Background(), events.Event{Topic: "demo.hello", Payload: []byte("world"), Source: "host"})
	if strings.Contains(buf.String(), "event demo.hello") {
		t.Fatalf("non-matching topic delivered: %q", buf.String())
	}
}

func TestEventEmptyPayloadDelivered(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	attachModule(t, h, eventManifest("com.example.sub", EventsCapabilities{Subscribe: []string{"demo.*"}}),
		wasmgen.EventModule("", ""))
	bus.Publish(context.Background(), events.Event{Topic: "demo.empty", Source: "host"})
	if !strings.Contains(buf.String(), "event demo.empty ") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventPublishBetweenPlugins(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	ctx := context.Background()
	// A publishes and subscribes demo.*; the singleton model would deadlock
	// on re-entrant delivery, proving the self-skip.
	am := eventManifest("com.example.a", EventsCapabilities{Publish: []string{"demo.*"}, Subscribe: []string{"demo.*"}})
	am.Runtime.InstanceModel = "singleton"
	pa := attachModule(t, h, am, wasmgen.EventModule("demo.ping", "from-a"))
	attachModule(t, h, eventManifest("com.example.b", EventsCapabilities{Subscribe: []string{"demo.*"}}),
		wasmgen.EventModule("", ""))

	results, err := pa.Call(ctx, "do_publish")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || int32(results[0]) != ErrCodeOK {
		t.Fatalf("do_publish = %v", results)
	}
	var delivered []string
	for line := range strings.Lines(buf.String()) {
		if strings.Contains(line, "event demo.ping from-a") {
			delivered = append(delivered, line)
		}
	}
	if len(delivered) != 1 {
		t.Fatalf("delivered to %d plugins, want 1: %q", len(delivered), buf.String())
	}
	if !strings.Contains(delivered[0], "plugin.id=com.example.b") {
		t.Fatalf("receiver must be B (self-skip A): %q", delivered[0])
	}
}

func TestEventPublishCoreTopicDenied(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	m := probeManifest()
	m.Capabilities = Capabilities{Events: EventsCapabilities{Publish: []string{"*"}}}
	installModule(t, h, m, wasmgen.EventProbeModule("core.x", ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventPublishOKWithBus(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	m := probeManifest()
	m.Capabilities = Capabilities{Events: EventsCapabilities{Publish: []string{"demo.*"}}}
	installModule(t, h, m, wasmgen.EventProbeModule("demo.x", ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestEventDeliveryFailureIsolated(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	ctx := context.Background()
	pa := attachModule(t, h, eventManifest("com.example.a", EventsCapabilities{Publish: []string{"demo.*"}}),
		wasmgen.EventModule("demo.boom", "x"))
	// One subscriber returns a non-zero code, one traps; the healthy one must
	// still receive the event and the publisher must still get OK.
	fm := eventManifest("com.example.fail", EventsCapabilities{Subscribe: []string{"demo.*"}})
	attachModule(t, h, fm, wasmgen.EventFailListenerModule())
	tm := eventManifest("com.example.trap", EventsCapabilities{Subscribe: []string{"demo.*"}})
	attachModule(t, h, tm, wasmgen.EventTrapListenerModule())
	attachModule(t, h, eventManifest("com.example.good", EventsCapabilities{Subscribe: []string{"demo.*"}}),
		wasmgen.EventModule("", ""))

	results, err := pa.Call(ctx, "do_publish")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || int32(results[0]) != ErrCodeOK {
		t.Fatalf("do_publish = %v", results)
	}
	out := buf.String()
	if !strings.Contains(out, "event demo.boom x") {
		t.Fatalf("healthy subscriber missed the event: %q", out)
	}
	if !strings.Contains(out, "event delivery failed") {
		t.Fatalf("delivery failures not logged: %q", out)
	}
}

func TestEventDeliveryMissingEntryLogged(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	m := eventManifest("com.example.sub", EventsCapabilities{Subscribe: []string{"demo.*"}})
	m.EntryPoints.OnEvent = "ncgo_missing"
	attachModule(t, h, m, wasmgen.EventModule("", ""))
	bus.Publish(context.Background(), events.Event{Topic: "demo.hello", Source: "host"})
	out := buf.String()
	if !strings.Contains(out, "event delivery failed") || !strings.Contains(out, "ncgo_missing") {
		t.Fatalf("log %q", out)
	}
}

func TestEventDetachStopsDelivery(t *testing.T) {
	bus := events.NewBus(nil)
	h, buf := testHost(t, HostConfig{Bus: bus})
	attachModule(t, h, eventManifest("com.example.sub", EventsCapabilities{Subscribe: []string{"demo.*"}}),
		wasmgen.EventModule("", ""))
	bus.Publish(context.Background(), events.Event{Topic: "demo.one", Source: "host"})
	// Plugin.Close (run by cleanup would be too late) detaches immediately.
	for p := range h.dispatch {
		_ = p.Close(context.Background())
	}
	bus.Publish(context.Background(), events.Event{Topic: "demo.two", Source: "host"})
	out := buf.String()
	if !strings.Contains(out, "event demo.one") {
		t.Fatalf("first event missing: %q", out)
	}
	if strings.Contains(out, "event demo.two") {
		t.Fatalf("detached plugin still receives: %q", out)
	}
}
