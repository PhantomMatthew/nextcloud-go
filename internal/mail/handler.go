package mail

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

// AccountsPrefix is the JSON REST mount path mirroring the official
// Nextcloud Mail app (ADR-0108 §2). The router has no path parameters, so
// the handler is registered as a prefix route and parses the tail itself
// (the WOPI FilesHandler pattern).
const AccountsPrefix = "/apps/mail/api/accounts"

// Handler serves the session-authed accounts API:
//
//	POST   /apps/mail/api/accounts        create (201)
//	GET    /apps/mail/api/accounts        list the caller's accounts
//	GET    /apps/mail/api/accounts/{id}   one account (+ "mailboxes": [] placeholder)
//	PUT    /apps/mail/api/accounts/{id}   partial update
//	DELETE /apps/mail/api/accounts/{id}   delete (200 {})
//
// Passwords never appear in any response; cross-user rows are 404, not 403.
type Handler struct {
	Svc *Service
}

// accountResponse is the account JSON shape (official app field names).
// It NEVER carries password material.
type accountResponse struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	EmailAddress string `json:"emailAddress"`
	IMAPHost     string `json:"imapHost"`
	IMAPPort     int    `json:"imapPort"`
	IMAPSSLMode  string `json:"imapSslMode"`
	IMAPUser     string `json:"imapUser"`
	SMTPHost     string `json:"smtpHost"`
	SMTPPort     int    `json:"smtpPort"`
	SMTPSSLMode  string `json:"smtpSslMode"`
	SMTPUser     string `json:"smtpUser"`
}

// accountDetailResponse adds the forward-compat mailboxes placeholder the
// official app's single-account response carries (populated by M2 sync).
type accountDetailResponse struct {
	accountResponse
	Mailboxes []any `json:"mailboxes"`
}

func respondAccount(a *Account) accountResponse {
	return accountResponse{
		ID:           a.ID,
		Name:         a.Name,
		EmailAddress: a.Email,
		IMAPHost:     a.IMAPHost,
		IMAPPort:     a.IMAPPort,
		IMAPSSLMode:  a.IMAPSSLMode,
		IMAPUser:     a.IMAPUser,
		SMTPHost:     a.SMTPHost,
		SMTPPort:     a.SMTPPort,
		SMTPSSLMode:  a.SMTPSSLMode,
		SMTPUser:     a.SMTPUser,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok || p.UID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, AccountsPrefix)
	if tail == "" || tail == "/" {
		switch r.Method {
		case http.MethodGet:
			h.list(w, r, p.UID)
		case http.MethodPost:
			h.create(w, r, p.UID)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	id, ok := parseAccountTail(tail)
	if !ok {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.get(w, r, p.UID, id)
	case http.MethodPut:
		h.update(w, r, p.UID, id)
	case http.MethodDelete:
		h.delete(w, r, p.UID, id)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// parseAccountTail extracts the {id} from "/{id}"; anything else (extra
// segments, non-numeric, non-positive) is not an account path.
func parseAccountTail(tail string) (int64, bool) {
	idStr, ok := strings.CutPrefix(tail, "/")
	if !ok || idStr == "" || strings.Contains(idStr, "/") {
		return 0, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// createRequest is the POST body (official app field names). An empty
// smtpPassword means "same as imapPassword".
type createRequest struct {
	Name         string `json:"name"`
	EmailAddress string `json:"emailAddress"`
	IMAPHost     string `json:"imapHost"`
	IMAPPort     int    `json:"imapPort"`
	IMAPSSLMode  string `json:"imapSslMode"`
	IMAPUser     string `json:"imapUser"`
	IMAPPassword string `json:"imapPassword"`
	SMTPHost     string `json:"smtpHost"`
	SMTPPort     int    `json:"smtpPort"`
	SMTPSSLMode  string `json:"smtpSslMode"`
	SMTPUser     string `json:"smtpUser"`
	SMTPPassword string `json:"smtpPassword"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, uid string) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in, verr := validateCreate(&req)
	if verr != "" {
		writeError(w, http.StatusBadRequest, verr)
		return
	}
	a, err := h.Svc.Create(r.Context(), uid, in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, respondAccount(a))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, uid string) {
	accounts, err := h.Svc.List(r.Context(), uid)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]accountResponse, 0, len(accounts))
	for i := range accounts {
		out = append(out, respondAccount(&accounts[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	a, err := h.Svc.Get(r.Context(), uid, id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, accountDetailResponse{
		accountResponse: respondAccount(a),
		Mailboxes:       []any{},
	})
}

// updateRequest is the PUT body: pointer fields distinguish "absent" (keep)
// from "present" (set), so partial updates cannot zero values by omission.
type updateRequest struct {
	Name         *string `json:"name"`
	EmailAddress *string `json:"emailAddress"`
	IMAPHost     *string `json:"imapHost"`
	IMAPPort     *int    `json:"imapPort"`
	IMAPSSLMode  *string `json:"imapSslMode"`
	IMAPUser     *string `json:"imapUser"`
	IMAPPassword *string `json:"imapPassword"`
	SMTPHost     *string `json:"smtpHost"`
	SMTPPort     *int    `json:"smtpPort"`
	SMTPSSLMode  *string `json:"smtpSslMode"`
	SMTPUser     *string `json:"smtpUser"`
	SMTPPassword *string `json:"smtpPassword"`
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if verr := validateUpdate(&req); verr != "" {
		writeError(w, http.StatusBadRequest, verr)
		return
	}
	patch := AccountPatch{
		Name:         req.Name,
		Email:        req.EmailAddress,
		IMAPHost:     req.IMAPHost,
		IMAPPort:     req.IMAPPort,
		IMAPSSLMode:  req.IMAPSSLMode,
		IMAPUser:     req.IMAPUser,
		IMAPPassword: req.IMAPPassword,
		SMTPHost:     req.SMTPHost,
		SMTPPort:     req.SMTPPort,
		SMTPSSLMode:  req.SMTPSSLMode,
		SMTPUser:     req.SMTPUser,
		SMTPPassword: req.SMTPPassword,
	}
	a, err := h.Svc.Update(r.Context(), uid, id, patch)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, respondAccount(a))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if err := h.Svc.Delete(r.Context(), uid, id); err != nil {
		mapStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// validateCreate enforces the M1 contract: required fields, port range,
// ssl-mode enum, parseable email address. M1 deliberately does NOT dial
// IMAP to verify credentials (that lands in M2) — validation alone gates.
func validateCreate(req *createRequest) (AccountInput, string) {
	if req.EmailAddress == "" {
		return AccountInput{}, "emailAddress is required"
	}
	if _, err := mail.ParseAddress(req.EmailAddress); err != nil {
		return AccountInput{}, "emailAddress is invalid"
	}
	if req.IMAPHost == "" {
		return AccountInput{}, "imapHost is required"
	}
	if req.IMAPUser == "" {
		return AccountInput{}, "imapUser is required"
	}
	if req.IMAPPassword == "" {
		return AccountInput{}, "imapPassword is required"
	}
	if !validPort(req.IMAPPort) {
		return AccountInput{}, "imapPort must be between 1 and 65535"
	}
	imapMode, verr := sslModeOrDefault(req.IMAPSSLMode, "imapSslMode")
	if verr != "" {
		return AccountInput{}, verr
	}
	if req.SMTPHost == "" {
		return AccountInput{}, "smtpHost is required"
	}
	if req.SMTPUser == "" {
		return AccountInput{}, "smtpUser is required"
	}
	if !validPort(req.SMTPPort) {
		return AccountInput{}, "smtpPort must be between 1 and 65535"
	}
	smtpMode, verr := sslModeOrDefault(req.SMTPSSLMode, "smtpSslMode")
	if verr != "" {
		return AccountInput{}, verr
	}
	return AccountInput{
		Name:         req.Name,
		Email:        req.EmailAddress,
		IMAPHost:     req.IMAPHost,
		IMAPPort:     req.IMAPPort,
		IMAPSSLMode:  imapMode,
		IMAPUser:     req.IMAPUser,
		IMAPPassword: req.IMAPPassword,
		SMTPHost:     req.SMTPHost,
		SMTPPort:     req.SMTPPort,
		SMTPSSLMode:  smtpMode,
		SMTPUser:     req.SMTPUser,
		SMTPPassword: req.SMTPPassword,
	}, ""
}

// validateUpdate checks only the fields the patch carries.
func validateUpdate(req *updateRequest) string {
	if req.EmailAddress != nil {
		if *req.EmailAddress == "" {
			return "emailAddress is required"
		}
		if _, err := mail.ParseAddress(*req.EmailAddress); err != nil {
			return "emailAddress is invalid"
		}
	}
	if req.IMAPHost != nil && *req.IMAPHost == "" {
		return "imapHost is required"
	}
	if req.IMAPUser != nil && *req.IMAPUser == "" {
		return "imapUser is required"
	}
	if req.IMAPPassword != nil && *req.IMAPPassword == "" {
		return "imapPassword is required"
	}
	if req.IMAPPort != nil && !validPort(*req.IMAPPort) {
		return "imapPort must be between 1 and 65535"
	}
	if req.IMAPSSLMode != nil && !ValidSSLMode(*req.IMAPSSLMode) {
		return "imapSslMode must be one of ssl, starttls, none"
	}
	if req.SMTPHost != nil && *req.SMTPHost == "" {
		return "smtpHost is required"
	}
	if req.SMTPUser != nil && *req.SMTPUser == "" {
		return "smtpUser is required"
	}
	if req.SMTPPort != nil && !validPort(*req.SMTPPort) {
		return "smtpPort must be between 1 and 65535"
	}
	if req.SMTPSSLMode != nil && !ValidSSLMode(*req.SMTPSSLMode) {
		return "smtpSslMode must be one of ssl, starttls, none"
	}
	return ""
}

func validPort(p int) bool {
	return p >= 1 && p <= 65535
}

func sslModeOrDefault(mode, field string) (string, string) {
	if mode == "" {
		return SSLModeSSL, ""
	}
	if !ValidSSLMode(mode) {
		return "", field + " must be one of ssl, starttls, none"
	}
	return mode, ""
}

func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "account not found")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// writeError is the repo's JSON error convention (console's errorPayload).
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, struct {
		Error string `json:"error"`
	}{Error: msg})
}
