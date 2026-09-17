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
