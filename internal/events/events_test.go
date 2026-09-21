package events

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestBusFanOutOrder(t *testing.T) {
	b := NewBus(nil)
	var got []string
	for _, name := range []string{"first", "second", "third"} {
		b.Subscribe(func(_ context.Context, ev Event) {
			got = append(got, name+":"+ev.Topic)
		})
	}
	b.Publish(context.Background(), Event{Topic: "demo.x", Source: "host"})
	want := []string{"first:demo.x", "second:demo.x", "third:demo.x"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBusUnsubscribe(t *testing.T) {
	b := NewBus(nil)
	var n int
	unsub := b.Subscribe(func(context.Context, Event) { n++ })
	b.Publish(context.Background(), Event{Topic: "a"})
	unsub()
	b.Publish(context.Background(), Event{Topic: "a"})
	if n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
	// Unsubscribing twice is a no-op.
	unsub()
}

func TestBusNoSubscribers(t *testing.T) {
	b := NewBus(nil)
	b.Publish(context.Background(), Event{Topic: "a"})
}

func TestBusPanicRecovery(t *testing.T) {
	var buf bytes.Buffer
	b := NewBus(slog.New(slog.NewTextHandler(&buf, nil)))
	var ran bool
	b.Subscribe(func(context.Context, Event) { panic("boom") })
	b.Subscribe(func(context.Context, Event) { ran = true })
	b.Publish(context.Background(), Event{Topic: "demo.x"})
	if !ran {
		t.Fatal("second handler must run after a panicking one")
	}
	if !strings.Contains(buf.String(), "handler panic") || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("panic not logged: %q", buf.String())
	}
}

func TestBusPublishFromHandler(t *testing.T) {
	b := NewBus(nil)
	var got []string
	b.Subscribe(func(ctx context.Context, ev Event) {
		got = append(got, "h1:"+ev.Topic)
		if ev.Topic == "outer" {
			b.Publish(ctx, Event{Topic: "inner"})
		}
	})
	b.Subscribe(func(_ context.Context, ev Event) {
		got = append(got, "h2:"+ev.Topic)
	})
	b.Publish(context.Background(), Event{Topic: "outer"})
	// The nested publish runs synchronously inside h1.
	want := "h1:outer,h1:inner,h2:inner,h2:outer"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBusSubscribeFromHandler(t *testing.T) {
	b := NewBus(nil)
	var late int
	b.Subscribe(func(_ context.Context, ev Event) {
		if ev.Topic == "first" {
			b.Subscribe(func(context.Context, Event) { late++ })
		}
	})
	b.Publish(context.Background(), Event{Topic: "first"})
	if late != 0 {
		t.Fatal("handler subscribed mid-publish must not see the in-flight event")
	}
	b.Publish(context.Background(), Event{Topic: "second"})
	if late != 1 {
		t.Fatalf("late handler ran %d times, want 1", late)
	}
}
