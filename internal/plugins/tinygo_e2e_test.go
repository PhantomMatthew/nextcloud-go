package plugins

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTinyGoHelloPluginEndToEnd compiles examples/hello-plugin from source
// with the real TinyGo toolchain and runs its on_install entry point in the
// host. This is the only test that exercises the pluginsdk wasm bindings
// (the //go:build wasm files) as compiled by a real compiler — wasmgen probes
// cover the host side only (ADR-0092). Skipped when tinygo is not installed.
func TestTinyGoHelloPluginEndToEnd(t *testing.T) {
	tinygo, err := exec.LookPath("tinygo")
	if err != nil {
		t.Skip("tinygo not installed; skipping end-to-end plugin build")
	}
	ctx := context.Background()
	wasmOut := filepath.Join(t.TempDir(), "hello.wasm")
	// wasip1 reactor mode (ADR-0092): the module's asyncify scheduler is
	// self-contained (unlike wasm-unknown, whose scheduler requires host
	// stack-switching hooks wazero cannot express), and only a pinned WASI
	// import subset passes the load guard.
	build := exec.CommandContext(ctx, tinygo, "build", "-o", wasmOut,
		"-target=wasip1", "-buildmode=c-shared", "-no-debug", "./examples/hello-plugin")
	build.Dir = "../.." // module root, matching the Makefile invocation
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("tinygo build: %v\n%s", err, out)
	}
	wasm, err := os.ReadFile(wasmOut)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(ctx, HostConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, helloManifest(), wasm)
	if err != nil {
		t.Fatalf("load real tinygo module: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "hello from wasm") {
		t.Fatalf("expected plugin log line, got %q", out)
	}
}
