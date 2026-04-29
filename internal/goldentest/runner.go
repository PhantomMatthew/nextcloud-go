package goldentest

import (
	"bufio"
	"bytes"
	"context"
	"io"
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
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(c.RequestRaw)))
	if err != nil {
		t.Fatalf("goldentest: parse request: %v", err)
	}
	req.RequestURI = ""
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantParsed, err := ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("goldentest: parse golden response: %v", err)
	}
	gotParsed := &ParsedResponse{
		Status:  rec.Code,
		Headers: rec.Result().Header.Clone(),
		Body:    rec.Body.Bytes(),
	}
	compareOrFail(t, c, wantParsed, gotParsed)
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
	httpReq, err := http.NewRequestWithContext(ctx, parsedReq.Method, target.String(), bytes.NewReader(parsedReq.Body))
	if err != nil {
		t.Fatalf("goldentest: build request: %v", err)
	}
	for k, vs := range parsedReq.Headers {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("goldentest: do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("goldentest: read response: %v", err)
	}
	wantParsed, err := ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("goldentest: parse golden response: %v", err)
	}
	gotParsed := &ParsedResponse{
		Status:  resp.StatusCode,
		Headers: resp.Header.Clone(),
		Body:    body,
	}
	compareOrFail(t, c, wantParsed, gotParsed)
}

func compareOrFail(t *testing.T, c *Case, want, got *ParsedResponse) {
	t.Helper()
	wantNorm, err := Normalize(c, want)
	if err != nil {
		t.Fatalf("goldentest: normalize want: %v", err)
	}
	gotNorm, err := Normalize(c, got)
	if err != nil {
		t.Fatalf("goldentest: normalize got: %v", err)
	}
	if d := Diff(wantNorm, gotNorm); d != "" {
		t.Fatalf("goldentest: case %s mismatch:\n%s", c.ID, d)
	}
}
