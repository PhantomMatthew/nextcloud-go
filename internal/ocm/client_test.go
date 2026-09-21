package ocm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
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

func TestGetWebDAVBasicAuth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/public.php/webdav/", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "ocmtok001" || pass != "" {
			t.Errorf("basic = %v %q %q", ok, user, pass)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"abc"`)
		_, _ = io.WriteString(w, "hello from remote\n")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rc, ent, err := (&Client{HTTP: srv.Client()}).Get(t.Context(), srv.URL, "ocmtok001", "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello from remote\n" {
		t.Fatalf("body = %q %v", body, err)
	}
	if ent == nil || ent.ETag != "abc" || !strings.HasPrefix(ent.ContentType, "text/plain") {
		t.Fatalf("entry = %+v", ent)
	}
}

func TestGetWebDAVNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, _, err := (&Client{HTTP: srv.Client()}).Get(t.Context(), srv.URL, "tok", "/")
	if !errors.Is(err, webdav.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetWebDAVForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, _, err := (&Client{HTTP: srv.Client()}).Get(t.Context(), srv.URL, "tok", "/")
	if !errors.Is(err, webdav.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetWebDAVRejectsScheme(t *testing.T) {
	if _, _, err := NewClient().Get(t.Context(), "file:///tmp", "tok", "/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := NewClient().Get(t.Context(), "", "tok", "/"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty origin = %v", err)
	}
}

func TestGetWebDAVStatus500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	_, _, err := (&Client{HTTP: srv.Client()}).Get(t.Context(), srv.URL, "tok", "/")
	if err == nil || errors.Is(err, webdav.ErrNotImplemented) {
		t.Fatalf("err = %v", err)
	}
}

func TestPropfindAndWrites(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/public.php/webdav/", func(w http.ResponseWriter, r *http.Request) {
		user, _, ok := r.BasicAuth()
		if !ok || user != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
<d:response><d:href>/public.php/webdav/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/public.php/webdav/child.txt</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>5</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`)
		case http.MethodPut:
			w.Header().Set("ETag", `"put"`)
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case "MKCOL":
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client()}
	ents, err := c.Propfind(t.Context(), srv.URL, "tok", "/", 1)
	if err != nil || len(ents) != 2 || ents[1].Path != "/child.txt" {
		t.Fatalf("propfind = %+v %v", ents, err)
	}
	if _, err := c.Propfind(t.Context(), srv.URL, "tok", "/", 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("depth 2 = %v", err)
	}
	ent, err := c.Put(t.Context(), srv.URL, "tok", "/child.txt", strings.NewReader("hello"))
	if err != nil || ent.ETag != "put" {
		t.Fatalf("put = %+v %v", ent, err)
	}
	if err := c.Delete(t.Context(), srv.URL, "tok", "/child.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkcol(t.Context(), srv.URL, "tok", "/sub"); err != nil {
		t.Fatal(err)
	}
}
