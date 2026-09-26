package observability

import (
	"math"
	"testing"
)

func TestCounterSeriesDump(t *testing.T) {
	r := NewRegistry()
	r.AddCounter(MetricPluginHostCallsTotal, 3,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"}, Label{Name: "result", Value: "ok"})
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "result", Value: "timeout"}, Label{Name: "function", Value: "f"}, Label{Name: "plugin", Value: "p1"})

	series := r.CounterSeries(MetricPluginHostCallsTotal)
	if len(series) != 2 {
		t.Fatalf("series count = %d, want 2", len(series))
	}
	// Series are key-ordered like Render ("ok" < "timeout"), labels
	// canonical-sorted by name.
	for i, want := range []struct {
		result string
		value  int64
	}{{"ok", 3}, {"timeout", 1}} {
		s := series[i]
		wantLabels := []Label{
			{Name: "function", Value: "f"},
			{Name: "plugin", Value: "p1"},
			{Name: "result", Value: want.result},
		}
		if len(s.Labels) != len(wantLabels) {
			t.Fatalf("series[%d] labels = %+v", i, s.Labels)
		}
		for j, l := range wantLabels {
			if s.Labels[j] != l {
				t.Errorf("series[%d].Labels[%d] = %+v, want %+v", i, j, s.Labels[j], l)
			}
		}
		if s.Value != want.value {
			t.Errorf("series[%d].Value = %d, want %d", i, s.Value, want.value)
		}
	}
}

func TestCounterSeriesUnknownFamily(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "plugin", Value: "p"}, Label{Name: "function", Value: "f"}, Label{Name: "result", Value: "ok"})
	for _, name := range []string{
		"nope_total",                        // never registered
		MetricPluginHostCallDurationSeconds, // a histogram family
		MetricPluginMemoryHighWaterBytes,    // a gauge family
		MetricPluginStorageBytesTotal,       // registered but series-less
	} {
		if got := r.CounterSeries(name); len(got) != 0 {
			t.Errorf("CounterSeries(%q) = %+v, want empty", name, got)
		}
	}
}

func TestGaugeSeriesDump(t *testing.T) {
	r := NewRegistry()
	r.SetGaugeMax(MetricPluginMemoryHighWaterBytes, 1024, Label{Name: "plugin", Value: "p1"})
	r.SetGaugeMax(MetricPluginMemoryHighWaterBytes, 2048, Label{Name: "plugin", Value: "p1"})
	r.SetGauge(MetricPluginMemoryHighWaterBytes, 512, Label{Name: "plugin", Value: "p2"})

	series := r.GaugeSeries(MetricPluginMemoryHighWaterBytes)
	if len(series) != 2 {
		t.Fatalf("series count = %d, want 2", len(series))
	}
	// Series are key-ordered like Render ("p1" < "p2").
	for i, want := range []struct {
		plugin string
		value  int64
	}{{"p1", 2048}, {"p2", 512}} {
		s := series[i]
		wantLabels := []Label{{Name: "plugin", Value: want.plugin}}
		if len(s.Labels) != len(wantLabels) {
			t.Fatalf("series[%d] labels = %+v", i, s.Labels)
		}
		for j, l := range wantLabels {
			if s.Labels[j] != l {
				t.Errorf("series[%d].Labels[%d] = %+v, want %+v", i, j, s.Labels[j], l)
			}
		}
		if s.Value != want.value {
			t.Errorf("series[%d].Value = %d, want %d", i, s.Value, want.value)
		}
	}
}

func TestGaugeSeriesUnknownFamily(t *testing.T) {
	r := NewRegistry()
	r.SetGauge(MetricPluginMemoryHighWaterBytes, 1, Label{Name: "plugin", Value: "p"})
	r.RegisterGauge("empty_gauge", "registered but series-less", "plugin")
	for _, name := range []string{
		"nope_bytes",                         // never registered
		MetricPluginHostCallsTotal,           // a counter family
		MetricPluginEntryCallDurationSeconds, // a histogram family
		"empty_gauge",                        // registered but series-less
	} {
		if got := r.GaugeSeries(name); len(got) != 0 {
			t.Errorf("GaugeSeries(%q) = %+v, want empty", name, got)
		}
	}
}

func TestHistogramSeriesDump(t *testing.T) {
	r := NewRegistry()
	r.ObserveHistogram(MetricPluginHostCallDurationSeconds, 0.002,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"})
	r.ObserveHistogram(MetricPluginHostCallDurationSeconds, 0.03,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"})

	series := r.HistogramSeries(MetricPluginHostCallDurationSeconds)
	if len(series) != 1 {
		t.Fatalf("series count = %d, want 1", len(series))
	}
	s := series[0]
	wantLabels := []Label{{Name: "function", Value: "f"}, {Name: "plugin", Value: "p1"}}
	for j, l := range wantLabels {
		if s.Labels[j] != l {
			t.Errorf("Labels[%d] = %+v, want %+v", j, s.Labels[j], l)
		}
	}
	// Cumulative shape over the fixed grid: 0.002 lands in le=0.005, 0.03 in
	// le=0.05; everything at/above le=0.05 carries both observations.
	wantCounts := []int64{0, 0, 1, 1, 1, 2, 2, 2, 2, 2, 2, 2}
	if len(s.Buckets) != len(histogramBuckets)+1 {
		t.Fatalf("buckets = %d, want %d", len(s.Buckets), len(histogramBuckets)+1)
	}
	for i, want := range wantCounts {
		if s.Buckets[i].Count != want {
			t.Errorf("Buckets[%d].Count = %d, want %d (le=%v)", i, s.Buckets[i].Count, want, s.Buckets[i].Le)
		}
		if i < len(histogramBuckets) && s.Buckets[i].Le != histogramBuckets[i] {
			t.Errorf("Buckets[%d].Le = %v, want grid %v", i, s.Buckets[i].Le, histogramBuckets[i])
		}
	}
	last := s.Buckets[len(s.Buckets)-1]
	if !math.IsInf(last.Le, 1) || last.Count != 2 {
		t.Errorf("final bucket = %+v, want +Inf/2", last)
	}
	if s.Count != 2 || math.Abs(s.Sum-0.032) > 1e-12 {
		t.Errorf("Count/Sum = %d/%v, want 2/0.032", s.Count, s.Sum)
	}

	for _, name := range []string{"nope", MetricPluginHostCallsTotal} {
		if got := r.HistogramSeries(name); len(got) != 0 {
			t.Errorf("HistogramSeries(%q) = %+v, want empty", name, got)
		}
	}
}

func TestSeriesDumpDefensiveCopies(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginCapabilityDenialsTotal,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"})
	r.ObserveHistogram(MetricPluginHostCallDurationSeconds, 0.002,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"})
	r.SetGauge(MetricPluginMemoryHighWaterBytes, 1024, Label{Name: "plugin", Value: "p1"})

	counters := r.CounterSeries(MetricPluginCapabilityDenialsTotal)
	counters[0].Labels[0].Value = "mutated"
	hists := r.HistogramSeries(MetricPluginHostCallDurationSeconds)
	hists[0].Labels[0].Value = "mutated"
	hists[0].Buckets[0].Count = 999
	gauges := r.GaugeSeries(MetricPluginMemoryHighWaterBytes)
	gauges[0].Labels[0].Value = "mutated"
	gauges[0].Value = 1

	counters = r.CounterSeries(MetricPluginCapabilityDenialsTotal)
	if counters[0].Labels[0].Value != "f" {
		t.Errorf("counter labels mutated through the copy: %+v", counters[0].Labels)
	}
	hists = r.HistogramSeries(MetricPluginHostCallDurationSeconds)
	if hists[0].Labels[0].Value != "f" || hists[0].Buckets[0].Count != 0 {
		t.Errorf("histogram mutated through the copy: %+v", hists[0])
	}
	gauges = r.GaugeSeries(MetricPluginMemoryHighWaterBytes)
	if gauges[0].Labels[0].Value != "p1" || gauges[0].Value != 1024 {
		t.Errorf("gauge mutated through the copy: %+v", gauges[0])
	}
}

func TestQuantile(t *testing.T) {
	// merged is a hand-built series on a 10/20/+Inf grid: cumulative counts
	// 2, 4, 4 (no observations above 20).
	merged := HistogramSeries{
		Buckets: []HistogramBucket{{Le: 10, Count: 2}, {Le: 20, Count: 4}, {Le: math.Inf(1), Count: 4}},
		Count:   4,
	}
	// mid carries all four observations in the 10–20 bucket, so p50
	// interpolates to the bucket midpoint.
	mid := HistogramSeries{
		Buckets: []HistogramBucket{{Le: 10, Count: 0}, {Le: 20, Count: 4}, {Le: math.Inf(1), Count: 4}},
		Count:   4,
	}
	// overflow has one observation past the last finite bound (cum 1, 2, 3).
	overflow := HistogramSeries{
		Buckets: []HistogramBucket{{Le: 10, Count: 1}, {Le: 20, Count: 2}, {Le: math.Inf(1), Count: 3}},
		Count:   3,
	}
	tests := []struct {
		name string
		h    HistogramSeries
		q    float64
		want float64
	}{
		{name: "empty series", h: HistogramSeries{}, q: 0.5, want: 0},
		{name: "zero count with buckets", h: HistogramSeries{Buckets: merged.Buckets}, q: 0.5, want: 0},
		{name: "midpoint interpolation", h: mid, q: 0.5, want: 15},
		{name: "partial bucket", h: merged, q: 0.5, want: 10},
		{name: "q=0 floor", h: merged, q: 0, want: 0},
		{name: "q=1 reaches last finite bucket end", h: merged, q: 1, want: 20},
		{name: "q clamped above 1", h: merged, q: 2, want: 20},
		{name: "q clamped below 0", h: merged, q: -1, want: 0},
		{name: "last-bucket cap", h: overflow, q: 0.99, want: 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Quantile(tt.h, tt.q); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Quantile(q=%v) = %v, want %v", tt.q, got, tt.want)
			}
		})
	}
}

func TestQuantileSingleObservation(t *testing.T) {
	r := NewRegistry()
	r.ObserveHistogram(MetricPluginHostCallDurationSeconds, 0.02,
		Label{Name: "plugin", Value: "p1"}, Label{Name: "function", Value: "f"})
	s := r.HistogramSeries(MetricPluginHostCallDurationSeconds)[0]
	// 0.02 sits in the le=0.025 bucket: p50 interpolates halfway between the
	// previous bound (0.01) and 0.025 (Prometheus bucket semantics — the
	// estimate is a bucket position, not the observed value).
	if got, want := Quantile(s, 0.5), 0.0175; math.Abs(got-want) > 1e-12 {
		t.Errorf("p50 = %v, want %v", got, want)
	}
	if got := Quantile(s, 0.95); got <= 0.01 || got > 0.025 {
		t.Errorf("p95 = %v, want within (0.01, 0.025]", got)
	}
}
