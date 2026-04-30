package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestID_GeneratesAndPropagates(t *testing.T) {
	t.Parallel()

	var captured string
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(HeaderRequestID); got == "" || got != captured {
		t.Fatalf("response header = %q, ctx = %q; want equal non-empty", got, captured)
	}
	if len(captured) != requestIDLength {
		t.Fatalf("request id length = %d; want %d", len(captured), requestIDLength)
	}
	for _, c := range captured {
		if !strings.ContainsRune(requestIDAlphabet, c) {
			t.Fatalf("request id contains invalid char %q", c)
		}
	}
}

func TestRequestID_PreservesIncomingValue(t *testing.T) {
	t.Parallel()

	const incoming = "AbCdEfGhIjKlMnOpQrSt"
	var captured string
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, incoming)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if captured != incoming || rec.Header().Get(HeaderRequestID) != incoming {
		t.Fatalf("incoming id not preserved: ctx=%q header=%q", captured, rec.Header().Get(HeaderRequestID))
	}
}

func TestSecurityHeaders_DefaultSet(t *testing.T) {
	t.Parallel()

	handler := SecurityHeaders(DefaultSecurityHeaders())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	wantHeaders := map[string]string{
		"X-Content-Type-Options":            "nosniff",
		"X-Frame-Options":                   "SAMEORIGIN",
		"X-Permitted-Cross-Domain-Policies": "none",
		"X-Robots-Tag":                      "noindex, nofollow",
		"Referrer-Policy":                   "no-referrer",
		"Feature-Policy":                    "autoplay 'self';camera 'none';fullscreen 'self';geolocation 'none';microphone 'none';payment 'none'",
	}
	for k, v := range wantHeaders {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q; want %q", k, got, v)
		}
	}
}

func TestRecover_WritesFiveHundredOnPanic(t *testing.T) {
	t.Parallel()

	handler := Chain(
		RequestID(),
		Recover(nil),
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q; want text/plain prefix", ct)
	}
}

func TestChain_OutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, "in:"+name)
				next.ServeHTTP(w, r)
				order = append(order, "out:"+name)
			})
		}
	}
	handler := Chain(mw("a"), mw("b"), mw("c"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"in:a", "in:b", "in:c", "handler", "out:c", "out:b", "out:a"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v; want %v", order, want)
	}
}

func TestMaintenance_OCSReturns503Envelope(t *testing.T) {
	t.Parallel()

	enabled := MaintenanceFunc(func() bool { return true })
	handler := Maintenance(enabled)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler must not be invoked in maintenance mode")
	}))

	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	if rec.Header().Get(HeaderMaintenanceMode) != "1" {
		t.Errorf("missing %s: 1 header", HeaderMaintenanceMode)
	}
	if rec.Header().Get("Retry-After") != maintenanceRetryAfter {
		t.Errorf("Retry-After = %q; want %q", rec.Header().Get("Retry-After"), maintenanceRetryAfter)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"statuscode":503`) {
		t.Errorf("body missing OCS statuscode 503: %s", body)
	}
}

func TestMaintenance_StatusPHPBypasses(t *testing.T) {
	t.Parallel()

	called := false
	enabled := MaintenanceFunc(func() bool { return true })
	handler := Maintenance(enabled)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status.php", nil))

	if !called {
		t.Fatal("status.php must bypass maintenance")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
}

func TestMaintenance_DisabledPassthrough(t *testing.T) {
	t.Parallel()

	enabled := MaintenanceFunc(func() bool { return false })
	called := false
	handler := Maintenance(enabled)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/ocs/v1.php/foo", nil))
	if !called {
		t.Fatal("disabled maintenance must pass through")
	}
}

func TestCSRF_BypassesForOCSRequest(t *testing.T) {
	t.Parallel()

	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/ocs/v2.php/foo", nil)
	req.Header.Set(HeaderOCSAPIRequest, "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("OCS-APIRequest must bypass CSRF; got %d", rec.Code)
	}
}

func TestCSRF_BypassesForBearer(t *testing.T) {
	t.Parallel()

	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Header.Set("Authorization", "Bearer abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer must bypass CSRF; got %d", rec.Code)
	}
}

func TestCSRF_RejectsUnsafeWithoutToken(t *testing.T) {
	t.Parallel()

	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach next handler")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d; want 412", rec.Code)
	}
}

func TestCSRF_SafeMethodPassthrough(t *testing.T) {
	t.Parallel()

	called := false
	handler := CSRF(CSRFConfig{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Fatal("GET must pass through CSRF")
	}
}

func TestResponseRecorder_DefaultsToTwoHundred(t *testing.T) {
	t.Parallel()

	var rec httptest.ResponseRecorder
	rr := &responseRecorder{ResponseWriter: &rec}
	_, _ = rr.Write([]byte("hi"))
	if rr.status != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.status)
	}
	if rr.bytes != 2 {
		t.Fatalf("bytes = %d; want 2", rr.bytes)
	}
}

func TestRequestIDFromContext_EmptyWhenAbsent(t *testing.T) {
	t.Parallel()
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}
