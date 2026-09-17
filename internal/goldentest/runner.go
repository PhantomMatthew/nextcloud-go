package goldentest

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func RunHandler(t *testing.T, c *Case, h http.Handler) {
	t.Helper()
	if c == nil || h == nil {
		t.Fatalf("goldentest: nil case or handler")
	}
	got, err := Execute(context.Background(), c, func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), nil
	})
	if err != nil {
		t.Fatalf("goldentest: execute: %v", err)
	}
	wantParsed, err := ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("goldentest: parse golden response: %v", err)
	}
	if err := Compare(c, wantParsed, got); err != nil {
		t.Fatal(err)
	}
}

func RunHTTP(ctx context.Context, t *testing.T, c *Case, baseURL string) {
	t.Helper()
	if c == nil {
		t.Fatalf("goldentest: nil case")
	}
	parsedReq, err := ParseRequest(c.RequestRaw)
	if err != nil {
		t.Fatalf("goldentest: parse request: %v", err)
	}
	target, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("goldentest: parse baseURL: %v", err)
	}
	target.Path = parsedReq.Path
	got, err := Execute(ctx, c, func(_ *http.Request) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, parsedReq.Method, target.String(), bytes.NewReader(parsedReq.Body))
		if err != nil {
			return nil, err
		}
		for k, vs := range parsedReq.Headers {
			for _, v := range vs {
				httpReq.Header.Add(k, v)
			}
		}
		return http.DefaultClient.Do(httpReq)
	})
	if err != nil {
		t.Fatalf("goldentest: do request: %v", err)
	}
	wantParsed, err := ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("goldentest: parse golden response: %v", err)
	}
	if err := Compare(c, wantParsed, got); err != nil {
		t.Fatal(err)
	}
}
