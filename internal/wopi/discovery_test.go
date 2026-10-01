package wopi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const discoveryFixture = `<?xml version="1.0" encoding="utf-8"?>
<wopi-discovery>
  <net-zone name="external-http">
    <app name="application/vnd.oasis.opendocument.text" favIconUrl="http://colla.test/browser/images/odt.svg">
      <action name="edit" ext="odt" urlsrc="http://colla.test/browser/abc123/cool.html?"/>
      <action name="view" ext="odt" urlsrc="http://colla.test/browser/abc123/cool.html?readonly=1&amp;"/>
    </app>
    <app name="application/vnd.openxmlformats-officedocument.wordprocessingml.document">
      <action name="edit" ext="docx" urlsrc="http://colla.test/browser/abc123/cool.html?"/>
    </app>
    <app name="image/x-png">
      <action name="view" ext="png" urlsrc="http://colla.test/browser/abc123/cool.html?view=1&amp;"/>
    </app>
  </net-zone>
</wopi-discovery>`

func discoveryServer(t *testing.T, body string, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hosting/discovery" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(body))
	}))
}

func TestDiscoveryActionURL(t *testing.T) {
	srv := discoveryServer(t, discoveryFixture, nil)
	defer srv.Close()
	d := &Discovery{BaseURL: srv.URL}
	ctx := t.Context()

	u, err := d.ActionURL(ctx, "odt", true)
	if err != nil || u != "http://colla.test/browser/abc123/cool.html?" {
		t.Fatalf("edit odt = %q %v", u, err)
	}
	u, err = d.ActionURL(ctx, "odt", false)
	if err != nil || u != "http://colla.test/browser/abc123/cool.html?readonly=1&" {
		t.Fatalf("view odt = %q %v", u, err)
	}
	// No view action offered: read-only falls back to the edit URL (the
	// token still blocks writes server-side).
	u, err = d.ActionURL(ctx, "docx", false)
	if err != nil || u != "http://colla.test/browser/abc123/cool.html?" {
		t.Fatalf("view-fallback docx = %q %v", u, err)
	}
	// A view-only extension resolves for read-only use.
	u, err = d.ActionURL(ctx, "png", false)
	if err != nil || u != "http://colla.test/browser/abc123/cool.html?view=1&" {
		t.Fatalf("view png = %q %v", u, err)
	}
	if _, err := d.ActionURL(ctx, "xyz", true); !errors.Is(err, ErrNoDiscoveryAction) {
		t.Fatalf("unknown ext = %v, want ErrNoDiscoveryAction", err)
	}
	var nilD *Discovery
	if _, err := nilD.ActionURL(ctx, "odt", true); err == nil {
		t.Fatal("nil discovery: expected error")
	}
}

func TestDiscoveryCaching(t *testing.T) {
	hits := 0
	srv := discoveryServer(t, discoveryFixture, &hits)
	defer srv.Close()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d := &Discovery{BaseURL: srv.URL, Clock: func() time.Time { return now }}
	ctx := t.Context()

	if _, err := d.ActionURL(ctx, "odt", true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ActionURL(ctx, "docx", true); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (cached)", hits)
	}
	// Past the TTL the document refetches.
	now = now.Add(2 * time.Hour)
	if _, err := d.ActionURL(ctx, "odt", true); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("hits after TTL = %d, want 2", hits)
	}
}

func TestDiscoveryFetchFailures(t *testing.T) {
	ctx := t.Context()

	srv := discoveryServer(t, "not xml at all", nil)
	srv.Close() // connection refused
	d := &Discovery{BaseURL: srv.URL}
	if _, err := d.ActionURL(ctx, "odt", true); err == nil {
		t.Fatal("unreachable: expected error")
	}

	bad := discoveryServer(t, "not xml at all", nil)
	defer bad.Close()
	d = &Discovery{BaseURL: bad.URL}
	if _, err := d.ActionURL(ctx, "odt", true); err == nil {
		t.Fatal("malformed xml: expected error")
	}

	empty := discoveryServer(t, `<?xml version="1.0"?><wopi-discovery/>`, nil)
	defer empty.Close()
	d = &Discovery{BaseURL: empty.URL}
	if _, err := d.ActionURL(ctx, "odt", true); err == nil {
		t.Fatal("no actions: expected error")
	}
}
