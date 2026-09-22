package events

import (
	"context"
	"log/slog"
	"sync"
)

// Event is one bus message. Source is "host" for core-module emissions or
// "plugin:<id>" for plugin publishes. UserID, when set, names the user the
// event is about (e.g. the uploader for files.uploaded); subscribers may use
// it as their call-time user identity.
type Event struct {
	Topic   string
	Payload []byte
	Source  string
	UserID  string
}

// Handler consumes one event.
type Handler func(ctx context.Context, ev Event)

type subscription struct {
	id int
	h  Handler
}

// Bus is a synchronous fan-out bus. Handlers run in subscription order from a
// snapshot taken under the lock, so they may subscribe, unsubscribe, and
// publish re-entrantly without deadlocking.
type Bus struct {
	logger *slog.Logger
	mu     sync.RWMutex
	next   int
	subs   []subscription
}

// NewBus returns a bus that reports handler panics to logger (nil discards).
func NewBus(logger *slog.Logger) *Bus {
	return &Bus{logger: logger}
}

// Subscribe registers h and returns its unsubscribe function.
func (b *Bus) Subscribe(h Handler) (unsubscribe func()) {
	b.mu.Lock()
	b.next++
	id := b.next
	b.subs = append(b.subs, subscription{id: id, h: h})
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, s := range b.subs {
			if s.id == id {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				return
			}
		}
	}
}

// Publish invokes every subscribed handler synchronously in subscription
// order. With no subscribers it is a no-op. A panicking handler is logged and
// skipped; the remaining handlers still run.
func (b *Bus) Publish(ctx context.Context, ev Event) {
	b.mu.RLock()
	handlers := make([]Handler, 0, len(b.subs))
	for _, s := range b.subs {
		handlers = append(handlers, s.h)
	}
	b.mu.RUnlock()
	for _, h := range handlers {
		b.invoke(ctx, h, ev)
	}
}

func (b *Bus) invoke(ctx context.Context, h Handler, ev Event) {
	defer func() {
		if r := recover(); r != nil && b.logger != nil {
			b.logger.WarnContext(ctx, "events: handler panic",
				slog.String("topic", ev.Topic), slog.Any("panic", r))
		}
	}()
	h(ctx, ev)
}
