package plugins

import (
	"errors"
	"strings"
	"testing"
)

func TestParseManifestOK(t *testing.T) {
	raw := `
[plugin]
id = "com.example.hello"
name = "Hello"
version = "0.1.0"
abi = "ncgo-abi/1"

[runtime]
instance_model = "per_request"
memory_limit_mb = 32
cpu_timeout_ms = 5000

[entry_points]
module = "hello.wasm"
on_install = "ncgo_on_install"
`
	m, err := ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Plugin.ID != "com.example.hello" || m.EntryPoints.Module != "hello.wasm" {
		t.Fatalf("%+v", m)
	}
}

func TestParseManifestDefaultInstanceModel(t *testing.T) {
	raw := `
[plugin]
id = "com.example.hello"
abi = "ncgo-abi/1"
[entry_points]
module = "hello.wasm"
`
	m, err := ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.Runtime.InstanceModel != "per_request" {
		t.Fatalf("instance_model = %q", m.Runtime.InstanceModel)
	}
}

func TestManifestValidate(t *testing.T) {
	ok := Manifest{
		Plugin:      PluginSection{ID: "com.example.hello", ABI: abiV1},
		EntryPoints: EntryPointsSection{Module: "hello.wasm"},
	}
	cases := []struct {
		name string
		mut  func(*Manifest)
	}{
		{"bad id", func(m *Manifest) { m.Plugin.ID = "Hello" }},
		{"short id", func(m *Manifest) { m.Plugin.ID = "hello" }},
		{"bad abi", func(m *Manifest) { m.Plugin.ABI = "ncgo-abi/2" }},
		{"bad model", func(m *Manifest) { m.Runtime.InstanceModel = "fork" }},
		{"memory", func(m *Manifest) { m.Runtime.MemoryLimitMB = 257 }},
		{"timeout", func(m *Manifest) { m.Runtime.CPUTimeoutMS = 30001 }},
		{"module", func(m *Manifest) { m.EntryPoints.Module = "" }},
		{"pooled no size", func(m *Manifest) {
			m.Runtime.InstanceModel = "pooled"
			m.Runtime.PoolSize = 0
		}},
		{"bad storage scope", func(m *Manifest) {
			m.Capabilities.Storage.Read = []string{"everything"}
		}},
		{"bad outbound host", func(m *Manifest) {
			m.Capabilities.HTTP.Outbound = []string{"not a host"}
		}},
		{"core topic", func(m *Manifest) {
			m.Capabilities.Events.Publish = []string{"core.user.deleted"}
		}},
		{"route outside apps", func(m *Manifest) {
			m.Capabilities.Routes.Register = []string{"/admin/x"}
		}},
		{"reserved prop", func(m *Manifest) {
			m.Capabilities.WebDAV.Props = []string{"oc:size"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := ok
			tc.mut(&m)
			if err := m.Validate(); !errors.Is(err, ErrManifestInvalid) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestParseManifestCapabilities(t *testing.T) {
	raw := `
[plugin]
id = "com.example.tagger"
abi = "ncgo-abi/1"

[runtime]
instance_model = "pooled"
pool_size = 2

[capabilities]
db.read = ["file_tags", "filecache"]
db.write = ["file_tags"]
http.outbound = ["*.icloud.com:443"]
http.outbound_allow_private = true
events.publish = ["tagger.tagged"]
events.subscribe = ["files.uploaded"]
jobs.register = true
ocs.register = ["/apps/tagger/api/v1/*"]
webdav.props = ["tagger:score"]
config.read = ["tagger.*"]

[entry_points]
module = "tagger.wasm"
`
	m, err := ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := m.Capabilities
	if len(c.DB.Read) != 2 || c.DB.Read[0] != "file_tags" {
		t.Fatalf("db.read = %v", c.DB.Read)
	}
	if !c.canDBWrite("file_tags") || c.canDBWrite("filecache") {
		t.Fatalf("db.write = %v", c.DB.Write)
	}
	if !c.Jobs.Register || !c.canRegisterOCS("/apps/tagger/api/v1/list") {
		t.Fatalf("caps = %+v", c)
	}
	if !c.httpOutboundAllowPrivate() {
		t.Fatalf("http.outbound_allow_private not decoded: %+v", c.HTTP)
	}
	if !c.canProvideProp("tagger:score") || !c.hasConfigRead() {
		t.Fatalf("caps = %+v", c)
	}
}
