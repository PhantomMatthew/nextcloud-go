package observability

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	r.Render(&buf)
	return buf.String()
}

func TestRegistryCounterRender(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "plugin", Value: "com.example.probe"},
		Label{Name: "function", Value: "cache_get"},
		Label{Name: "result", Value: "ok"})
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "plugin", Value: "com.example.probe"},
		Label{Name: "function", Value: "cache_get"},
		Label{Name: "result", Value: "ok"})
	out := render(t, r)
	want := "# HELP ncgo_plugin_host_calls_total Per-plugin ABI host calls by function and result class.\n" +
		"# TYPE ncgo_plugin_host_calls_total counter\n" +
		`ncgo_plugin_host_calls_total{function="cache_get",plugin="com.example.probe",result="ok"} 2` + "\n"
	if !strings.Contains(out, want) {
		t.Fatalf("render missing series:\nwant %q\ngot:\n%s", want, out)
	}
}

func TestRegistryAddCounter(t *testing.T) {
	r := NewRegistry()
	labels := func() []Label {
		return []Label{
			{Name: "plugin", Value: "com.example.probe"},
			{Name: "op", Value: "write"},
			{Name: "scope", Value: "user"},
		}
	}
	r.AddCounter(MetricPluginStorageBytesTotal, 14, labels()...)
	r.AddCounter(MetricPluginStorageBytesTotal, 6, labels()...)
	// Non-positive deltas are no-ops: no series movement, no new series.
	r.AddCounter(MetricPluginStorageBytesTotal, 0, labels()...)
	r.AddCounter(MetricPluginStorageBytesTotal, -3, labels()...)
	r.AddCounter(MetricPluginStorageBytesTotal, 0,
		Label{Name: "plugin", Value: "p"}, Label{Name: "op", Value: "read"}, Label{Name: "scope", Value: "system"})
	out := render(t, r)
	want := `ncgo_plugin_storage_bytes_total{op="write",plugin="com.example.probe",scope="user"} 20`
	if !strings.Contains(out, want) {
		t.Fatalf("render missing accumulated series %q:\n%s", want, out)
	}
	if strings.Contains(out, `scope="system"`) {
		t.Fatalf("zero delta must not create a series:\n%s", out)
	}
}

func TestRegistryAddCounterConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.AddCounter(MetricPluginStorageBytesTotal, 3,
					Label{Name: "plugin", Value: "p"},
					Label{Name: "op", Value: "read"},
					Label{Name: "scope", Value: "user"})
			}
		}()
	}
	wg.Wait()
	out := render(t, r)
	want := `ncgo_plugin_storage_bytes_total{op="read",plugin="p",scope="user"} 12000`
	if !strings.Contains(out, want) {
		t.Fatalf("missing %q:\n%s", want, out)
	}
}

func TestRegistryAddCounterPanicsOnMisuse(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on unregistered family")
		}
	}()
	r.AddCounter("nope_total", 1, Label{Name: "plugin", Value: "p"})
}

func TestRegistryLabelCanonicalization(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "result", Value: "ok"},
		Label{Name: "plugin", Value: "p"},
		Label{Name: "function", Value: "f"})
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "function", Value: "f"},
		Label{Name: "result", Value: "ok"},
		Label{Name: "plugin", Value: "p"})
	out := render(t, r)
	line := `ncgo_plugin_host_calls_total{function="f",plugin="p",result="ok"} 2`
	if !strings.Contains(out, line) {
		t.Fatalf("label order must canonicalize to one series, got:\n%s", out)
	}
}

func TestRegistryLabelEscaping(t *testing.T) {
	r := NewRegistry()
	r.RegisterCounter("test_escapes_total", "escapes", "plugin", "function", "result")
	r.IncCounter("test_escapes_total",
		Label{Name: "plugin", Value: "quo\"te"},
		Label{Name: "function", Value: "back\\slash"},
		Label{Name: "result", Value: "new\nline"})
	out := render(t, r)
	line := `test_escapes_total{function="back\\slash",plugin="quo\"te",result="new\nline"} 1`
	if !strings.Contains(out, line) {
		t.Fatalf("escaping wrong, got:\n%s", out)
	}
}

func TestRegistryHistogramBuckets(t *testing.T) {
	r := NewRegistry()
	// Boundary values: 0.001 lands in the le="0.001" bucket; 0.0006 in the
	// next one up; 5 only in +Inf.
	for _, v := range []float64{0.001, 0.0006, 5} {
		r.ObserveHistogram(MetricPluginHostCallDurationSeconds, v,
			Label{Name: "plugin", Value: "p"}, Label{Name: "function", Value: "f"})
	}
	out := render(t, r)
	wants := []string{
		"# TYPE ncgo_plugin_host_call_duration_seconds histogram",
		`ncgo_plugin_host_call_duration_seconds_bucket{function="f",plugin="p",le="0.0005"} 0`,
		`ncgo_plugin_host_call_duration_seconds_bucket{function="f",plugin="p",le="0.001"} 2`,
		`ncgo_plugin_host_call_duration_seconds_bucket{function="f",plugin="p",le="0.005"} 2`,
		`ncgo_plugin_host_call_duration_seconds_bucket{function="f",plugin="p",le="2.5"} 2`,
		`ncgo_plugin_host_call_duration_seconds_bucket{function="f",plugin="p",le="+Inf"} 3`,
		fmt.Sprintf(`ncgo_plugin_host_call_duration_seconds_sum{function="f",plugin="p"} %s`,
			formatFloat(0.001+0.0006+5)),
		`ncgo_plugin_host_call_duration_seconds_count{function="f",plugin="p"} 3`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("render missing %q, got:\n%s", w, out)
		}
	}
}

func TestRegistryEntryFamiliesRender(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginEntryCallsTotal,
		Label{Name: "plugin", Value: "com.example.probe"},
		Label{Name: "entry", Value: "ncgo_on_install"},
		Label{Name: "result", Value: "ok"})
	r.ObserveHistogram(MetricPluginEntryCallDurationSeconds, 0.002,
		Label{Name: "plugin", Value: "com.example.probe"},
		Label{Name: "entry", Value: "ncgo_on_install"})
	out := render(t, r)
	want := "# HELP ncgo_plugin_entry_calls_total Per-plugin guest entry-point invocations by result class.\n" +
		"# TYPE ncgo_plugin_entry_calls_total counter\n" +
		`ncgo_plugin_entry_calls_total{entry="ncgo_on_install",plugin="com.example.probe",result="ok"} 1` + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("render missing entry counter family:\nwant %q\ngot:\n%s", want, out)
	}
	wants := []string{
		"# HELP ncgo_plugin_entry_call_duration_seconds Per-plugin guest entry-point latency in seconds.",
		"# TYPE ncgo_plugin_entry_call_duration_seconds histogram",
		`ncgo_plugin_entry_call_duration_seconds_bucket{entry="ncgo_on_install",plugin="com.example.probe",le="0.005"} 1`,
		`ncgo_plugin_entry_call_duration_seconds_bucket{entry="ncgo_on_install",plugin="com.example.probe",le="+Inf"} 1`,
		`ncgo_plugin_entry_call_duration_seconds_count{entry="ncgo_on_install",plugin="com.example.probe"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("render missing %q, got:\n%s", w, out)
		}
	}
}

func TestRegistryZeroSeriesRender(t *testing.T) {
	r := NewRegistry()
	out := render(t, r)
	for _, fam := range []struct{ name, typ string }{
		{MetricPluginHostCallsTotal, "counter"},
		{MetricPluginHostCallDurationSeconds, "histogram"},
		{MetricPluginCapabilityDenialsTotal, "counter"},
		{MetricPluginStorageBytesTotal, "counter"},
		{MetricPluginEntryCallsTotal, "counter"},
		{MetricPluginEntryCallDurationSeconds, "histogram"},
	} {
		if !strings.Contains(out, "# HELP "+fam.name+" ") {
			t.Errorf("zero-series render missing HELP for %s:\n%s", fam.name, out)
		}
		if !strings.Contains(out, "# TYPE "+fam.name+" "+fam.typ) {
			t.Errorf("zero-series render missing TYPE for %s:\n%s", fam.name, out)
		}
		if strings.Contains(out, fam.name+"{") {
			t.Errorf("zero-series render must not emit series for %s:\n%s", fam.name, out)
		}
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			plugin := fmt.Sprintf("p%d", g%2)
			for i := 0; i < 500; i++ {
				r.IncCounter(MetricPluginHostCallsTotal,
					Label{Name: "plugin", Value: plugin},
					Label{Name: "function", Value: "f"},
					Label{Name: "result", Value: "ok"})
				r.ObserveHistogram(MetricPluginHostCallDurationSeconds, 0.001,
					Label{Name: "plugin", Value: plugin},
					Label{Name: "function", Value: "f"})
				var buf bytes.Buffer
				r.Render(&buf)
			}
		}(g)
	}
	wg.Wait()
	out := render(t, r)
	for _, plugin := range []string{"p0", "p1"} {
		want := fmt.Sprintf(`ncgo_plugin_host_calls_total{function="f",plugin="%s",result="ok"} 2000`, plugin)
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
		want = fmt.Sprintf(`ncgo_plugin_host_call_duration_seconds_count{function="f",plugin="%s"} 2000`, plugin)
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestRegistryPanicsOnMisuse(t *testing.T) {
	r := NewRegistry()
	for name, fn := range map[string]func(){
		"unregistered family": func() {
			r.IncCounter("nope_total", Label{Name: "plugin", Value: "p"})
		},
		"undeclared label": func() {
			r.IncCounter(MetricPluginHostCallsTotal,
				Label{Name: "plugin", Value: "p"},
				Label{Name: "function", Value: "f"},
				Label{Name: "bogus", Value: "x"})
		},
		"missing label": func() {
			r.IncCounter(MetricPluginHostCallsTotal,
				Label{Name: "plugin", Value: "p"},
				Label{Name: "function", Value: "f"})
		},
		"counter as histogram": func() {
			r.ObserveHistogram(MetricPluginHostCallsTotal, 1,
				Label{Name: "plugin", Value: "p"},
				Label{Name: "function", Value: "f"},
				Label{Name: "result", Value: "ok"})
		},
		"duplicate family": func() {
			r.RegisterCounter(MetricPluginHostCallsTotal, "dup", "plugin")
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			fn()
		}()
	}
}

func TestClassifyResult(t *testing.T) {
	cases := map[int32]string{
		pluginsdk.ErrCodeOK:               "ok",
		pluginsdk.ErrCodePermissionDenied: "permission_denied",
		pluginsdk.ErrCodeInvalidArgument:  "invalid_argument",
		pluginsdk.ErrCodeNotFound:         "not_found",
		pluginsdk.ErrCodeUnavailable:      "unavailable",
		pluginsdk.ErrCodeUnsupported:      "unsupported",
		pluginsdk.ErrCodeTooLarge:         "too_large",
		pluginsdk.ErrCodeTimeout:          "timeout",
		pluginsdk.ErrCodeCanceled:         "canceled",
		pluginsdk.ErrCodeInternal:         "internal",
		pluginsdk.ErrCodeAlreadyExists:    "internal",
		pluginsdk.ErrCodeQuotaExceeded:    "internal",
		pluginsdk.ErrCodeConflict:         "internal",
		-99:                               "internal",
		1:                                 "ok",
		100:                               "ok",
	}
	for code, want := range cases {
		if got := ClassifyResult(code); got != want {
			t.Errorf("ClassifyResult(%d) = %q, want %q", code, got, want)
		}
	}
	// Every defined code must map to a non-empty class.
	for _, code := range []int32{
		pluginsdk.ErrCodeOK, pluginsdk.ErrCodeInternal, pluginsdk.ErrCodeInvalidArgument,
		pluginsdk.ErrCodePermissionDenied, pluginsdk.ErrCodeNotFound, pluginsdk.ErrCodeAlreadyExists,
		pluginsdk.ErrCodeTimeout, pluginsdk.ErrCodeCanceled, pluginsdk.ErrCodeQuotaExceeded,
		pluginsdk.ErrCodeUnsupported, pluginsdk.ErrCodeConflict, pluginsdk.ErrCodeTooLarge,
		pluginsdk.ErrCodeUnavailable,
	} {
		if ClassifyResult(code) == "" {
			t.Errorf("ClassifyResult(%d) returned empty", code)
		}
	}
}

func TestHandlerNoToken(t *testing.T) {
	r := NewRegistry()
	r.IncCounter(MetricPluginHostCallsTotal,
		Label{Name: "plugin", Value: "p"}, Label{Name: "function", Value: "f"}, Label{Name: "result", Value: "ok"})
	rec := httptest.NewRecorder()
	r.Handler("").ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "ncgo_plugin_host_calls_total") {
		t.Fatalf("body missing series: %s", rec.Body.String())
	}
}

func TestHandlerBearerToken(t *testing.T) {
	r := NewRegistry()
	h := r.Handler("s3cret")
	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"missing", "", 401},
		{"wrong", "Bearer nope", 401},
		{"no scheme", "s3cret", 401},
		{"correct", "Bearer s3cret", 200},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), "GET", "/metrics", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
