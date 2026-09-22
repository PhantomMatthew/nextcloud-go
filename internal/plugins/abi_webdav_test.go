package plugins

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/plugins/internal/wasmgen"
)

func propCapsManifest(props ...string) *Manifest {
	m := probeManifest()
	m.Capabilities = Capabilities{WebDAV: WebDAVCapabilities{Props: props}}
	return m
}

func TestPropRegisterPersists(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, propCapsManifest("x:tags"), wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.PropsForPlugin(context.Background(), "com.example.probe")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("recs = %+v", recs)
	}
	rec := recs[0]
	if rec.Name != "x:tags" || rec.Getter != "getTags" || rec.Setter != "setTags" {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestPropRegisterReadOnly(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, propCapsManifest("x:tags"), wasmgen.PropModule("x:tags", "getTags", "", ErrCodeOK))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.PropsForPlugin(context.Background(), "com.example.probe")
	if err != nil || len(recs) != 1 || recs[0].Setter != "" {
		t.Fatalf("recs = %+v %v", recs, err)
	}
}

func TestPropRegisterDenied(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	installModule(t, h, propCapsManifest("y:other"), wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodePermissionDenied))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestPropRegisterBadName(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	m := propCapsManifest("*")
	for _, name := range []string{"plain", ":local", "x:", "x:1bad", "x:has space"} {
		installModule(t, h, m, wasmgen.PropModule(name, "getTags", "setTags", ErrCodeInvalidArgument))
	}
	if strings.Count(buf.String(), "probe-ok") != 5 {
		t.Fatalf("log %q", buf.String())
	}
	recs, err := reg.AllProps(context.Background())
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
}

func TestPropRegisterBadFunc(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	m := propCapsManifest("x:tags")
	installModule(t, h, m, wasmgen.PropModule("x:tags", "", "setTags", ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.PropModule("x:tags", "has space", "setTags", ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.PropModule("x:tags", strings.Repeat("a", 129), "setTags", ErrCodeInvalidArgument))
	installModule(t, h, m, wasmgen.PropModule("x:tags", "getTags", "bad setter", ErrCodeInvalidArgument))
	if strings.Count(buf.String(), "probe-ok") != 4 {
		t.Fatalf("log %q", buf.String())
	}
}

func TestPropRegisterOutsideHook(t *testing.T) {
	reg := NewRegistry(testDB(t))
	h, buf := testHost(t, HostConfig{Registry: reg})
	ctx := context.Background()
	p, err := h.Load(ctx, propCapsManifest("x:tags"), wasmgen.PropModuleOpts(wasmgen.PropOpts{
		Name: "x:tags", Getter: "getTags", Want: ErrCodePermissionDenied, NoHook: true,
	}))
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
	recs, err := reg.AllProps(ctx)
	if err != nil || len(recs) != 0 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
}

func TestPropRegisterNilRegistry(t *testing.T) {
	h, buf := testHost(t, HostConfig{})
	installModule(t, h, propCapsManifest("x:tags"), wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeUnavailable))
	if !strings.Contains(buf.String(), "probe-ok") {
		t.Fatalf("log %q", buf.String())
	}
}

func TestRegistryPropsRoundTrip(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	if err := reg.UpsertProp(ctx, &PropRecord{PluginID: "com.example.a", Name: "x:tags", Getter: "getTags", Setter: "setTags"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertProp(ctx, &PropRecord{PluginID: "com.example.b", Name: "y:tags", Getter: "getTags"}); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces getter/setter for the same (plugin, name).
	if err := reg.UpsertProp(ctx, &PropRecord{PluginID: "com.example.a", Name: "x:tags", Getter: "getTagsV2"}); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.PropsForPlugin(ctx, "com.example.a")
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
	if recs[0].Getter != "getTagsV2" || recs[0].Setter != "" {
		t.Fatalf("rec = %+v", recs[0])
	}
	all, err := reg.AllProps(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v %v", all, err)
	}
	if all[0].PluginID != "com.example.a" || all[1].PluginID != "com.example.b" {
		t.Fatalf("all = %+v", all)
	}
	if err := reg.DeletePropsForPlugin(ctx, "com.example.a"); err != nil {
		t.Fatal(err)
	}
	all, err = reg.AllProps(ctx)
	if err != nil || len(all) != 1 || all[0].PluginID != "com.example.b" {
		t.Fatalf("all = %+v %v", all, err)
	}
	// Deleting a plugin without props is not an error.
	if err := reg.DeletePropsForPlugin(ctx, "com.example.gone"); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallDeletesProps(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	reg := NewRegistry(testDB(t))
	h, err := NewHost(ctx, HostConfig{Registry: reg}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	in := &Installer{
		Host:        h,
		Registry:    reg,
		InstallDir:  t.TempDir(),
		TrustedKeys: []ed25519.PublicKey{pub},
	}
	caps := `
[capabilities]
webdav.props = ["x:tags"]
`
	members := map[string][]byte{
		"plugin.toml": installManifest("1.0.0", caps),
		"hello.wasm":  wasmgen.PropModule("x:tags", "getTags", "setTags", ErrCodeOK),
	}
	sig, err := SignMembers(members, priv)
	if err != nil {
		t.Fatal(err)
	}
	members[SignatureFile] = sig
	raw, err := WriteArchive(members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Install(ctx, raw, InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	recs, err := reg.PropsForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 1 {
		t.Fatalf("recs = %+v %v", recs, err)
	}
	if err := in.Uninstall(ctx, "com.example.inst"); err != nil {
		t.Fatal(err)
	}
	recs, err = reg.PropsForPlugin(ctx, "com.example.inst")
	if err != nil || len(recs) != 0 {
		t.Fatalf("props survive uninstall: %+v %v", recs, err)
	}
}
