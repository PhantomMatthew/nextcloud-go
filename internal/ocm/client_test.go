package ocm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiscoverFallsBackToProvider(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ocm", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	var providerHits int
	mux.HandleFunc("/ocm-provider", func(w http.ResponseWriter, r *http.Request) {
		providerHits++
		w.Header().Set("Content-Type", "application/json")
		ep := "http://" + r.Host + "/ocm"
		_, _ = io.WriteString(w, `{"enabled":true,"apiVersion":"1.0-proposal1","endPoint":"`+ep+`"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client()}
	got, err := c.Discover(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := srv.URL + "/ocm"
	if got != want {
		t.Fatalf("endpoint = %q want %q", got, want)
	}
	if providerHits != 1 {
		t.Fatalf("provider hits = %d", providerHits)
	}
}

func TestNotifyOutgoingNon2xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ocm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		ep := "http://" + r.Host + "/ocm"
		_, _ = io.WriteString(w, `{"enabled":true,"endPoint":"`+ep+`"}`)
	})
	mux.HandleFunc("/ocm/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client()}
	ep, err := c.Discover(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = c.NotifyOutgoing(t.Context(), ep, OutgoingNotice{
		ShareWith:    "bob@remote.example.com",
		Name:         "hello.txt",
		ProviderID:   "1",
		Owner:        "alice@http://cloud.example.com",
		Sender:       "alice@http://cloud.example.com",
		ResourceType: "file",
		Token:        "ncgopublic00001",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("notify = %v", err)
	}
}

func TestNotifyOutgoingOK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ocm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":true,"endPoint":"http://`+r.Host+`/ocm"}`)
	})
	mux.HandleFunc("/ocm/shares", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["shareType"] != "user" || body["name"] != "hello.txt" {
			t.Fatalf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"recipientDisplayName":"bob"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client()}
	ep, err := c.Discover(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.NotifyOutgoing(t.Context(), ep, OutgoingNotice{
		ShareWith: "bob@" + srv.URL, Name: "hello.txt", ProviderID: "9",
		Owner: "alice", Sender: "alice", ResourceType: "file", Token: "tok15charsxxxx",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSplitCloudIDAndOrigin(t *testing.T) {
	uid, remote := SplitCloudID("bob@https://remote.example.com")
	if uid != "bob" || remote != "https://remote.example.com" {
		t.Fatalf("split = %q %q", uid, remote)
	}
	if got := NormalizeOrigin("remote.example.com"); got != "https://remote.example.com" {
		t.Fatalf("origin = %q", got)
	}
	if got := NormalizeOrigin("https://remote.example.com/"); got != "https://remote.example.com" {
		t.Fatalf("origin trim = %q", got)
	}
	if uid, remote := SplitCloudID("no-at"); uid != "" || remote != "" {
		t.Fatalf("empty split = %q %q", uid, remote)
	}
}

func TestDiscoverEmptyOrigin(t *testing.T) {
	if _, err := (*Client)(nil).Discover(t.Context(), "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestNotifyOutgoingMissingFields(t *testing.T) {
	if err := NewClient().NotifyOutgoing(t.Context(), "http://x", OutgoingNotice{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitCloudIDTrailingAt(t *testing.T) {
	if uid, remote := SplitCloudID("bob@"); uid != "" || remote != "" {
		t.Fatalf("split = %q %q", uid, remote)
	}
}

func TestDiscoverRejectsDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"enabled":false,"endPoint":"http://`+r.Host+`/ocm"}`)
	}))
	t.Cleanup(srv.Close)
	if _, err := (&Client{HTTP: srv.Client()}).Discover(t.Context(), srv.URL); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestNormalizeOriginEmpty(t *testing.T) {
	if got := NormalizeOrigin("  "); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDiscoverJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not-json")
	}))
	t.Cleanup(srv.Close)
	if _, err := (&Client{HTTP: srv.Client()}).Discover(t.Context(), srv.URL); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitCloudIDSpaces(t *testing.T) {
	uid, remote := SplitCloudID("  bob@host  ")
	if uid != "bob" || remote != "host" {
		t.Fatalf("split = %q %q", uid, remote)
	}
}

func TestNotifyUsesDefaultClient(t *testing.T) {
	// cover httpc nil receiver timeout client construction without a network call
	c := &Client{}
	if c.httpc() == nil {
		t.Fatal("httpc")
	}
	if strings.Contains(NormalizeOrigin("http://h"), "https://http") {
		t.Fatal("scheme preserved")
	}
}
