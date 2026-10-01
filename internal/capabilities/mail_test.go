package capabilities

import (
	"encoding/json"
	"testing"
)

func TestMailProviderShape(t *testing.T) {
	got := MailProvider{}.GetCapabilities()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	block, ok := top["mail"].(map[string]any)
	if !ok {
		t.Fatalf("mail block missing: %s", raw)
	}
	if block["enabled"] != true {
		t.Errorf("mail.enabled = %v", block["enabled"])
	}
	if len(block) != 1 {
		t.Errorf("mail block = %v (M1 is intentionally minimal)", block)
	}
}

func TestMailProviderMerges(t *testing.T) {
	m := NewManager()
	m.Register(DefaultCoreProvider())
	m.Register(MailProvider{})
	merged := m.Collect()
	keys := make([]string, 0, len(merged))
	for _, kv := range merged {
		keys = append(keys, kv.Key)
	}
	if len(keys) != 2 || keys[0] != "core" || keys[1] != "mail" {
		t.Errorf("merged keys = %v", keys)
	}
}
