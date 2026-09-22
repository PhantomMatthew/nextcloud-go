package plugins

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// startPropPlugin installs the module (running the on_install hook, which
// registers the prop) and attaches the plugin for live delivery.
func startPropPlugin(t *testing.T, h *Host, m *Manifest, wasm []byte) {
	t.Helper()
	ctx := context.Background()
	p, err := h.Load(ctx, m, wasm)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	h.attach(p)
}

func propManifest(id string, props ...string) *Manifest {
	m := propCapsManifest(props...)
	m.Plugin.ID = id
	return m
}

func extraPropValue(e *webdav.Entry, ns string) (string, bool) {
	for _, p := range e.ExtraProps {
		if p.NS == ns {
			return p.Value, true
		}
	}
	return "", false
}

func TestPropProviderPropfind(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	fx.dav.LiveProps = NewPropProvider(h, reg, nil)
	startPropPlugin(t, h, propManifest("com.example.probe", "x:tags"),
		wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK))
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	ctx := context.Background()
	e, err := fx.dav.Stat(ctx, "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := extraPropValue(e, "http://ncgo.local/ns/plugin/com.example.probe")
	if !ok || v != "prop-value" {
		t.Fatalf("extra props = %+v", e.ExtraProps)
	}

	var buf bytes.Buffer
	webdav.WriteMultistatus(&buf, webdav.PropfindContext{BaseHref: "/remote.php/dav/files/alice/"}, []*webdav.Entry{e})
	out := buf.String()
	want := `<x:tags xmlns:x="http://ncgo.local/ns/plugin/com.example.probe">prop-value</x:tags>`
	if !strings.Contains(out, want) {
		t.Fatalf("multistatus missing %q\n%s", want, out)
	}
	entries, err := webdav.ParseMultistatus(strings.NewReader(out))
	if err != nil || len(entries) != 1 {
		t.Fatalf("parse = %+v %v", entries, err)
	}

	// List attaches the prop to children too.
	children, err := fx.dav.List(ctx, "alice", "/docs")
	if err != nil || len(children) != 1 {
		t.Fatalf("list = %+v %v", children, err)
	}
	if _, ok := extraPropValue(children[0], "http://ncgo.local/ns/plugin/com.example.probe"); !ok {
		t.Fatalf("list extra props = %+v", children[0].ExtraProps)
	}
}

func TestPropProviderGetterErrorOmitted(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	fx.dav.LiveProps = NewPropProvider(h, reg, nil)
	startPropPlugin(t, h, propManifest("com.example.probe", "x:tags"),
		wasmgen.PropModuleOpts(wasmgen.PropOpts{Name: "x:tags", Getter: "getTags", Want: ErrCodeOK, GetterCode: -5}))
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	e, err := fx.dav.Stat(context.Background(), "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.ExtraProps) != 0 {
		t.Fatalf("extra props = %+v", e.ExtraProps)
	}
	// The multistatus is still produced (207-able) without the prop.
	var buf bytes.Buffer
	webdav.WriteMultistatus(&buf, webdav.PropfindContext{BaseHref: "/remote.php/dav/files/alice/"}, []*webdav.Entry{e})
	if !strings.Contains(buf.String(), "<d:multistatus") {
		t.Fatalf("multistatus = %s", buf.String())
	}
}

func TestPropProviderTwoPluginsSameLocalName(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	fx.dav.LiveProps = NewPropProvider(h, reg, nil)
	startPropPlugin(t, h, propManifest("com.example.a", "x:tags"),
		wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK))
	startPropPlugin(t, h, propManifest("com.example.b", "y:tags"),
		wasmgen.PropModuleOpts(wasmgen.PropOpts{Name: "y:tags", Getter: "getTags", Setter: "setTags", Want: ErrCodeOK, Value: "b-value"}))
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	e, err := fx.dav.Stat(context.Background(), "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.ExtraProps) != 2 {
		t.Fatalf("extra props = %+v", e.ExtraProps)
	}
	va, ok := extraPropValue(e, "http://ncgo.local/ns/plugin/com.example.a")
	if !ok || va != "prop-value" {
		t.Fatalf("plugin a prop = %+v", e.ExtraProps)
	}
	vb, ok := extraPropValue(e, "http://ncgo.local/ns/plugin/com.example.b")
	if !ok || vb != "b-value" {
		t.Fatalf("plugin b prop = %+v", e.ExtraProps)
	}
	var buf bytes.Buffer
	webdav.WriteMultistatus(&buf, webdav.PropfindContext{BaseHref: "/remote.php/dav/files/alice/"}, []*webdav.Entry{e})
	out := buf.String()
	for _, want := range []string{
		`<x:tags xmlns:x="http://ncgo.local/ns/plugin/com.example.a">prop-value</x:tags>`,
		`<x:tags xmlns:x="http://ncgo.local/ns/plugin/com.example.b">b-value</x:tags>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("multistatus missing %q\n%s", want, out)
		}
	}
}

func TestPropProviderProppatch(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	fx.dav.LiveProps = NewPropProvider(h, reg, nil)
	startPropPlugin(t, h, propManifest("com.example.probe", "x:tags"),
		wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK))
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	ctx := context.Background()
	ns := "http://ncgo.local/ns/plugin/com.example.probe"
	results, err := fx.dav.PatchProps(ctx, "alice", "/docs/a.txt", []webdav.PropPatchOp{{
		Space: ns, Name: "tags", Value: "blue",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("patch = %+v %v", results, err)
	}
	if !strings.Contains(buf.String(), "setprop /docs/a.txt blue") {
		t.Fatalf("log %q", buf.String())
	}

	// A remove op is delivered to the setter as an empty value.
	results, err = fx.dav.PatchProps(ctx, "alice", "/docs/a.txt", []webdav.PropPatchOp{{
		Remove: true, Space: ns, Name: "tags",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusOK {
		t.Fatalf("remove = %+v %v", results, err)
	}
	if !strings.Contains(buf.String(), "setprop /docs/a.txt ") {
		t.Fatalf("log %q", buf.String())
	}

	// Unknown prop under a plugin namespace and unknown namespace both fall
	// through to the existing behavior (403: only oc:favorite is persisted).
	results, err = fx.dav.PatchProps(ctx, "alice", "/docs/a.txt", []webdav.PropPatchOp{
		{Space: ns, Name: "nope", Value: "1"},
		{Space: "http://example.com/ns", Name: "tags", Value: "1"},
	})
	if err != nil || len(results) != 2 {
		t.Fatalf("unknown = %+v %v", results, err)
	}
	for _, r := range results {
		if r.Status != http.StatusForbidden {
			t.Fatalf("unknown = %+v", results)
		}
	}
}

func TestPropProviderReadOnlyProppatch(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	fx.dav.LiveProps = NewPropProvider(h, reg, nil)
	startPropPlugin(t, h, propManifest("com.example.probe", "x:tags"),
		wasmgen.PropModule("x:tags", "getTags", "", ErrCodeOK))
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	results, err := fx.dav.PatchProps(context.Background(), "alice", "/docs/a.txt", []webdav.PropPatchOp{{
		Space: "http://ncgo.local/ns/plugin/com.example.probe", Name: "tags", Value: "blue",
	}})
	if err != nil || len(results) != 1 || results[0].Status != http.StatusForbidden {
		t.Fatalf("patch = %+v %v", results, err)
	}
}

func TestPropProviderDetachedPlugin(t *testing.T) {
	fx := newStorageFixture(t)
	reg := NewRegistry(testDB(t))
	h, _ := testHost(t, HostConfig{Registry: reg})
	pp := NewPropProvider(h, reg, nil)
	fx.dav.LiveProps = pp
	// Install (registering the prop) but never attach: the plugin is not live.
	ctx := context.Background()
	p, err := h.Load(ctx, propManifest("com.example.probe", "x:tags"),
		wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	if err := p.Install(ctx); err != nil {
		t.Fatal(err)
	}
	fx.mkdirAndWrite(t, "/docs", "/docs/a.txt", "hello")

	e, err := fx.dav.Stat(ctx, "alice", "/docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.ExtraProps) != 0 {
		t.Fatalf("extra props = %+v", e.ExtraProps)
	}
	handled, status := pp.SetProp(ctx, "alice", "/docs/a.txt",
		"http://ncgo.local/ns/plugin/com.example.probe", "tags", "blue")
	if !handled || status != http.StatusForbidden {
		t.Fatalf("setprop = %v %d", handled, status)
	}
}

func TestPropProviderNilGuards(t *testing.T) {
	var pp *PropProvider
	if got := pp.PropsFor(context.Background(), "alice", "/a"); got != nil {
		t.Fatalf("props = %+v", got)
	}
	if handled, _ := pp.SetProp(context.Background(), "alice", "/a", "ns", "n", "v"); handled {
		t.Fatal("nil provider handled a prop")
	}
	pp = NewPropProvider(nil, nil, nil)
	if got := pp.PropsFor(context.Background(), "alice", "/a"); got != nil {
		t.Fatalf("props = %+v", got)
	}
}
