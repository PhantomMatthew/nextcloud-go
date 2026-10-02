package mail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// AccountsPrefix is the JSON REST mount path mirroring the official
// Nextcloud Mail app (ADR-0108 §2). The router has no path parameters, so
// the handler is registered as a prefix route and parses the tail itself
// (the WOPI FilesHandler pattern).
const AccountsPrefix = "/apps/mail/api/accounts"

// Handler serves the session-authed accounts API:
//
//	POST   /apps/mail/api/accounts                  create (201)
//	GET    /apps/mail/api/accounts                  list the caller's accounts
//	GET    /apps/mail/api/accounts/{id}             one account (+ synced mailboxes)
//	PUT    /apps/mail/api/accounts/{id}             partial update
//	DELETE /apps/mail/api/accounts/{id}             delete (200 {})
//	GET    /apps/mail/api/accounts/{id}/mailboxes   synced mailboxes with counts
//	POST   /apps/mail/api/accounts/{id}/sync        run one sync pass (200 {newMessages})
//
// M4 (ADR-0108 §6) message APIs under one synced mailbox:
//
//	GET    .../mailboxes/{mbid}/messages                     list view, cursor pages
//	GET    .../mailboxes/{mbid}/messages/{mid}               live body fetch (+ \Seen)
//	PUT    .../mailboxes/{mbid}/messages/{mid}/flags         live UID STORE + local row
//	DELETE .../mailboxes/{mbid}/messages/{mid}               trash-COPY or expunge
//	PUT    .../mailboxes/{mbid}/messages/{mid}/move          UID COPY + expunge
//	GET    .../mailboxes/{mbid}/messages/{mid}/attachments/{index}  live download
//
// M5 (ADR-0108 §1):
//
//	POST   /apps/mail/api/accounts/{id}/send        compose + SMTP send + save to Sent
//
// Passwords never appear in any response; cross-user rows are 404, not 403.
type Handler struct {
	Svc *Service
	// Syncer runs the M3 mailbox sync for POST .../sync; nil leaves that
	// endpoint a 500 (a miswiring, not a client error).
	Syncer *Syncer
	// Ops runs the M4 live IMAP operations; nil leaves the live endpoints a
	// 500 (a miswiring, not a client error). The list endpoint is
	// store-only and works without it.
	Ops *MessageOps
	// Sender runs the M5 send flow for POST .../send; nil leaves that
	// endpoint a 500 (a miswiring, not a client error).
	Sender *Sender
	// ReadFile reads one file's content from a user's files with the
	// share/ownership checks of the WOPI GetFile seam (production:
	// webdav.FS.Read). It backs the {path} attachment form; nil leaves
	// files-path attachments a 400.
	ReadFile func(ctx context.Context, uid, path string) (io.ReadCloser, error)
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

// accountDetailResponse adds the synced mailbox list (M3) the official
// app's single-account response carries.
type accountDetailResponse struct {
	accountResponse
	Mailboxes []mailboxResponse `json:"mailboxes"`
}

// mailboxResponse is one synced mailbox (M3): name is the DISPLAY name
// (the wire name mUTF7-decoded), counts come from the summary rows.
type mailboxResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	SpecialUse string `json:"specialUse"`
	Selectable bool   `json:"selectable"`
	Unread     int64  `json:"unread"`
	Total      int64  `json:"total"`
}

func respondMailboxes(counts []MailboxCounts) []mailboxResponse {
	out := make([]mailboxResponse, 0, len(counts))
	for i := range counts {
		out = append(out, mailboxResponse{
			ID:         counts[i].ID,
			Name:       imap.DecodeMailboxName(counts[i].Name),
			SpecialUse: counts[i].SpecialUse,
			Selectable: counts[i].Selectable,
			Unread:     counts[i].Unread,
			Total:      counts[i].Total,
		})
	}
	return out
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
	id, rest, ok := parseAccountTail(tail)
	if !ok {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	switch {
	case rest == "":
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
	case rest == "mailboxes" || rest == "sync" || rest == "send":
		// Sub-resources: GET mailboxes, POST sync, POST send. Everything
		// else is 405.
		switch {
		case rest == "mailboxes" && r.Method == http.MethodGet:
			h.mailboxes(w, r, p.UID, id)
		case rest == "sync" && r.Method == http.MethodPost:
			h.sync(w, r, p.UID, id)
		case rest == "send" && r.Method == http.MethodPost:
			h.send(w, r, p.UID, id)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case strings.HasPrefix(rest, "mailboxes/"):
		// The M4 message APIs live one level deeper.
		h.mailboxSub(w, r, p.UID, id, strings.TrimPrefix(rest, "mailboxes/"))
	default:
		writeError(w, http.StatusNotFound, "account not found")
	}
}

// parseAccountTail extracts the {id} and the remaining path from
// "/{id}[/<rest>]"; a non-numeric or non-positive id is not an account
// path. rest is everything after the id segment with no leading slash
// ("mailboxes", "sync", "mailboxes/3/messages/1/flags", …).
func parseAccountTail(tail string) (id int64, rest string, ok bool) {
	seg, ok := strings.CutPrefix(tail, "/")
	if !ok || seg == "" {
		return 0, "", false
	}
	idStr, rest, _ := strings.Cut(seg, "/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	return id, rest, true
}

// mailboxSub dispatches the M4 message APIs under
// /accounts/{id}/mailboxes/{mbid}/messages[...]: sub is everything after
// "mailboxes/". A malformed {mbid} or an unknown path is 404; a wrong verb
// is 405.
func (h *Handler) mailboxSub(w http.ResponseWriter, r *http.Request, uid string, accountID int64, sub string) {
	mbidStr, rest, _ := strings.Cut(sub, "/")
	mbid, err := strconv.ParseInt(mbidStr, 10, 64)
	if err != nil || mbid <= 0 {
		writeError(w, http.StatusNotFound, "mailbox not found")
		return
	}
	switch {
	case rest == "messages" || rest == "messages/":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.messagesList(w, r, uid, accountID, mbid)
	case strings.HasPrefix(rest, "messages/"):
		h.messageSub(w, r, uid, accountID, mbid, strings.TrimPrefix(rest, "messages/"))
	default:
		writeError(w, http.StatusNotFound, "mailbox not found")
	}
}

// messageSub dispatches under .../messages/{mid}: sub is everything after
// "messages/" — "{mid}", "{mid}/flags", "{mid}/move", or
// "{mid}/attachments/{index}".
func (h *Handler) messageSub(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid int64, sub string) {
	midStr, op, _ := strings.Cut(sub, "/")
	mid, err := strconv.ParseInt(midStr, 10, 64)
	if err != nil || mid <= 0 {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	switch op {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.messageDetail(w, r, uid, accountID, mbid, mid)
		case http.MethodDelete:
			h.messageDelete(w, r, uid, accountID, mbid, mid)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "flags", "move":
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", "PUT")
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if op == "flags" {
			h.messageFlags(w, r, uid, accountID, mbid, mid)
		} else {
			h.messageMove(w, r, uid, accountID, mbid, mid)
		}
	default:
		if idxStr, ok := strings.CutPrefix(op, "attachments/"); ok && !strings.Contains(idxStr, "/") {
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idx < 0 {
				writeError(w, http.StatusNotFound, "attachment not found")
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			h.messageAttachment(w, r, uid, accountID, mbid, mid, idx)
			return
		}
		writeError(w, http.StatusNotFound, "message not found")
	}
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
		mapServiceError(w, err)
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
		mapServiceError(w, err)
		return
	}
	counts, err := h.Svc.Store.ListMailboxCounts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, accountDetailResponse{
		accountResponse: respondAccount(a),
		Mailboxes:       respondMailboxes(counts),
	})
}

// mailboxes lists the account's synced mailboxes with unread/total counts
// (M3). The account lookup enforces ownership: cross-user is the same 404
// as a missing account.
func (h *Handler) mailboxes(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if _, err := h.Svc.Get(r.Context(), uid, id); err != nil {
		mapServiceError(w, err)
		return
	}
	counts, err := h.Svc.Store.ListMailboxCounts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, respondMailboxes(counts))
}

// sync runs one synchronous sync pass over the account (M3's primary test
// seam). A sync failure is 502 — the account exists and the request was
// valid (unlike 404/400), but the upstream IMAP side failed.
func (h *Handler) sync(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	a, err := h.Svc.Get(r.Context(), uid, id)
	if err != nil {
		mapServiceError(w, err)
		return
	}
	if h.Syncer == nil {
		writeError(w, http.StatusInternalServerError, "sync is not configured")
		return
	}
	n, err := h.Syncer.SyncAccount(r.Context(), a)
	if err != nil {
		writeError(w, http.StatusBadGateway, "sync failed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		NewMessages int `json:"newMessages"`
	}{NewMessages: n})
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
		mapServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, respondAccount(a))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	if err := h.Svc.Delete(r.Context(), uid, id); err != nil {
		mapServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// validateCreate enforces the request-shape contract: required fields, port
// range, ssl-mode enum, parseable email address. Shape errors are 400s;
// beyond shape, Service.Create verifies the IMAP LOGIN before persisting
// (M2, ADR-0108 §1) and its failures map to their own 400s.
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

// mapServiceError renders store and verification failures. Verify failures
// are 400s — the account fields, not the server, are at fault — with
// distinct messages for bad credentials vs. an unreachable server; a
// missing (or cross-user) row is 404; anything else is 500.
func mapServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "account not found")
	case errors.Is(err, ErrVerifyAuth):
		writeError(w, http.StatusBadRequest, "IMAP authentication failed")
	case errors.Is(err, ErrVerifyConnect):
		writeError(w, http.StatusBadRequest, "cannot connect to IMAP server")
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
