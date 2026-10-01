package capabilities

import (
	"encoding/json"
	"testing"
)

func TestRichdocumentsProviderShape(t *testing.T) {
	got := DefaultRichdocumentsProvider().GetCapabilities()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	rd, ok := top["richdocuments"].(map[string]any)
	if !ok {
		t.Fatalf("richdocuments block missing: %s", raw)
	}
	if rd["version"] != "1.0" || rd["productName"] != "Collabora Online" {
		t.Errorf("version/productName = %v %v", rd["version"], rd["productName"])
	}
	if rd["templates"] != false || rd["direct_editing"] != false {
		t.Errorf("templates/direct_editing = %v %v", rd["templates"], rd["direct_editing"])
	}
	mimes, ok := rd["mimetypes"].([]any)
	if !ok || len(mimes) != 9 {
		t.Fatalf("mimetypes = %v", rd["mimetypes"])
	}
	if mimes[0] != "application/vnd.oasis.opendocument.text" ||
		mimes[3] != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Errorf("mimetypes head = %v %v", mimes[0], mimes[3])
	}
	viewOnly, ok := rd["mimetypesNoDefaultOpen"].([]any)
	if !ok || len(viewOnly) != 1 || viewOnly[0] != "application/pdf" {
		t.Errorf("mimetypesNoDefaultOpen = %v", rd["mimetypesNoDefaultOpen"])
	}
}

func TestRichdocumentsProviderMerges(t *testing.T) {
	m := NewManager()
	m.Register(DefaultCoreProvider())
	m.Register(DefaultRichdocumentsProvider())
	merged := m.Collect()
	keys := make([]string, 0, len(merged))
	for _, kv := range merged {
		keys = append(keys, kv.Key)
	}
	if len(keys) != 2 || keys[0] != "core" || keys[1] != "richdocuments" {
		t.Errorf("merged keys = %v", keys)
	}
}
