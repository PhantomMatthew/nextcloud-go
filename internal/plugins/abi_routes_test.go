package plugins

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func routeCapsManifest() *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{
		Routes: RoutesCapabilities{Register: []string{"/apps/com.example.probe/*"}},
		OCS:    OCSCapabilities{Register: []string{"/apps/com.example.probe/*"}},
	}
	return m
}

func TestRouteRegisterPersists(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleApi"}}, ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.RoutesForPlugin(context.Background(), "com.example.probe")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("recs = %+v", recs)
	}
	rec := recs[0]
	if rec.Kind != "route" || rec.Method != http.MethodGet || rec.Path != "/apps/com.example.probe/api" || rec.HandlerName != "handleApi" {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestRouteRegisterUpsert(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{
			{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleV1"},
			{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleV2"},
		}, ErrCodeOK))
	if strings.Count(buf.String(), "probe-ok") != 2 {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.RoutesForPlugin(context.Background(), "com.example.probe")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].HandlerName != "handleV2" {
		t.Fatalf("recs = %+v", recs)
	}
}

func TestRouteRegisterOutsideHook(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	ctx := context.Background()
	p, err := h.Load(ctx, routeCapsManifest(), wasmgen.RouteRegModule(false, false,
		[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleApi"}}, ErrCodePermissionDenied))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if _, err := p.Call(ctx, "regprobe"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.RoutesForPlugin(ctx, "com.example.probe")
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
}

func TestRouteRegisterWrongPrefix(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	// The grant covers all of /apps/ so the capability check passes and the
	// per-plugin namespace check is what fires.
	m := probeManifest()
	m.Capabilities = Capabilities{Routes: RoutesCapabilities{Register: []string{"/apps/*"}}}
	installModule(t, h, m, wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{
			{Method: "GET", Path: "/apps/other.id/api", Handler: "handleApi"},
			{Method: "GET", Path: "/apps/com.example.probe", Handler: "handleApi"},
		}, ErrCodeInvalidArgument))
	if strings.Count(buf.String(), "probe-ok") != 2 {
		t.Fatalf("log %q", buf.String())
	}
}

func TestRouteRegisterBadMethod(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{
			{Method: "get", Path: "/apps/com.example.probe/api", Handler: "handleApi"},
			{Method: "FOO", Path: "/apps/com.example.probe/api", Handler: "handleApi"},
		}, ErrCodeInvalidArgument))
	if strings.Count(buf.String(), "probe-ok") != 2 {
		t.Fatalf("log %q", buf.String())
	}
}

func TestRouteRegisterBadHandler(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{
			{Method: "GET", Path: "/apps/com.example.probe/api", Handler: ""},
			{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "has space"},
			{Method: "GET", Path: "/apps/com.example.probe/api", Handler: strings.Repeat("a", 129)},
		}, ErrCodeInvalidArgument))
	if strings.Count(buf.String(), "probe-ok") != 3 {
		t.Fatalf("log %q", buf.String())
	}
}

func TestRouteRegisterNilRegistry(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(false, true,
		[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleApi"}}, ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestOCSRegisterPersists(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, routeCapsManifest(), wasmgen.RouteRegModule(true, true,
		[]wasmgen.RouteReg{{Method: "POST", Path: "/apps/com.example.probe/api/v1/things", Handler: "createThing"}}, ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.RoutesForPlugin(context.Background(), "com.example.probe")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Kind != "ocs" || recs[0].Method != http.MethodPost || recs[0].HandlerName != "createThing" {
		t.Fatalf("recs = %+v", recs)
	}
}

func TestOCSRegisterDenied(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	m := probeManifest() // no ocs.register grant
	m.Capabilities = Capabilities{Routes: RoutesCapabilities{Register: []string{"/apps/com.example.probe/*"}}}
	installModule(t, h, m, wasmgen.RouteRegModule(true, true,
		[]wasmgen.RouteReg{{Method: "GET", Path: "/apps/com.example.probe/api", Handler: "handleApi"}}, ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}
