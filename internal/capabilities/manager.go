package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// Manager registers capability providers in deterministic order and
// merges their contributions into a single OrderedMap. Top-level keys
// retain registration order; same-key contributions are recursively
// merged (mirroring PHP array_replace_recursive semantics).
type Manager struct {
	providers []Provider
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) Register(p Provider) {
	m.providers = append(m.providers, p)
}

// Collect returns the merged capabilities map. The result is safe to
// hand to the OCS renderer; callers must not mutate it.
func (m *Manager) Collect() ocs.OrderedMap {
	merged := ocs.OrderedMap{}
	for _, p := range m.providers {
		caps := p.GetCapabilities()
		for _, kv := range caps {
			merged = mergeKey(merged, kv)
		}
	}
	return merged
}

func mergeKey(dst ocs.OrderedMap, kv ocs.KV) ocs.OrderedMap {
	for i, existing := range dst {
		if existing.Key != kv.Key {
			continue
		}
		dst[i].Value = mergeValue(existing.Value, kv.Value)
		return dst
	}
	return append(dst, kv)
}

func mergeValue(a, b any) any {
	am, aok := a.(ocs.OrderedMap)
	bm, bok := b.(ocs.OrderedMap)
	if !aok || !bok {
		return b
	}
	out := append(ocs.OrderedMap{}, am...)
	for _, kv := range bm {
		out = mergeKey(out, kv)
	}
	return out
}
