package plugins

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
)

// stdioLineCap bounds one buffered stdio line. A guest writing an unterminated
// stream cannot grow the buffer past the cap: the first stdioLineCap bytes are
// emitted with a truncation marker and buffering resumes from what follows.
const stdioLineCap = 4096

// stdioLogWriter routes one plugin instance's WASI stdout/stderr fd into the
// host log stream (ADR-0093, closing the ADR-0092 follow-up). fd_write bytes
// are line-buffered and emitted as one slog record per line — stdout at Info,
// stderr at Warn — with plugin identity attached, mirroring the ncgo.log host
// function's attributes plus a plugin.stdio discriminator. A partial line is
// held until its newline arrives (possibly across calls of a long-lived
// instance) and flushed on instance close. Blank lines are dropped: they carry
// no information and fmt.Println-style guests produce them liberally.
//
// Guest calls are serialized per instance in every instance model, so Write is
// effectively single-threaded; the mutex only covers shutdown-time flushes
// racing an in-flight call. Writes cannot fail from the guest's perspective —
// fd_write always reports success so a logging hiccup can never trap a plugin.
type stdioLogWriter struct {
	mu     sync.Mutex
	logger *slog.Logger
	id     string
	ver    string
	stream string // "stdout" or "stderr"
	buf    []byte
}

func newStdioLogWriter(logger *slog.Logger, id, ver, stream string) *stdioLogWriter {
	return &stdioLogWriter{logger: logger, id: id, ver: ver, stream: stream}
}

func (w *stdioLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.logger == nil {
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	for len(w.buf) > 0 {
		i := bytes.IndexByte(w.buf, '\n')
		switch {
		case i >= 0:
			w.emit(w.buf[:i], false)
			w.buf = w.buf[i+1:]
		case len(w.buf) > stdioLineCap:
			w.emit(w.buf[:stdioLineCap], true)
			w.buf = w.buf[stdioLineCap:]
		default:
			return len(p), nil
		}
	}
	return len(p), nil
}

// flush emits any buffered partial line; called after the instance's module is
// closed, when no further writes can arrive.
func (w *stdioLogWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.logger == nil || len(w.buf) == 0 {
		return
	}
	w.emit(w.buf, false)
	w.buf = nil
}

func (w *stdioLogWriter) emit(line []byte, forced bool) {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	truncated := forced
	if len(line) > stdioLineCap {
		line = line[:stdioLineCap]
		truncated = true
	}
	if len(line) == 0 && !truncated {
		return
	}
	msg := string(line)
	if truncated {
		msg += "…[truncated]"
	}
	// The writer has no access to the in-flight call context; identity attrs
	// carry the attribution, as the pool-replenish log path does.
	level := slog.LevelInfo
	if w.stream == "stderr" {
		level = slog.LevelWarn
	}
	w.logger.LogAttrs(context.Background(), level, msg,
		slog.String("plugin.id", w.id),
		slog.String("plugin.version", w.ver),
		slog.String("plugin.stdio", w.stream),
	)
}
