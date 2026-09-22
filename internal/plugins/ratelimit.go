package plugins

import "time"

// Outbound HTTP rate-limit policy (ADR-0059): a per-plugin token bucket over
// http_request calls. One call — including its whole redirect chain — costs
// one token. The burst of 30 lets a normal burst-style sync loop (fetch a
// dozen feeds at once) pass; it only throttles a plugin that keeps calling
// past its sustained rate, which is the runaway/abuse case.
const (
	defaultHTTPRatePerMinute = 120
	defaultHTTPBurst         = 30
)

// tokenBucket is a minimal stdlib token bucket (the repo deliberately does
// not take on x/time). It is not goroutine-safe; callers hold
// Host.httpRateMu. now is a field so tests can drive refills with a fake
// clock.
type tokenBucket struct {
	tokens float64
	max    float64
	// perNano is the refill rate in tokens per nanosecond.
	perNano float64
	last    time.Time
	now     func() time.Time
}

// newTokenBucket returns a full bucket refilling at ratePerMinute tokens per
// minute, capped at burst.
func newTokenBucket(ratePerMinute, burst int) *tokenBucket {
	return &tokenBucket{
		tokens:  float64(burst),
		max:     float64(burst),
		perNano: float64(ratePerMinute) / float64(time.Minute.Nanoseconds()),
		last:    time.Now(),
		now:     time.Now,
	}
}

// allow draws one token, refilling for the elapsed time first.
func (b *tokenBucket) allow() bool {
	now := b.now()
	b.tokens += float64(now.Sub(b.last).Nanoseconds()) * b.perNano
	b.last = now
	if b.tokens > b.max {
		b.tokens = b.max
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// allowHTTPRequest draws one token from the plugin's http_request bucket,
// creating a full bucket for a plugin id on first use (ADR-0059). The map is
// bounded by the installed plugin count and is process-local state: restart
// resets every allowance.
func (h *Host) allowHTTPRequest(pluginID string) bool {
	h.httpRateMu.Lock()
	defer h.httpRateMu.Unlock()
	b, ok := h.httpRate[pluginID]
	if !ok {
		b = newTokenBucket(h.cfg.HTTPRatePerMinute, h.cfg.HTTPRateBurst)
		h.httpRate[pluginID] = b
	}
	return b.allow()
}
