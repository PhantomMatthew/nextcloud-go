package capabilities

import (
	"bytes"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

type stubProvider struct {
	caps ocs.OrderedMap
}

func (s stubProvider) GetCapabilities() ocs.OrderedMap { return s.caps }

func TestManagerCollectPreservesRegistrationOrder(t *testing.T) {
	m := NewManager()
	m.Register(stubProvider{caps: ocs.Obj(ocs.K("alpha", ocs.Obj(ocs.K("a", 1))))})
	m.Register(stubProvider{caps: ocs.Obj(ocs.K("beta", ocs.Obj(ocs.K("b", 2))))})
	m.Register(stubProvider{caps: ocs.Obj(ocs.K("gamma", ocs.Obj(ocs.K("g", 3))))})

	got := m.Collect()
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i, kv := range got {
		if kv.Key != want[i] {
			t.Errorf("idx %d key=%q want %q", i, kv.Key, want[i])
		}
	}
}

func TestManagerCollectRecursiveMerge(t *testing.T) {
	m := NewManager()
	m.Register(stubProvider{caps: ocs.Obj(
		ocs.K("core", ocs.Obj(
			ocs.K("pollinterval", 60),
			ocs.K("webdav-root", "remote.php/webdav"),
		)),
	)})
	m.Register(stubProvider{caps: ocs.Obj(
		ocs.K("core", ocs.Obj(
			ocs.K("reference-api", true),
		)),
		ocs.K("files", ocs.Obj(ocs.K("undelete", true))),
	)})

	got := m.Collect()
	if len(got) != 2 {
		t.Fatalf("top-level len=%d want 2", len(got))
	}
	if got[0].Key != "core" || got[1].Key != "files" {
		t.Fatalf("top-level keys=%v want [core files]", []string{got[0].Key, got[1].Key})
	}
	core, ok := got[0].Value.(ocs.OrderedMap)
	if !ok {
		t.Fatalf("core not OrderedMap: %T", got[0].Value)
	}
	wantCoreKeys := []string{"pollinterval", "webdav-root", "reference-api"}
	if len(core) != len(wantCoreKeys) {
		t.Fatalf("core len=%d want %d", len(core), len(wantCoreKeys))
	}
	for i, kv := range core {
		if kv.Key != wantCoreKeys[i] {
			t.Errorf("core idx %d key=%q want %q", i, kv.Key, wantCoreKeys[i])
		}
	}
}

func TestManagerCollectScalarOverwrites(t *testing.T) {
	m := NewManager()
	m.Register(stubProvider{caps: ocs.Obj(ocs.K("core", ocs.Obj(ocs.K("pollinterval", 60))))})
	m.Register(stubProvider{caps: ocs.Obj(ocs.K("core", ocs.Obj(ocs.K("pollinterval", 120))))})

	got := m.Collect()
	core := got[0].Value.(ocs.OrderedMap)
	if core[0].Value != 120 {
		t.Errorf("pollinterval=%v want 120", core[0].Value)
	}
}

func TestCoreProviderShape(t *testing.T) {
	caps := DefaultCoreProvider().GetCapabilities()
	if len(caps) != 1 || caps[0].Key != "core" {
		t.Fatalf("top key=%v want core", caps)
	}
	core := caps[0].Value.(ocs.OrderedMap)
	wantKeys := []string{"pollinterval", "webdav-root", "reference-api", "reference-regex"}
	if len(core) != len(wantKeys) {
		t.Fatalf("core len=%d want %d", len(core), len(wantKeys))
	}
	for i, kv := range core {
		if kv.Key != wantKeys[i] {
			t.Errorf("core idx %d key=%q want %q", i, kv.Key, wantKeys[i])
		}
	}
	body, err := caps.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(body, []byte(`"pollinterval":60`)) {
		t.Errorf("missing pollinterval: %s", body)
	}
	if !bytes.Contains(body, []byte(`"reference-api":true`)) {
		t.Errorf("missing reference-api: %s", body)
	}
}
