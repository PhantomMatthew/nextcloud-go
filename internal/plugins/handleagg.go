package plugins

import "sync"

// handleAggregate enforces the spec §8 handle budgets per plugin id, shared
// across all of the plugin's live instances (ADR-0068). The pooled and
// per_request instance models multiply a plugin's instance count, so
// per-instance tables alone let one plugin's open-handle footprint — rows
// handles pin database/sql pool connections — scale with instance count and
// starve the shared pool. Counts are keyed by plugin id then handle kind;
// entries are deleted at zero so the map stays bounded by handles actually
// open and resets on process restart like the 4n/4o quota state.
type handleAggregate struct {
	mu  sync.Mutex
	per map[string]map[handleKind]int32
}

func newHandleAggregate() *handleAggregate {
	return &handleAggregate{per: make(map[string]map[handleKind]int32)}
}

// acquire takes one of the plugin's aggregate slots for kind; ok is false
// when the plugin is already at the kind's §8 budget across its instances.
// Take and release happen in the same critical section — there is no
// lock-free state.
func (a *handleAggregate) acquire(pluginID string, kind handleKind) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	kinds := a.per[pluginID]
	if kinds == nil {
		kinds = make(map[handleKind]int32)
		a.per[pluginID] = kinds
	}
	if kinds[kind] >= handleLimits[kind] {
		return false
	}
	kinds[kind]++
	return true
}

// release returns one slot, deleting the kind entry at zero (and the plugin
// entry once it holds nothing) so the map stays bounded by open handles.
func (a *handleAggregate) release(pluginID string, kind handleKind) {
	a.mu.Lock()
	defer a.mu.Unlock()
	kinds := a.per[pluginID]
	if kinds == nil {
		return
	}
	if n := kinds[kind] - 1; n > 0 {
		kinds[kind] = n
	} else {
		delete(kinds, kind)
	}
	if len(kinds) == 0 {
		delete(a.per, pluginID)
	}
}
