package ocm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/users"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

type discoveryDoc struct {
	Enabled       bool                `json:"enabled"`
	APIVersion    string              `json:"apiVersion"`
	EndPoint      string              `json:"endPoint"`
	ResourceTypes []discoveryResource `json:"resourceTypes"`
}

type discoveryResource struct {
	Name       string             `json:"name"`
	ShareTypes []string           `json:"shareTypes"`
	Protocols  discoveryProtocols `json:"protocols"`
}

type discoveryProtocols struct {
	WebDAV string `json:"webdav"`
}

// Discovery serves GET/HEAD /.well-known/ocm and /ocm-provider.
func Discovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	doc := discoveryDoc{
		Enabled:    true,
		APIVersion: "1.0-proposal1",
		EndPoint:   RequestBaseURL(r) + "/ocm",
		ResourceTypes: []discoveryResource{{
			Name:       "file",
			ShareTypes: []string{"user"},
			Protocols:  discoveryProtocols{WebDAV: "/public.php/webdav/"},
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// RequestBaseURL is the scheme://host of this request (X-Forwarded-* honored).
func RequestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = h
	}
	return scheme + "://" + host
}

type incomingShareRequest struct {
	ShareWith        string           `json:"shareWith"`
	Name             string           `json:"name"`
	ProviderID       string           `json:"providerId"`
	Owner            string           `json:"owner"`
	Sender           string           `json:"sender"`
	OwnerDisplayName string           `json:"ownerDisplayName"`
	ShareType        string           `json:"shareType"`
	ResourceType     string           `json:"resourceType"`
	Protocol         incomingProtocol `json:"protocol"`
}

type incomingProtocol struct {
	Name    string                  `json:"name"`
	Options incomingProtocolOptions `json:"options"`
	WebDAV  *incomingWebDAV         `json:"webdav"`
}

type incomingProtocolOptions struct {
	SharedSecret string `json:"sharedSecret"`
	Permissions  any    `json:"permissions"`
}

type incomingWebDAV struct {
	SharedSecret string `json:"sharedSecret"`
}

// IncomingHandler serves POST /ocm/shares.
type IncomingHandler struct {
	Store Store
	Users users.Store
}

func (h IncomingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/ocm"), "/")
	if rest != "shares" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req incomingShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOCMError(w, http.StatusBadRequest, "invalid json")
		return
	}
	in, display, err := h.buildIncoming(r, &req)
	if err != nil {
		writeIncomingErr(w, err)
		return
	}
	if err := h.Store.Insert(r.Context(), in); err != nil {
		writeIncomingErr(w, err)
		return
	}
	body, err := json.Marshal(struct {
		RecipientDisplayName string `json:"recipientDisplayName"`
	}{RecipientDisplayName: display})
	if err != nil {
		http.Error(w, "render", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func (h IncomingHandler) buildIncoming(r *http.Request, req *incomingShareRequest) (*Incoming, string, error) {
	uid := localUID(req.ShareWith)
	if uid == "" {
		return nil, "", fmtInvalid("shareWith")
	}
	u, err := h.Users.GetByUID(r.Context(), uid)
	if err != nil {
		return nil, "", fmtInvalid("shareWith")
	}
	if req.ShareType != "user" {
		return nil, "", ErrUnsupported
	}
	switch req.ResourceType {
	case "file", "folder":
	default:
		return nil, "", ErrUnsupported
	}
	if req.Protocol.Name != "webdav" && req.Protocol.Name != "" {
		return nil, "", ErrUnsupported
	}
	if req.Protocol.Name == "" && req.Protocol.WebDAV == nil {
		return nil, "", ErrUnsupported
	}
	secret := strings.TrimSpace(req.Protocol.Options.SharedSecret)
	if secret == "" && req.Protocol.WebDAV != nil {
		secret = strings.TrimSpace(req.Protocol.WebDAV.SharedSecret)
	}
	if secret == "" {
		return nil, "", fmtInvalid("sharedSecret")
	}
	name := strings.TrimSpace(req.Name)
	if !validShareName(name) {
		return nil, "", fmtInvalid("name")
	}
	remote := remoteFromOwner(req.Owner)
	if remote == "" {
		return nil, "", fmtInvalid("owner")
	}
	if strings.TrimSpace(req.ProviderID) == "" {
		return nil, "", fmtInvalid("providerId")
	}
	display := u.DisplayName
	if display == "" {
		display = u.UID
	}
	return &Incoming{
		UserID:      u.ID,
		UserUID:     u.UID,
		Name:        name,
		Remote:      remote,
		RemoteID:    strings.TrimSpace(req.ProviderID),
		Owner:       strings.TrimSpace(req.Owner),
		Token:       secret,
		ItemType:    req.ResourceType,
		Permissions: parseIncomingPermissions(req.Protocol.Options.Permissions),
		Accepted:    1,
	}, display, nil
}

func parseIncomingPermissions(v any) int {
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case json.Number:
		i, err := t.Int64()
		if err != nil {
			return webdav.PermRead
		}
		n = int(i)
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return webdav.PermRead
		}
		n = i
	default:
		return webdav.PermRead
	}
	n &= webdav.PermAll
	if n == 0 {
		return webdav.PermRead
	}
	return n
}

func fmtInvalid(field string) error {
	return errorsJoinInvalid(field)
}

func errorsJoinInvalid(field string) error {
	return &invalidFieldError{field: field}
}

type invalidFieldError struct {
	field string
}

func (e *invalidFieldError) Error() string { return "ocm: invalid " + e.field }

func (e *invalidFieldError) Unwrap() error { return ErrInvalid }

func localUID(shareWith string) string {
	s := strings.TrimSpace(shareWith)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i]
	}
	return s
}

func remoteFromOwner(owner string) string {
	s := strings.TrimSpace(owner)
	if i := strings.LastIndex(s, "@"); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	return ""
}

func validShareName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return true
}

func writeIncomingErr(w http.ResponseWriter, err error) {
	switch {
	case errorsIsInvalid(err):
		writeOCMError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeOCMError(w, http.StatusConflict, "share already exists")
	case errors.Is(err, ErrUnsupported):
		writeOCMError(w, http.StatusNotImplemented, "unsupported")
	default:
		writeOCMError(w, http.StatusInternalServerError, "internal error")
	}
}

func errorsIsInvalid(err error) bool {
	return errors.Is(err, ErrInvalid)
}

func writeOCMError(w http.ResponseWriter, status int, message string) {
	body, err := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: message})
	if err != nil {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
