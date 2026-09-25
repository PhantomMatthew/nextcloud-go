package plugins

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func TestStdioLogWriterLineBuffering(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStdioLogWriter(logger, "com.example.x", "1.0.0", "stdout")

	// Partial writes join into one line; one write may carry several lines.
	if _, err := w.Write([]byte("hel")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("lo\nfirst\nsecond\r\n")); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"hello", "first", "second", "plugin.stdio=stdout", `plugin.id=com.example.x`, `plugin.version=1.0.0`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q: %q", want, out)
		}
	}
	if strings.Contains(out, "second\r") {
		t.Fatalf("carriage return not stripped: %q", out)
	}
	// "second" arrives in the same Write as "first": both flushed, nothing
	// buffered afterwards — a blank line is dropped.
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "msg="); got != 3 {
		t.Fatalf("records = %d, want 3 (blank line dropped): %q", got, buf.String())
	}
}

func TestStdioLogWriterLevels(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	out := newStdioLogWriter(logger, "p", "1", "stdout")
	errw := newStdioLogWriter(logger, "p", "1", "stderr")
	_, _ = out.Write([]byte("to-out\n"))
	_, _ = errw.Write([]byte("to-err\n"))
	s := buf.String()
	if !strings.Contains(s, "level=INFO msg=to-out") {
		t.Fatalf("stdout not INFO: %q", s)
	}
	if !strings.Contains(s, "level=WARN msg=to-err") {
		t.Fatalf("stderr not WARN: %q", s)
	}
}

func TestStdioLogWriterTruncation(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStdioLogWriter(logger, "p", "1", "stdout")

	// An unterminated stream longer than the cap is force-emitted at the cap.
	long := strings.Repeat("a", stdioLineCap+100)
	if _, err := w.Write([]byte(long)); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, strings.Repeat("a", stdioLineCap)+"…[truncated]") {
		t.Fatalf("forced flush missing truncation marker: %.80q", out)
	}
	// The remaining 100 bytes are still buffered; a newline completes them.
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "msg="+strings.Repeat("a", 100)) {
		t.Fatalf("remainder not emitted: %q", out)
	}

	// A single newline-terminated line over the cap is truncated too.
	buf.Reset()
	if _, err := w.Write([]byte(strings.Repeat("b", stdioLineCap+50) + "\n")); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, strings.Repeat("b", stdioLineCap)+"…[truncated]") {
		t.Fatalf("overlong line not truncated: %.80q", out)
	}
}

func TestStdioLogWriterFlushAndNilLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := newStdioLogWriter(logger, "p", "1", "stderr")
	if _, err := w.Write([]byte("tail-no-newline")); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("partial line emitted early: %q", buf.String())
	}
	w.flush()
	if out := buf.String(); !strings.Contains(out, "level=WARN msg=tail-no-newline") {
		t.Fatalf("flush lost: %q", out)
	}
	w.flush() // idempotent
	if got := strings.Count(buf.String(), "msg="); got != 1 {
		t.Fatalf("flush not idempotent: %q", buf.String())
	}

	// Nil logger discards without error and reports full consumption.
	nilw := newStdioLogWriter(nil, "p", "1", "stdout")
	n, err := nilw.Write([]byte("anything\n"))
	if err != nil || n != len("anything\n") {
		t.Fatalf("nil logger write = %d, %v", n, err)
	}
	nilw.flush()
}

// TestHostStdioRoutedToLog drives a real fd_write guest through Load+Install
// and asserts the bytes land in the host log stream (ADR-0093).
func TestHostStdioRoutedToLog(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(ctx, HostConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, helloManifest(), wasmgen.WASIFdWriteModule(1,
		[]byte("hello "), []byte("stdout\nsecond line\n"), []byte("unterminated-tail")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	// per_request: the instance closes after the install call, flushing the
	// unterminated tail.
	out := buf.String()
	for _, want := range []string{
		"level=INFO msg=\"hello stdout\"", "msg=\"second line\"", "msg=unterminated-tail",
		"plugin.id=com.example.hello", "plugin.stdio=stdout",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q: %q", want, out)
		}
	}
}

func TestHostStderrRoutedToWarn(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h, err := NewHost(ctx, HostConfig{}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	p, err := h.Load(ctx, helloManifest(), wasmgen.WASIFdWriteModule(2, []byte("boom\n")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN msg=boom") || !strings.Contains(out, "plugin.stdio=stderr") {
		t.Fatalf("stderr routing wrong: %q", out)
	}
}
