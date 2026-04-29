package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
)

const sampleHAR = `{
  "log": {
    "version": "1.2",
    "creator": {"name": "mitmproxy", "version": "10.0"},
    "entries": [
      {
        "startedDateTime": "2025-04-29T10:00:00.000Z",
        "request": {
          "method": "GET",
          "url": "https://cloud.example.com/ocs/v2.php/cloud/capabilities?format=json",
          "httpVersion": "HTTP/1.1",
          "headers": [
            {"name": ":authority", "value": "cloud.example.com"},
            {"name": "Host", "value": "cloud.example.com"},
            {"name": "User-Agent", "value": "mitm-test/1.0"},
            {"name": "OCS-APIRequest", "value": "true"},
            {"name": "Authorization", "value": "Basic dGVzdDp0ZXN0"},
            {"name": "Cookie", "value": "oc_sessionPassphrase=secret"}
          ]
        },
        "response": {
          "status": 200,
          "statusText": "OK",
          "httpVersion": "HTTP/1.1",
          "headers": [
            {"name": "Date", "value": "Wed, 29 Apr 2026 10:00:01 GMT"},
            {"name": "Server", "value": "Apache"},
            {"name": "Content-Type", "value": "application/json; charset=utf-8"},
            {"name": "Set-Cookie", "value": "oc_sessionPassphrase=xyz; path=/"},
            {"name": "X-Request-Id", "value": "abc123"}
          ],
          "content": {
            "size": 96,
            "mimeType": "application/json",
            "text": "{\"ocs\":{\"meta\":{\"status\":\"ok\",\"statuscode\":200,\"message\":\"OK\"},\"data\":{\"version\":{\"major\":29}}}}"
          }
        }
      }
    ]
  }
}`

func TestImportHAR_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "sample.har")
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(in, []byte(sampleHAR), 0o644); err != nil {
		t.Fatalf("write har: %v", err)
	}
	if code := runImportHAR([]string{"-in", in, "-out", out, "-group", "capabilities"}); code != 0 {
		t.Fatalf("runImportHAR exit code = %d", code)
	}

	caseDir := filepath.Join(out, "capabilities", "001-get-capabilities")
	for _, f := range []string{"case.yaml", "request.http", "response.http"} {
		if _, err := os.Stat(filepath.Join(caseDir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}

	c, err := goldentest.Load(caseDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	req, err := goldentest.ParseRequest(c.RequestRaw)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Method != "GET" {
		t.Errorf("method = %q, want GET", req.Method)
	}
	if req.Path != "/ocs/v2.php/cloud/capabilities?format=json" {
		t.Errorf("path = %q", req.Path)
	}
	if got := req.Headers.Get("Cookie"); got != "" {
		t.Errorf("Cookie not scrubbed: %q", got)
	}
	if got := req.Headers.Get("Authorization"); got != "" {
		t.Errorf("Authorization not scrubbed: %q", got)
	}
	if got := req.Headers.Get("OCS-APIRequest"); got != "true" {
		t.Errorf("OCS-APIRequest = %q, want true", got)
	}

	resp, err := goldentest.ParseResponse(c.ResponseRaw)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if got := resp.Headers.Get("Server"); got != "nginx" {
		t.Errorf("Server = %q, want nginx", got)
	}
	for _, h := range []string{"Date", "X-Request-Id", "Set-Cookie"} {
		if got := resp.Headers.Get(h); got != "" {
			t.Errorf("%s not dropped: %q", h, got)
		}
	}
	if !bytes.Contains(resp.Body, []byte(`"statuscode":200`)) {
		t.Errorf("body missing OCS payload: %s", resp.Body)
	}

	clHeader := resp.Headers.Get("Content-Length")
	cl, err := strconv.Atoi(clHeader)
	if err != nil {
		t.Fatalf("Content-Length not numeric: %q", clHeader)
	}
	if cl != len(resp.Body) {
		t.Errorf("Content-Length = %d, body = %d", cl, len(resp.Body))
	}

	yamlBytes, err := os.ReadFile(filepath.Join(caseDir, "case.yaml"))
	if err != nil {
		t.Fatalf("read case.yaml: %v", err)
	}
	yaml := string(yamlBytes)
	for _, want := range []string{
		"id: capabilities/001-get-capabilities",
		"capture: mitmproxy-har",
		"body_kind: json",
		"tags: [har, imported]",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("case.yaml missing %q\n---\n%s", want, yaml)
		}
	}
}
