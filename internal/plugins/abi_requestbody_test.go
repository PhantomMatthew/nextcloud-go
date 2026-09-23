package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

// loadBodyStream loads wasm with an opt-in request_body_stream manifest.
func loadBodyStream(t *testing.T, h *Host, wasm []byte) *Plugin {
	t.Helper()
	m := dispatchManifest("com.example.probe")
	m.Runtime.RequestBodyStream = true
	p, err := h.Load(context.Background(), m, wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func postBodyRoute(t *testing.T, p *Plugin, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: http.MethodPost, Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/apps/com.example.probe/api", bytes.NewReader(body))
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, req)
	return w
}

// requestBodyProbeResponse is the JSON body the RequestBodyModule probe
// serves: mode/bytes/head on the handle path, error otherwise.
type requestBodyProbeResponse struct {
	Mode  string `json:"mode"`
	Bytes int64  `json:"bytes"`
	Head  string `json:"head"`
	Error string `json:"error"`
}

func decodeProbeResponse(t *testing.T, w *httptest.ResponseRecorder) requestBodyProbeResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", w.Code, w.Body.String())
	}
	var resp requestBodyProbeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("probe body %q: %v", w.Body.String(), err)
	}
	if resp.Error != "" {
		t.Fatalf("probe saw no body_handle (legacy inline map?): %q", resp.Error)
	}
	return resp
}

func TestDispatchRequestBodyStreamLarge(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadBodyStream(t, h, wasmgen.RequestBodyModule())
	body := bytes.Repeat([]byte("abcdefgh"), (2<<20)/8) // 2 MiB, inline cap is 1 MiB
	resp := decodeProbeResponse(t, postBodyRoute(t, p, body))
	if resp.Mode != "handle" {
		t.Fatalf("mode = %q", resp.Mode)
	}
	if resp.Bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", resp.Bytes, len(body))
	}
	if resp.Head != string(body[:256]) {
		t.Fatalf("head = %q...", resp.Head[:32])
	}
}

func TestDispatchRequestBodyStreamSmall(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadBodyStream(t, h, wasmgen.RequestBodyModule())
	body := []byte(strings.Repeat("0123456789", 10)) // 100 B, JSON-safe
	resp := decodeProbeResponse(t, postBodyRoute(t, p, body))
	if resp.Bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", resp.Bytes, len(body))
	}
	if resp.Head != string(body) {
		t.Fatalf("head = %q, want full echo of %d bytes", resp.Head, len(body))
	}
}

func TestDispatchRequestBodyStreamEmpty(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadBodyStream(t, h, wasmgen.RequestBodyModule())
	resp := decodeProbeResponse(t, postBodyRoute(t, p, nil))
	if resp.Bytes != 0 || resp.Head != "" {
		t.Fatalf("bytes = %d head = %q", resp.Bytes, resp.Head)
	}
}

// The same probe module without the manifest opt-in keeps the legacy inline
// behavior: >1 MiB bodies are still refused with 413 before on_request runs.
func TestDispatchRequestBodyInlineStillCapped(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RequestBodyModule())
	w := postBodyRoute(t, p, bytes.Repeat([]byte("x"), 2<<20))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
}

// Host-level ABI boundaries for request_body_read/close: handle resolution
// and buf_max validation.
func TestRequestBodyABIBounds(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.HelloModule("hi"))
	ctx := context.Background()
	inst, release, err := p.manager.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release(false)
	tabs := h.handlesFor(inst.mod)
	if tabs == nil {
		t.Fatal("no handle table for instance")
	}

	if code := h.requestBodyRead(ctx, inst.mod, 42, 0, 16); code != ErrCodeNotFound {
		t.Fatalf("read missing handle = %d, want -4", code)
	}
	if code := h.requestBodyClose(ctx, inst.mod, 42); code != ErrCodeNotFound {
		t.Fatalf("close missing handle = %d, want -4", code)
	}

	// A stream handle of another type (here a storage stream stand-in) is
	// invalid for the request body functions.
	other, err := tabs.add(handleStream, errBody{})
	if err != nil {
		t.Fatal(err)
	}
	if code := h.requestBodyRead(ctx, inst.mod, other, 0, 16); code != ErrCodeInvalidArgument {
		t.Fatalf("read foreign stream handle = %d, want -2", code)
	}
	if code := h.requestBodyClose(ctx, inst.mod, other); code != ErrCodeInvalidArgument {
		t.Fatalf("close foreign stream handle = %d, want -2", code)
	}

	id, err := tabs.add(handleStream, &requestBodyHandle{body: strings.NewReader("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if code := h.requestBodyRead(ctx, inst.mod, id, 0, maxPayloadArg+1); code != ErrCodeTooLarge {
		t.Fatalf("read oversized buf_max = %d, want -11", code)
	}
	if code := h.requestBodyRead(ctx, inst.mod, id, 0, -1); code != ErrCodeTooLarge {
		t.Fatalf("read negative buf_max = %d, want -11", code)
	}

	// Positive path: bytes land in guest memory, EOF is 0, close removes the
	// handle and later reads answer -4.
	if code := h.requestBodyRead(ctx, inst.mod, id, 4096, 16); code != 2 {
		t.Fatalf("read = %d, want 2", code)
	}
	b, ok := inst.mod.Memory().Read(4096, 2)
	if !ok || string(b) != "hi" {
		t.Fatalf("guest memory = %q ok=%v", b, ok)
	}
	if code := h.requestBodyRead(ctx, inst.mod, id, 4096, 16); code != ErrCodeOK {
		t.Fatalf("read at EOF = %d, want 0", code)
	}
	if code := h.requestBodyClose(ctx, inst.mod, id); code != ErrCodeOK {
		t.Fatalf("close = %d, want 0", code)
	}
	if code := h.requestBodyRead(ctx, inst.mod, id, 4096, 16); code != ErrCodeNotFound {
		t.Fatalf("read closed handle = %d, want -4", code)
	}
}
