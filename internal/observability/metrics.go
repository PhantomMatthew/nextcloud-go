package observability

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// Metric family names registered by NewRegistry. Label cardinality is
// deliberately bounded: plugin and function come from the fixed ABI surface,
// and result is a small classified set (see ClassifyResult) — never a raw
// error code.
const (
	// MetricPluginHostCallsTotal counts ABI host calls by plugin, function,
	// and result class.
	MetricPluginHostCallsTotal = "ncgo_plugin_host_calls_total"
	// MetricPluginHostCallDurationSeconds observes ABI host call latency by
	// plugin and function.
	MetricPluginHostCallDurationSeconds = "ncgo_plugin_host_call_duration_seconds"
	// MetricPluginCapabilityDenialsTotal counts host calls rejected for a
	// missing capability grant (a security signal).
	MetricPluginCapabilityDenialsTotal = "ncgo_plugin_capability_denials_total"
)

// histogramBuckets are the fixed latency bucket upper bounds (seconds) used
// for every histogram family, sized for ABI host calls (µs) up to slow
// outbound I/O (seconds).
var histogramBuckets = []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}

// Label is a single metric label key/value pair.
type Label struct {
	Name  string
	Value string
}

// Registry is a minimal, stdlib-only Prometheus text-exposition registry
// (format v0.0.4). It supports counters and fixed-bucket histograms keyed by
// label set. Families must be registered up front with their canonical label
// names; series are created on first use. It is safe for concurrent use.
//
// ncgo deliberately does not depend on github.com/prometheus/client_golang:
// the exposition format is simple text, the metric surface is small and
// bounded, and the project is stdlib-first (ADR-0055).
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	order    []string // family names in registration order, for stable render
}

type family struct {
	name       string
	help       string
	isHist     bool
	labelNames map[string]struct{}
	counters   map[string]*counterSeries
	histograms map[string]*histogramSeries
}

type counterSeries struct {
	labels []Label // canonical order
	value  uint64
}

type histogramSeries struct {
	labels  []Label  // canonical order
	buckets []uint64 // per-bucket (non-cumulative), len == len(histogramBuckets)+1
	count   uint64   // == buckets[len-1] (+Inf), kept for clarity
	sum     float64  // sum of observed values
}

// NewRegistry constructs a registry with the plugin metric families
// pre-registered so scrapes see their HELP/TYPE lines even before any series
// exists.
func NewRegistry() *Registry {
	r := &Registry{families: make(map[string]*family)}
	r.RegisterCounter(MetricPluginHostCallsTotal,
		"Per-plugin ABI host calls by function and result class.", "plugin", "function", "result")
	r.RegisterHistogram(MetricPluginHostCallDurationSeconds,
		"Per-plugin ABI host call latency in seconds.", "plugin", "function")
	r.RegisterCounter(MetricPluginCapabilityDenialsTotal,
		"Per-plugin ABI host calls denied for a missing capability grant.", "plugin", "function")
	return r
}

// RegisterCounter declares a counter family with its canonical label names.
// It panics on a duplicate or conflicting registration (programmer error).
func (r *Registry) RegisterCounter(name, help string, labelNames ...string) {
	r.register(name, help, false, labelNames)
}

// RegisterHistogram declares a histogram family (fixed buckets) with its
// canonical label names. It panics on a duplicate or conflicting registration.
func (r *Registry) RegisterHistogram(name, help string, labelNames ...string) {
	r.register(name, help, true, labelNames)
}

func (r *Registry) register(name, help string, isHist bool, labelNames []string) {
	if name == "" {
		panic("observability: empty metric name")
	}
	names := make(map[string]struct{}, len(labelNames))
	for _, n := range labelNames {
		if n == "" {
			panic("observability: empty label name")
		}
		if _, dup := names[n]; dup {
			panic("observability: duplicate label name " + n)
		}
		names[n] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.families[name]; exists {
		panic("observability: duplicate metric family " + name)
	}
	f := &family{name: name, help: help, isHist: isHist, labelNames: names}
	if isHist {
		f.histograms = make(map[string]*histogramSeries)
	} else {
		f.counters = make(map[string]*counterSeries)
	}
	r.families[name] = f
	r.order = append(r.order, name)
}

// canonical sorts labels into a deterministic order (by name) and validates
// them against the family's declared label names. It returns the canonical
// slice and a map key. Passing labels the family did not declare, or missing
// a declared label, is a programmer error and panics.
func (f *family) canonical(labels []Label) ([]Label, string) {
	if len(labels) != len(f.labelNames) {
		panic(fmt.Sprintf("observability: %s: got %d labels, want %d", f.name, len(labels), len(f.labelNames)))
	}
	out := make([]Label, len(labels))
	copy(out, labels)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	var key strings.Builder
	for i, l := range out {
		if _, ok := f.labelNames[l.Name]; !ok {
			panic(fmt.Sprintf("observability: %s: undeclared label %q", f.name, l.Name))
		}
		if i > 0 {
			key.WriteByte(0xff)
		}
		key.WriteString(l.Name)
		key.WriteByte(0xfe)
		key.WriteString(l.Value)
	}
	return out, key.String()
}

// IncCounter increments a counter series by one, creating it on first use.
func (r *Registry) IncCounter(name string, labels ...Label) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || f.isHist {
		panic("observability: unregistered counter " + name)
	}
	canonical, key := f.canonical(labels)
	s, ok := f.counters[key]
	if !ok {
		s = &counterSeries{labels: canonical}
		f.counters[key] = s
	}
	s.value++
}

// ObserveHistogram records one observation (seconds) in a histogram series,
// creating it on first use.
func (r *Registry) ObserveHistogram(name string, seconds float64, labels ...Label) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || !f.isHist {
		panic("observability: unregistered histogram " + name)
	}
	canonical, key := f.canonical(labels)
	s, ok := f.histograms[key]
	if !ok {
		s = &histogramSeries{labels: canonical, buckets: make([]uint64, len(histogramBuckets)+1)}
		f.histograms[key] = s
	}
	i := sort.SearchFloat64s(histogramBuckets, seconds)
	s.buckets[i]++
	s.count++
	s.sum += seconds
}

// Render writes the full exposition in Prometheus text format v0.0.4:
// HELP/TYPE lines per family (even with zero series), counters as integer
// values, histograms as cumulative *_bucket{le="..."} lines plus *_sum and
// *_count.
func (r *Registry) Render(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range r.order {
		f := r.families[name]
		fmt.Fprintf(w, "# HELP %s %s\n", f.name, escapeHelp(f.help))
		if f.isHist {
			fmt.Fprintf(w, "# TYPE %s histogram\n", f.name)
			r.renderHistogramFamily(w, f)
		} else {
			fmt.Fprintf(w, "# TYPE %s counter\n", f.name)
			r.renderCounterFamily(w, f)
		}
	}
}

func (r *Registry) renderCounterFamily(w io.Writer, f *family) {
	keys := sortedKeys(f.counters)
	for _, k := range keys {
		s := f.counters[k]
		fmt.Fprintf(w, "%s%s %d\n", f.name, renderLabels(s.labels), s.value)
	}
}

func (r *Registry) renderHistogramFamily(w io.Writer, f *family) {
	keys := sortedKeys(f.histograms)
	for _, k := range keys {
		s := f.histograms[k]
		var cumulative uint64
		for i, b := range histogramBuckets {
			cumulative += s.buckets[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.name,
				renderLabels(appendLe(s.labels, formatFloat(b))), cumulative)
		}
		fmt.Fprintf(w, "%s_bucket%s %d\n", f.name,
			renderLabels(appendLe(s.labels, "+Inf")), s.count)
		fmt.Fprintf(w, "%s_sum%s %s\n", f.name, renderLabels(s.labels), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", f.name, renderLabels(s.labels), s.count)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// appendLe returns labels with the histogram le label appended (le sorts
// after every declared label name used by ncgo, and Prometheus accepts label
// order regardless).
func appendLe(labels []Label, le string) []Label {
	out := make([]Label, 0, len(labels)+1)
	out = append(out, labels...)
	return append(out, Label{Name: "le", Value: le})
}

func renderLabels(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString("=\"")
		b.WriteString(escapeLabelValue(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes backslash, double quote, and newline per the
// Prometheus text format.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// escapeHelp escapes backslash and newline in HELP text.
func escapeHelp(v string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(v)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Handler returns an http.Handler serving the registry's exposition at
// Prometheus's text/plain; version=0.0.4 content type. When token is
// non-empty, requests must carry `Authorization: Bearer <token>`; the
// comparison is constant-time over SHA-256 digests (so neither the token nor
// its length leaks through timing), and anything else is a bare 401. Network
// level restriction of the listener is recommended regardless.
func (r *Registry) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if token != "" {
			got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
			wantSum := sha256.Sum256([]byte(token))
			gotSum := sha256.Sum256([]byte(got))
			if !ok || subtle.ConstantTimeCompare(gotSum[:], wantSum[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Render(w)
	})
}

// ClassifyResult maps a raw ABI error code to a bounded result-class label
// value. Raw codes are never used as label values: the label set is the
// cardinality budget, so everything without a dedicated class collapses to
// "internal". The ABI contract is negative = error code, non-negative =
// success (several functions return byte counts), so positive values are ok.
func ClassifyResult(code int32) string {
	if code > 0 {
		return "ok"
	}
	switch code {
	case pluginsdk.ErrCodeOK:
		return "ok"
	case pluginsdk.ErrCodePermissionDenied:
		return "permission_denied"
	case pluginsdk.ErrCodeInvalidArgument:
		return "invalid_argument"
	case pluginsdk.ErrCodeNotFound:
		return "not_found"
	case pluginsdk.ErrCodeUnavailable:
		return "unavailable"
	case pluginsdk.ErrCodeUnsupported:
		return "unsupported"
	case pluginsdk.ErrCodeTooLarge:
		return "too_large"
	case pluginsdk.ErrCodeTimeout:
		return "timeout"
	case pluginsdk.ErrCodeCanceled:
		return "canceled"
	default:
		// ErrCodeInternal, ErrCodeAlreadyExists, ErrCodeQuotaExceeded,
		// ErrCodeConflict, and any unknown negative code.
		return "internal"
	}
}
