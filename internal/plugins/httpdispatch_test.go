package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func dispatchManifest(id string) *Manifest {
	m := probeManifest()
	m.Plugin.ID = id
	m.EntryPoints.OnRequest = "ncgo_on_request"
	return m
}

func loadDispatch(t *testing.T, h *Host, id string, wasm []byte) *Plugin {
	t.Helper()
	p, err := h.Load(context.Background(), dispatchManifest(id), wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

func authedRequest(t *testing.T, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, strings.NewReader(body))
	ctx := auth.WithUser(req.Context(), &auth.Principal{UID: "alice"})
	return req.WithContext(httpx.WithRequestID(ctx, "req-1"))
}

func TestDispatchRoute(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe",
		wasmgen.RouteModule(200, []string{"X-Test: yes", "Content-Length: 999", "broken-line"}, "hello-body"))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api?x=1", "req-body"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, log %q", w.Code, buf.String())
	}
	if got := w.Header().Get("X-Test"); got != "yes" {
		t.Fatalf("X-Test = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got == "999" {
		t.Fatalf("plugin Content-Length must be ignored, got %q", got)
	}
	if got := w.Body.String(); got != "hello-body" {
		t.Fatalf("body = %q", got)
	}
	out := buf.String()
	for _, want := range []string{"method", "GET", "/apps/com.example.probe/api", "req-body"} {
		if !strings.Contains(out, want) {
			t.Fatalf("guest did not see %q: %q", want, out)
		}
	}
	if !strings.Contains(out, "malformed response header line") {
		t.Fatalf("malformed header not logged: %q", out)
	}
}

func TestDispatchBodyTooLarge(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, "x"))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "POST", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/apps/com.example.probe/api", strings.NewReader(strings.Repeat("x", maxPayloadArg+1)))
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", w.Code)
	}
	if strings.Contains(buf.String(), "/apps/com.example.probe/api") {
		t.Fatalf("plugin invoked for oversized body: %q", buf.String())
	}
}

func TestDispatchGuestError(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteFailModule(-7))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(buf.String(), "request dispatch failed") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDispatchTrap(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteTrapModule())
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchBodyReadFailure(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteBodyFailModule(200, "never-served", -5))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (body read failed before any write, must be 502)", w.Code)
	}
	if !strings.Contains(buf.String(), "plugin error -5") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDispatchOCSBodyReadFailure(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteBodyFailModule(200, "never-served", -5))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "ocs", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.OCSHandler(rec, ocs.V2).ServeHTTP(w, authedRequest(t, "/ocs/v2.php/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchInvalidStatus(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(99, nil, "x"))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchHeaderCountFailure(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe",
		wasmgen.RouteModuleOpts(wasmgen.RouteOpts{Status: 200, Body: "x", HeaderCount: -1}))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchBodyReadOverflow(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	// body_read claims more bytes than the host buffer holds.
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteBodyFailModule(200, "x", routeBodyChunk+1))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchCloseTrapStillResponds(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe",
		wasmgen.RouteModuleOpts(wasmgen.RouteOpts{Status: 200, Body: "body-ok", CloseTraps: true}))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusOK || w.Body.String() != "body-ok" {
		t.Fatalf("status = %d body = %q", w.Code, w.Body.String())
	}
	if !strings.Contains(buf.String(), "response close failed") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestDispatchStatusTrap(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe",
		wasmgen.RouteModuleOpts(wasmgen.RouteOpts{StatusTraps: true, Body: "x"}))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchHeaderAtFailure(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe",
		wasmgen.RouteModuleOpts(wasmgen.RouteOpts{Status: 200, Headers: []string{"X-Test: yes"}, Body: "x", HeaderAtCode: -3}))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchBodyReadError(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, "x"))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/apps/com.example.probe/api", nil)
	req.Body = errBody{}
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "POST", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

type errBody struct{}

func (errBody) Read([]byte) (int, error) { return 0, errors.New("boom") }
func (errBody) Close() error             { return nil }

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }

func TestStreamBodyWriteError(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, "hello"))
	pr, err := p.beginRequest(authedRequest(t, "/apps/com.example.probe/api", ""))
	if err != nil {
		t.Fatal(err)
	}
	defer pr.close()
	if err := pr.streamBody(errWriter{}); err == nil {
		t.Fatal("streamBody must fail on a failing writer")
	}
}

func TestDispatchNilLogger(t *testing.T) {
	h, err := NewHost(context.Background(), HostConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteFailModule(-7))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchMissingOnRequest(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	// No on_request entry point in the manifest and no such export.
	p, err := h.Load(context.Background(), probeManifest(), wasmgen.HelloModule("hi"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchMissingResponseExports(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteNoResponseModule())
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.RouteHandler(rec).ServeHTTP(w, authedRequest(t, "/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestDispatchOCS(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, `{"hello":"world","nested":{"arr":[1,{"x":2}]}}`))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "ocs", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}

	w := httptest.NewRecorder()
	p.OCSHandler(rec, ocs.V2).ServeHTTP(w, authedRequest(t, "/ocs/v2.php/apps/com.example.probe/api?format=json", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("v2 status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"statuscode":200`) || !strings.Contains(body, `"hello":"world"`) || !strings.Contains(body, `"arr":[1,{"x":2}]`) {
		t.Fatalf("v2 body = %q", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}

	w = httptest.NewRecorder()
	p.OCSHandler(rec, ocs.V1).ServeHTTP(w, authedRequest(t, "/ocs/v1.php/apps/com.example.probe/api?format=json", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("v1 status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"statuscode":100`) {
		t.Fatalf("v1 body = %q", w.Body.String())
	}
}

func TestDispatchOCSFailure(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(500, nil, `{"oops":true}`))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "ocs", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.OCSHandler(rec, ocs.V2).ServeHTTP(w, authedRequest(t, "/ocs/v2.php/apps/com.example.probe/api?format=json", ""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"status":"failure"`) || !strings.Contains(body, `"statuscode":996`) {
		t.Fatalf("body = %q", body)
	}
}

func TestDispatchOCSInvalidJSON(t *testing.T) {
	h, _ := testHost(t, HostConfig{})
	p := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, `not json`))
	rec := RouteRecord{PluginID: "com.example.probe", Kind: "ocs", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}
	w := httptest.NewRecorder()
	p.OCSHandler(rec, ocs.V2).ServeHTTP(w, authedRequest(t, "/ocs/v2.php/apps/com.example.probe/api", ""))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestMountRoutes(t *testing.T) {
	reg := NewRegistry(testDB(t))
	ctx := context.Background()
	h, _ := testHost(t, HostConfig{Registry: reg})
	pa := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, "from-a"))
	pb := loadDispatch(t, h, "com.example.other", wasmgen.RouteModule(200, nil, `{"b":1}`))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "handleApi"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.other", Kind: "ocs", Method: "GET", Path: "/apps/com.example.other/api", HandlerName: "handleApi"}); err != nil {
		t.Fatal(err)
	}

	router := httpx.NewRouter()
	pass := func(next http.Handler) http.Handler { return next }
	if err := MountRoutes(ctx, router, []*Plugin{pa, pb}, reg,
		httpx.Middleware(pass), httpx.Middleware(pass), httpx.Middleware(pass), nil); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apps/com.example.probe/api", nil))
	if w.Code != http.StatusOK || w.Body.String() != "from-a" {
		t.Fatalf("route: %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ocs/v2.php/apps/com.example.other/api?format=json", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"statuscode":200`) || !strings.Contains(w.Body.String(), `"b":1`) {
		t.Fatalf("ocs v2: %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ocs/v1.php/apps/com.example.other/api?format=json", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"statuscode":100`) {
		t.Fatalf("ocs v1: %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apps/com.example.probe/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unregistered: %d", w.Code)
	}
}

func TestMountRoutesNilArgs(t *testing.T) {
	reg := NewRegistry(testDB(t))
	if err := MountRoutes(context.Background(), nil, nil, reg, nil, nil, nil, nil); err == nil {
		t.Fatal("nil router must fail")
	}
	if err := MountRoutes(context.Background(), httpx.NewRouter(), nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("nil registry must fail")
	}
}

func TestMountRoutesSkips(t *testing.T) {
	reg := NewRegistry(testDB(t))
	ctx := context.Background()
	h, buf := testHost(t, HostConfig{Registry: reg})
	pa := loadDispatch(t, h, "com.example.probe", wasmgen.RouteModule(200, nil, "from-a"))
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.probe", Kind: "bogus", Method: "GET", Path: "/apps/com.example.probe/bogus", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertRoute(ctx, &RouteRecord{PluginID: "com.example.probe", Kind: "route", Method: "GET", Path: "/apps/com.example.probe/api", HandlerName: "h"}); err != nil {
		t.Fatal(err)
	}

	// A nil plugin in the slice and an unknown kind are skipped with logs;
	// the valid route still mounts.
	router := httpx.NewRouter()
	if err := MountRoutes(ctx, router, []*Plugin{nil, pa}, reg, nil, nil, nil, h.logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "unknown route kind") {
		t.Fatalf("log %q", buf.String())
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequestWithContext(ctx, http.MethodGet, "/apps/com.example.probe/api", nil))
	if w.Code != http.StatusOK || w.Body.String() != "from-a" {
		t.Fatalf("route: %d %q", w.Code, w.Body.String())
	}

	// A registry whose DB is gone fails per-plugin, logged and skipped.
	dead := testDB(t)
	deadReg := NewRegistry(dead)
	if err := dead.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MountRoutes(ctx, httpx.NewRouter(), []*Plugin{pa}, deadReg, nil, nil, nil, h.logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "list routes failed") {
		t.Fatalf("log %q", buf.String())
	}
}
