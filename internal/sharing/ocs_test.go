package sharing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

type stubHasher struct{}

func (stubHasher) Hash(pw string) (string, error)       { return "h:" + pw, nil }
func (stubHasher) Verify(hash, pw string) (bool, error) { return hash == "h:"+pw, nil }
func (stubHasher) NeedsRehash(string) bool              { return false }

func testService(t *testing.T) *Service {
	t.Helper()
	ctx := t.Context()
	db := testDB(t)
	us := users.NewSQLStore(db)
	u := &users.User{UID: "alice", DisplayName: "Alice", PasswordHash: "x", Enabled: true}
	if err := us.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := us.Create(ctx, &users.User{UID: "bob", DisplayName: "Bob", PasswordHash: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	st, err := localfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dav := files.NewDAV(st, files.NewSQLStore(db), us)
	freeze := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	dav.Clock = func() time.Time { return freeze }
	if _, _, err := dav.Write(ctx, "alice", "/a.txt", strings.NewReader("hello"), &freeze); err != nil {
		t.Fatal(err)
	}
	if _, err := dav.Mkdir(ctx, "alice", "/pub"); err != nil {
		t.Fatal(err)
	}
	return &Service{
		Store:    NewSQLShareStore(db),
		Files:    dav,
		Users:    us,
		Hasher:   stubHasher{},
		Clock:    func() time.Time { return freeze },
		NewToken: func() string { return "ncgopublic00001" },
	}
}

func withUser(r *http.Request) *http.Request {
	return r.WithContext(auth.WithUser(r.Context(), &auth.Principal{UID: "alice", DisplayName: "Alice", Enabled: true}))
}

func TestOCSCreateLinkAndRejectOtherTypes(t *testing.T) {
	svc := testService(t)
	h := Handler{Service: svc, Version: ocs.V2}

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=6"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("shareType 6 status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "unknown sharee") {
		t.Fatalf("body = %s", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=3"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data := env["ocs"].(map[string]any)["data"].(map[string]any)
	if data["token"] != "ncgopublic00001" {
		t.Fatalf("token = %v", data["token"])
	}
	if data["url"] != "https://cloud.example.com/s/ncgopublic00001" {
		t.Fatalf("url = %v", data["url"])
	}
	if data["share_type"] != float64(3) {
		t.Fatalf("share_type = %v", data["share_type"])
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", nil)
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/ocs/v2.php/apps/files_sharing/api/v1/shares/1?format=json", nil)
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOCSCreateUserShare(t *testing.T) {
	svc := testService(t)
	svc.Files.Incoming = svc
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=0&shareWith=bob"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data := env["ocs"].(map[string]any)["data"].(map[string]any)
	if data["share_type"] != float64(0) || data["share_with"] != "bob" {
		t.Fatalf("payload = %v", data)
	}
	if data["url"] != "" {
		t.Fatalf("url = %v", data["url"])
	}
	e, err := svc.Files.Stat(t.Context(), "bob", "/a.txt")
	if err != nil || !e.Shared {
		t.Fatalf("bob stat = %+v %v", e, err)
	}
}

func TestOCSMissingPath404(t *testing.T) {
	svc := testService(t)
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/missing.txt&shareType=3"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestServiceExpireDeletes(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	sh, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "", "2020-01-01", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetForOwner(ctx, "alice", sh.ID); err == nil {
		t.Fatal("expected expired share gone")
	}
}

func TestResolvePublicPassword(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "secret", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ResolvePublic(ctx, "ncgopublic00001", ""); err == nil {
		t.Fatal("expected unauthorized")
	}
	if _, _, err := svc.ResolvePublic(ctx, "ncgopublic00001", "secret"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicLinkGET(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	h := svc.PublicLinkHandler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/s/ncgopublic00001", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestTokenVerifier(t *testing.T) {
	svc := testService(t)
	ctx := t.Context()
	if _, err := svc.Create(ctx, "alice", "/a.txt", files.ShareTypeLink, 0, "", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	v := &TokenVerifier{Service: svc}
	p, err := v.Verify(ctx, "ncgopublic00001", "")
	if err != nil || p.UID != "ncgopublic00001" {
		t.Fatalf("verify = %+v %v", p, err)
	}
	if _, err := v.Verify(ctx, "missing", ""); err == nil {
		t.Fatal("expected fail")
	}
}

func TestOCSCreateUnknownShareType(t *testing.T) {
	svc := testService(t)
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=4"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown share type") {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

type remoteOCMTransport struct{}

func (remoteOCMTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() != "remote.example.com" {
		return nil, errors.New("connection refused")
	}
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	switch {
	case req.Method == http.MethodGet && (req.URL.Path == "/.well-known/ocm" || req.URL.Path == "/ocm-provider"):
		body := `{"enabled":true,"apiVersion":"1.0-proposal1","endPoint":"https://remote.example.com/ocm"}`
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	case req.Method == http.MethodPost && req.URL.Path == "/ocm/shares":
		return &http.Response{StatusCode: http.StatusCreated, Header: hdr, Body: io.NopCloser(bytes.NewReader([]byte(`{"recipientDisplayName":"bob"}`))), Request: req}, nil
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Header: hdr, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
}

func TestOCSCreateRemoteShare(t *testing.T) {
	svc := testService(t)
	svc.OCM = &ocm.Client{HTTP: &http.Client{Transport: remoteOCMTransport{}}}
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=6&shareWith=bob@https://remote.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	data := env["ocs"].(map[string]any)["data"].(map[string]any)
	if data["share_type"] != float64(6) || data["share_with"] != "bob@https://remote.example.com" {
		t.Fatalf("payload = %v", data)
	}
	if data["url"] != "" {
		t.Fatalf("url = %v", data["url"])
	}
}

func TestOCSCreateRemoteUnreachableRollsBack(t *testing.T) {
	svc := testService(t)
	svc.OCM = &ocm.Client{HTTP: &http.Client{Transport: remoteOCMTransport{}}}
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=6&shareWith=bob@https://no-such-ocm.invalid"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "cannot federate share") {
		t.Fatalf("body = %s", rr.Body.String())
	}
	u, err := svc.Users.GetByUID(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.Store.ListByOwner(t.Context(), u.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("leftover shares = %+v", listed)
	}
}

func TestOCSCreateRemoteSelfHostRejected(t *testing.T) {
	svc := testService(t)
	svc.OCM = &ocm.Client{HTTP: &http.Client{Transport: remoteOCMTransport{}}}
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=6&shareWith=bob@http://cloud.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "unknown sharee") {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateRemoteWithoutOCMClient(t *testing.T) {
	svc := testService(t)
	_, err := svc.Create(t.Context(), "alice", "/a.txt", files.ShareTypeRemote, 0, "bob@https://remote.example.com", "", "", "", "http://cloud.example.com")
	if !errors.Is(err, errFederate) {
		t.Fatalf("err = %v", err)
	}
	u, err := svc.Users.GetByUID(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.Store.ListByOwner(t.Context(), u.ID, "")
	if err != nil || len(listed) != 0 {
		t.Fatalf("leftover = %+v %v", listed, err)
	}
}

type notifyFailTransport struct{}

func (notifyFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/json")
	if req.Method == http.MethodGet {
		body := `{"enabled":true,"endPoint":"https://remote.example.com/ocm"}`
		return &http.Response{StatusCode: http.StatusOK, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusBadGateway, Header: hdr, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func TestOCSCreateRemoteNotifyFailRollsBack(t *testing.T) {
	svc := testService(t)
	svc.OCM = &ocm.Client{HTTP: &http.Client{Transport: notifyFailTransport{}}}
	h := Handler{Service: svc, Version: ocs.V2}
	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/ocs/v2.php/apps/files_sharing/api/v1/shares?format=json", strings.NewReader("path=/a.txt&shareType=6&shareWith=bob@https://remote.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "cloud.example.com"
	h.ServeHTTP(rr, withUser(req))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "cannot federate share") {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	u, err := svc.Users.GetByUID(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.Store.ListByOwner(t.Context(), u.ID, "")
	if err != nil || len(listed) != 0 {
		t.Fatalf("leftover = %+v %v", listed, err)
	}
}
