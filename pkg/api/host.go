package api

import (
	"context"
	"io"
	"log/slog"
)

// ModuleHost is the runtime surface given to in-tree modules at Init.
type ModuleHost interface {
	Logger() *slog.Logger
}

// Module is an in-tree feature package loaded by the server.
type Module interface {
	ID() string
	Init(ctx context.Context, host ModuleHost) error
	Routes() []Route
}

// Manifest is the minimal plugin identity projection exposed through pkg/api.
type Manifest struct {
	ID      string
	Name    string
	Version string
	ABI     string
}

// Host is the plugin host contract for out-of-tree WASM plugins.
type Host interface {
	Install(ctx context.Context, archive io.Reader) (*Manifest, error)
	Uninstall(ctx context.Context, pluginID string) error
	Invoke(ctx context.Context, pluginID, function string, payload []byte) ([]byte, error)
	PublishEvent(ctx context.Context, topic string, payload []byte) error
}
