package plugins

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

func helloManifest() *Manifest {
	return &Manifest{
		Plugin: PluginSection{
			ID:      "com.example.hello",
			Name:    "Hello",
			Version: "0.1.0",
			ABI:     abiV1,
		},
		Runtime:     RuntimeSection{InstanceModel: "per_request"},
		EntryPoints: EntryPointsSection{Module: "hello.wasm", OnInstall: "ncgo_on_install"},
	}
}

func TestHostHelloInstallLogs(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(ctx, HostConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, helloManifest(), wasmgen.HelloModule("hello from wasm"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "hello from wasm") {
		t.Fatalf("log %q", out)
	}
	if !strings.Contains(out, "plugin.id=com.example.hello") {
		t.Fatalf("missing id attr: %q", out)
	}
	if !strings.Contains(out, "plugin.version=0.1.0") {
		t.Fatalf("missing version attr: %q", out)
	}
}

func TestHostForbiddenImport(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	_, err = h.Load(ctx, helloManifest(), wasmgen.WASIModule())
	if !errors.Is(err, ErrForbiddenImport) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostMissingExport(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	_, err = h.Load(ctx, helloManifest(), wasmgen.NoExportsModule())
	if !errors.Is(err, ErrMissingExport) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostInstallTimeout(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{DefaultCallTimeout: 50 * time.Millisecond}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, helloManifest(), wasmgen.LoopModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	start := time.Now()
	err = p.Install(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTrap) {
		t.Fatalf("err = %v", err)
	}
	if elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("elapsed = %s", elapsed)
	}
}

func TestHostOOBLog(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, HostConfig{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	m := helloManifest()
	m.Plugin.ID = "com.example.oob"
	p, err := h.Load(ctx, m, wasmgen.OOBLogModule())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	err = p.Install(ctx)
	var pe *PluginError
	if !errors.As(err, &pe) || pe.Code != pluginsdk.ErrCodeInvalidArgument {
		t.Fatalf("err = %v", err)
	}
}
