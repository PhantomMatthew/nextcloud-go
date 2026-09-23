package plugins

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// httpHandleReqBytes builds the MessagePack http_request payload carrying a
// body_handle reference instead of an inline body_bytes.
func httpHandleReqBytes(t *testing.T, method, rawURL string, bodyHandle int) []byte {
	t.Helper()
	raw, err := msgpack.Marshal(map[string]any{
		"method":      method,
		"url":         rawURL,
		"headers":     map[string]string{},
		"body_handle": bodyHandle,
		"timeout_ms":  int32(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// httpBodyPlugin loads a probe module with an http.outbound grant and
// returns the plugin plus a call context carrying its capabilities, so
// tests can drive the host functions directly.
func httpBodyPlugin(t *testing.T, h *Host, wasm []byte) (*Plugin, context.Context) {
	t.Helper()
	ctx := context.Background()
	p, err := h.Load(ctx, httpManifest("example.com"), wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p, withCall(ctx, p, false)
}

func requireDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("spool dir not empty: %v", names)
	}
}

// Host-level ABI boundaries for http_request_body_create/write/close:
// capability gating, handle resolution, buf_len bounds, and seal semantics.
func TestHTTPRequestBodyABIBounds(t *testing.T) {
	dir := t.TempDir()
	h, _ := testHost(t, HostConfig{SpoolDir: dir})
	p, callCtx := httpBodyPlugin(t, h, wasmgen.HelloModule("hi"))
	ctx := context.Background()
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tabs := h.handlesFor(inst.mod)
	if tabs == nil {
		t.Fatal("no handle table for instance")
	}

	// Creating a body without any http.outbound grant is denied.
	packed := h.httpRequestBodyCreate(context.Background(), inst.mod)
	if code := int32(packed >> 32); code != ErrCodePermissionDenied {
		t.Fatalf("create without grant = %d, want -3", code)
	}

	if code := h.httpRequestBodyWrite(callCtx, inst.mod, 42, 0, 16); code != ErrCodeNotFound {
		t.Fatalf("write missing handle = %d, want -4", code)
	}
	if code := h.httpRequestBodyClose(callCtx, inst.mod, 42); code != ErrCodeNotFound {
		t.Fatalf("close missing handle = %d, want -4", code)
	}

	// A stream handle of another type (here a storage stream stand-in) is
	// invalid for the request body functions.
	other, err := tabs.add(handleStream, errBody{})
	if err != nil {
		t.Fatal(err)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, other, 0, 16); code != ErrCodeInvalidArgument {
		t.Fatalf("write foreign stream handle = %d, want -2", code)
	}
	if code := h.httpRequestBodyClose(callCtx, inst.mod, other); code != ErrCodeInvalidArgument {
		t.Fatalf("close foreign stream handle = %d, want -2", code)
	}

	packed = h.httpRequestBodyCreate(callCtx, inst.mod)
	code, handle := int32(packed>>32), int32(packed)
	if code != ErrCodeOK || handle <= 0 {
		t.Fatalf("create = (%d, %d)", code, handle)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 0, -1); code != ErrCodeTooLarge {
		t.Fatalf("write negative buf_len = %d, want -11", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 0, maxPayloadArg+1); code != ErrCodeTooLarge {
		t.Fatalf("write oversized buf_len = %d, want -11", code)
	}

	// Seal semantics: double close and write-after-seal are -2; the handle
	// stays resolvable (close does not remove it).
	if code := h.httpRequestBodyClose(callCtx, inst.mod, handle); code != ErrCodeOK {
		t.Fatalf("close = %d, want 0", code)
	}
	if code := h.httpRequestBodyClose(callCtx, inst.mod, handle); code != ErrCodeInvalidArgument {
		t.Fatalf("double close = %d, want -2", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 0, 16); code != ErrCodeInvalidArgument {
		t.Fatalf("write sealed handle = %d, want -2", code)
	}

	// Releasing the instance disposes the never-consumed spool.
	release(false)
	requireDirEmpty(t, dir)
}

// The write that would push the spool total past HostConfig.MaxSpoolBytes
// fails with -11; a write landing exactly on the cap succeeds.
func TestHTTPRequestBodySpoolCap(t *testing.T) {
	dir := t.TempDir()
	h, _ := testHost(t, HostConfig{SpoolDir: dir, MaxSpoolBytes: 8})
	p, callCtx := httpBodyPlugin(t, h, wasmgen.HelloModule("hi"))
	ctx := context.Background()
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release(false)
	if ok := inst.mod.Memory().Write(4096, []byte("0123456789abcdef")); !ok {
		t.Fatal("guest memory write failed")
	}

	packed := h.httpRequestBodyCreate(callCtx, inst.mod)
	code, handle := int32(packed>>32), int32(packed)
	if code != ErrCodeOK {
		t.Fatalf("create = %d", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 4096, 5); code != 5 {
		t.Fatalf("write 5 = %d, want 5", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 4096, 4); code != ErrCodeTooLarge {
		t.Fatalf("crossing write = %d, want -11", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 4096, 3); code != 3 {
		t.Fatalf("exact-fit write = %d, want 3", code)
	}
}

// Consumption is destructive: an unsealed spool is refused (-2) and left in
// the table; a sealed one leaves the table, serves the spooled bytes, and its
// cleanup deletes the temp file.
func TestHTTPRequestBodyConsumeLifecycle(t *testing.T) {
	dir := t.TempDir()
	h, _ := testHost(t, HostConfig{SpoolDir: dir})
	p, callCtx := httpBodyPlugin(t, h, wasmgen.HelloModule("hi"))
	ctx := context.Background()
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release(false)
	if ok := inst.mod.Memory().Write(4096, []byte("abc")); !ok {
		t.Fatal("guest memory write failed")
	}

	packed := h.httpRequestBodyCreate(callCtx, inst.mod)
	code, handle := int32(packed>>32), int32(packed)
	if code != ErrCodeOK {
		t.Fatalf("create = %d", code)
	}
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 4096, 3); code != 3 {
		t.Fatalf("write = %d, want 3", code)
	}
	if _, _, _, c := h.consumeHTTPRequestBody(inst.mod, handle); c != ErrCodeInvalidArgument {
		t.Fatalf("consume unsealed = %d, want -2", c)
	}
	if _, _, _, c := h.consumeHTTPRequestBody(inst.mod, 42); c != ErrCodeNotFound {
		t.Fatalf("consume missing = %d, want -4", c)
	}
	if code := h.httpRequestBodyClose(callCtx, inst.mod, handle); code != ErrCodeOK {
		t.Fatalf("seal = %d", code)
	}

	f, written, cleanup, c := h.consumeHTTPRequestBody(inst.mod, handle)
	if c != ErrCodeOK {
		t.Fatalf("consume sealed = %d", c)
	}
	if written != 3 {
		t.Fatalf("written = %d, want 3", written)
	}
	name := f.Name()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "abc" {
		t.Fatalf("spool content = %q err %v", got, err)
	}
	// The consumed handle is gone from the table.
	if code := h.httpRequestBodyWrite(callCtx, inst.mod, handle, 4096, 1); code != ErrCodeNotFound {
		t.Fatalf("write consumed handle = %d, want -4", code)
	}
	cleanup()
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("spool file still exists after cleanup: %v", err)
	}
}

// body_handle and body_bytes in the same http_request map are refused with
// -2 before any target validation.
func TestHTTPOutboundRequestBodyMutex(t *testing.T) {
	h, _ := testHost(t, HostConfig{SpoolDir: t.TempDir()})
	p, callCtx := httpBodyPlugin(t, h, wasmgen.HelloModule("hi"))
	ctx := context.Background()
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release(false)
	raw, err := msgpack.Marshal(map[string]any{
		"method":      "POST",
		"url":         "http://example.com/",
		"headers":     map[string]string{},
		"body_bytes":  []byte("x"),
		"body_handle": 1,
		"timeout_ms":  int32(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok := inst.mod.Memory().Write(4096, raw); !ok {
		t.Fatal("guest memory write failed")
	}
	packed := h.httpRequest(callCtx, inst.mod, 4096, int32(len(raw)))
	if code := int32(packed >> 32); code != ErrCodeInvalidArgument {
		t.Fatalf("mutex http_request = %d, want -2", code)
	}
}

// End-to-end streamed upload: the guest writes 2 MiB (64 × 32 KiB) of the
// byte(i) = i % 251 pattern through the spool and POSTs it via body_handle —
// a size the inline body_bytes path (1 MiB cap) cannot carry. The server
// verifies the pattern and reports the byte count and Content-Length.
func TestHTTPOutboundBodyStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for i, b := range body {
			if int(b) != i%251 {
				_, _ = fmt.Fprintf(w, "stream-mismatch at %d", i)
				return
			}
		}
		_, _ = fmt.Fprintf(w, "stream-ok %d cl=%d", len(body), r.ContentLength)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{SpoolDir: dir})
	req := httpHandleReqBytes(t, "POST", srv.URL+"/up", 1)
	p, err := h.Load(ctx, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPBodyStreamModule(req, 64, 200, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_upload"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "create-ok", "write-ok", "seal-ok", "req-ok", "status-ok", "resp-ok", "done-ok")
	if !strings.Contains(out, "stream-ok 2097152 cl=2097152") {
		t.Fatalf("server verification missing from log: %q", out)
	}
	// A consumed spool is destroyed by the request itself.
	requireDirEmpty(t, dir)
}

// The egress guard applies to streamed bodies unchanged: a plugin without
// http.outbound_allow_private is refused (-3) dialing a loopback target, the
// server is never hit, and the consumed spool is still destroyed.
func TestHTTPOutboundBodyStreamEgressGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("never"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{SpoolDir: dir})
	m := probeManifest()
	m.Capabilities = Capabilities{HTTP: HTTPCapabilities{Outbound: []string{httpHostPort(srv)}}}
	req := httpHandleReqBytes(t, "POST", srv.URL+"/up", 1)
	p, err := h.Load(ctx, m, wasmgen.HTTPBodyStreamModule(req, 4, 0, ErrCodePermissionDenied))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_upload"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "create-ok", "write-ok", "seal-ok", "denied-ok")
	if !strings.Contains(out, "http_request blocked by private-IP egress guard") {
		t.Fatalf("missing egress-guard warn log: %q", out)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server hits = %d, want 0", got)
	}
	requireDirEmpty(t, dir)
}

// The per-plugin rate limit applies to streamed bodies unchanged: with a
// burst of one the second do_upload's http_request is throttled with -8. The
// rate check precedes consumption, so that spool is disposed by the instance
// release cleanup instead — still leaving nothing on disk.
func TestHTTPOutboundBodyStreamRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("rate-body"))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ctx := t.Context()
	h, buf := testHost(t, HostConfig{SpoolDir: dir, HTTPRatePerMinute: 1, HTTPRateBurst: 1})
	req := httpHandleReqBytes(t, "POST", srv.URL+"/up", 1)
	p, err := h.Load(ctx, httpManifest(httpHostPort(srv)),
		wasmgen.HTTPBodyStreamModule(req, 1, 200, ErrCodeQuotaExceeded))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(ctx) })
	if _, err := p.Call(ctx, "do_upload"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Call(ctx, "do_upload"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	requireLogMarkers(t, out, "create-ok", "write-ok", "seal-ok", "req-ok", "status-ok", "resp-ok", "done-ok", "denied-ok")
	if !strings.Contains(out, "http_request rate limit exceeded") {
		t.Fatalf("missing rate-limit warn log: %q", out)
	}
	requireDirEmpty(t, dir)
}
