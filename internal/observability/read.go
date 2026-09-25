package observability

import (
	"math"
	"sort"
)

// CounterSeries is a read-model snapshot of one counter series: its
// canonical (name-sorted) label set and current value.
type CounterSeries struct {
	Labels []Label
	Value  int64
}

// HistogramBucket is one cumulative bucket count in a histogram snapshot.
// The final bucket always has Le = +Inf and Count equal to the series' total
// observation count.
type HistogramBucket struct {
	Le    float64
	Count int64
}

// HistogramSeries is a read-model snapshot of one histogram series:
// canonical labels, cumulative bucket counts over the registry's fixed le
// grid (final bucket +Inf), plus the sum and count of all observations.
type HistogramSeries struct {
	Labels  []Label
	Buckets []HistogramBucket
	Sum     float64
	Count   int64
}

// CounterSeries returns a snapshot of every series in the named counter
// family, ordered by series key like Render. An unknown (or histogram)
// family yields nil. Labels and slices are defensive copies; callers may
// mutate them freely.
func (r *Registry) CounterSeries(name string) []CounterSeries {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || f.isHist || len(f.counters) == 0 {
		return nil
	}
	out := make([]CounterSeries, 0, len(f.counters))
	for _, k := range sortedKeys(f.counters) {
		s := f.counters[k]
		labels := make([]Label, len(s.labels))
		copy(labels, s.labels)
		out = append(out, CounterSeries{Labels: labels, Value: int64(s.value)}) //nolint:gosec // G115: process-local call tally, 2^63 calls unreachable
	}
	return out
}

// HistogramSeries returns a snapshot of every series in the named histogram
// family, ordered by series key like Render, with per-bucket counts
// accumulated into the cumulative shape Render exposes (final bucket +Inf).
// An unknown (or counter) family yields nil. Labels and slices are defensive
// copies; callers may mutate them freely.
func (r *Registry) HistogramSeries(name string) []HistogramSeries {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.families[name]
	if f == nil || !f.isHist || len(f.histograms) == 0 {
		return nil
	}
	out := make([]HistogramSeries, 0, len(f.histograms))
	for _, k := range sortedKeys(f.histograms) {
		s := f.histograms[k]
		labels := make([]Label, len(s.labels))
		copy(labels, s.labels)
		buckets := make([]HistogramBucket, 0, len(histogramBuckets)+1)
		var cumulative uint64
		for i, le := range histogramBuckets {
			cumulative += s.buckets[i]
			buckets = append(buckets, HistogramBucket{Le: le, Count: int64(cumulative)}) //nolint:gosec // G115: process-local observation tally
		}
		buckets = append(buckets, HistogramBucket{Le: math.Inf(1), Count: int64(s.count)}) //nolint:gosec // G115: process-local observation tally
		out = append(out, HistogramSeries{
			Labels:  labels,
			Buckets: buckets,
			Sum:     s.sum,
			Count:   int64(s.count), //nolint:gosec // G115: process-local observation tally
		})
	}
	return out
}

// Quantile estimates the q-quantile from cumulative buckets by linear
// interpolation within the matched bucket (Prometheus histogram_quantile
// semantics): rank = q × total count, the first bucket whose cumulative
// count reaches rank holds the estimate, and a rank landing in the final
// +Inf bucket caps at the last finite le. Count 0 or empty buckets yield 0;
// q outside [0,1] is clamped rather than answering ±Inf, keeping the result
// JSON-safe for the console API.
func Quantile(h HistogramSeries, q float64) float64 {
	if h.Count == 0 || len(h.Buckets) < 2 {
		return 0
	}
	q = min(max(q, 0), 1)
	buckets := h.Buckets
	rank := q * float64(h.Count)
	b := sort.Search(len(buckets)-1, func(i int) bool {
		return float64(buckets[i].Count) >= rank
	})
	if b == len(buckets)-1 {
		return buckets[len(buckets)-2].Le
	}
	if b == 0 && buckets[0].Le <= 0 {
		return buckets[0].Le
	}
	var bucketStart float64
	bucketEnd := buckets[b].Le
	count := buckets[b].Count
	if b > 0 {
		bucketStart = buckets[b-1].Le
		count -= buckets[b-1].Count
		rank -= float64(buckets[b-1].Count)
	}
	if count == 0 {
		return bucketEnd
	}
	return bucketStart + (bucketEnd-bucketStart)*(rank/float64(count))
}
