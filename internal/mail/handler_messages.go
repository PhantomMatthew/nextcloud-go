package mail

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/mail/imap"
)

// handler_messages.go is the M4 message API half of Handler (ADR-0108 §6):
// the list view reads the synced summaries, and the detail/flags/delete/
// move/attachment endpoints run live IMAP operations through MessageOps
// (one per-request connection). Every route resolves its
// {account, mailbox, message} scope through the owning user first, so a
// cross-user (or cross-mailbox) row is the same 404 as a missing one.

// messageSummary is the summary JSON the list, flags, and detail responses
// share. Flags carry the IMAP backslash again ("\\Seen") — the storage form
// drops it.
type messageSummary struct {
	ID      int64    `json:"id"`
	UID     int64    `json:"uid"`
	Subject string   `json:"subject"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Date    int64    `json:"date"`
	Flags   []string `json:"flags"`
	Size    int64    `json:"size"`
}

// messageResponse is the list-view summary JSON: the shared summary plus
// the M6 sync-time extras — the plain-body snippet and the paperclip flag.
type messageResponse struct {
	messageSummary
	Preview        string `json:"preview"`
	HasAttachments bool   `json:"hasAttachments"`
}

// messageListResponse is one cursor page; nextCursor is null at the end.
type messageListResponse struct {
	Messages   []messageResponse `json:"messages"`
	NextCursor *string           `json:"nextCursor"`
}

// attachmentResponse is one attachment's metadata (the index keys the
// download route — see mime.go for the stability contract).
type attachmentResponse struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

// messageDetailResponse adds the live-fetched bodies. BodyHTML is
// UNSANITIZED — htmlSanitized stays false so clients know they must
// sanitize before rendering (ADR-0108 §6: first-party HTML rendering is
// blocked until a sanitizer lands). The detail shape is M4's — it does NOT
// inherit the M6 list preview fields.
type messageDetailResponse struct {
	messageSummary
	BodyPlain     string               `json:"bodyPlain"`
	BodyHTML      string               `json:"bodyHtml"`
	Attachments   []attachmentResponse `json:"attachments"`
	HTMLSanitized bool                 `json:"htmlSanitized"`
}

const (
	// messagesDefaultLimit is the list page size when ?limit is absent;
	// messagesMaxLimit clamps it.
	messagesDefaultLimit = 50
	messagesMaxLimit     = 200
)

// wireFlags renders storage tokens as IMAP flags: system flags get their
// backslash back ("\Seen"); keywords pass through unchanged.
func wireFlags(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if systemFlagName(tok) {
			out = append(out, `\`+tok)
		} else {
			out = append(out, tok)
		}
	}
	return out
}

// systemFlagName reports whether tok is one of the six RFC 3501 system flag
// names (case-insensitively — the server sent the canonical casing, but a
// lowercase "\seen" must round-trip too).
func systemFlagName(tok string) bool {
	for _, name := range []string{"Seen", "Answered", "Flagged", "Deleted", "Draft", "Recent"} {
		if strings.EqualFold(tok, name) {
			return true
		}
	}
	return false
}

// respondSummary renders the shared summary of one row; flagTokens are the
// bare storage tokens (from the row, or from the op that just changed them).
func respondSummary(m *Message, flagTokens []string) messageSummary {
	return messageSummary{
		ID:      m.ID,
		UID:     m.UID,
		Subject: m.Subject,
		From:    m.FromAddr,
		To:      m.ToAddrs,
		Date:    m.DateUnix,
		Flags:   wireFlags(flagTokens),
		Size:    m.Size,
	}
}

// respondMessage renders one list-view row: the shared summary plus the
// synced preview extras.
func respondMessage(m *Message, flagTokens []string) messageResponse {
	return messageResponse{
		messageSummary: respondSummary(m, flagTokens),
		Preview:        m.Preview,
		HasAttachments: m.HasAttachments,
	}
}

// mailScope is the resolved ownership chain of one M4 request.
type mailScope struct {
	account *Account
	mailbox *Mailbox
	message *Message // nil on the collection route
}

// resolveScope walks {account → mailbox → message} with ownership checks at
// every level; mid <= 0 stops at the mailbox (the collection route). A
// failure writes the response (404/500) and reports ok=false.
func (h *Handler) resolveScope(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64) (mailScope, bool) {
	a, err := h.Svc.Get(r.Context(), uid, accountID)
	if err != nil {
		mapServiceError(w, err)
		return mailScope{}, false
	}
	mb, err := h.Svc.Store.GetMailbox(r.Context(), accountID, mbid)
	if err != nil {
		mapScopeError(w, err, "mailbox not found")
		return mailScope{}, false
	}
	scope := mailScope{account: a, mailbox: mb}
	if mid > 0 {
		msg, err := h.Svc.Store.GetMessage(r.Context(), mbid, mid)
		if err != nil {
			mapScopeError(w, err, "message not found")
			return mailScope{}, false
		}
		scope.message = msg
	}
	return scope, true
}

// ops returns the live-op engine or writes the miswiring 500.
func (h *Handler) ops(w http.ResponseWriter) *MessageOps {
	if h.Ops == nil {
		writeError(w, http.StatusInternalServerError, "message operations are not configured")
		return nil
	}
	return h.Ops
}

// messagesList serves one cursor page of the list view:
// GET .../messages?cursor=<dateUnix>_<id>&limit=N (default 50, max 200).
// The cursor is the keyset of the last row of the previous page; the store
// is asked for one row extra to decide whether a next page exists.
func (h *Handler) messagesList(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid int64) {
	if _, ok := h.resolveScope(w, r, uid, accountID, mbid, 0); !ok {
		return
	}
	limit := messagesDefaultLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, messagesMaxLimit)
	}
	var beforeDate, beforeID int64
	if c := r.URL.Query().Get("cursor"); c != "" {
		d, i, found := strings.Cut(c, "_")
		bd, err1 := strconv.ParseInt(d, 10, 64)
		bi, err2 := strconv.ParseInt(i, 10, 64)
		if !found || err1 != nil || err2 != nil {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		beforeDate, beforeID = bd, bi
	}
	msgs, err := h.Svc.Store.ListMessages(r.Context(), mbid, beforeDate, beforeID, limit+1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var next *string
	if len(msgs) > limit {
		msgs = msgs[:limit]
		last := msgs[len(msgs)-1]
		c := strconv.FormatInt(last.DateUnix, 10) + "_" + strconv.FormatInt(last.ID, 10)
		next = &c
	}
	out := make([]messageResponse, 0, len(msgs))
	for i := range msgs {
		out = append(out, respondMessage(&msgs[i], strings.Fields(msgs[i].Flags)))
	}
	writeJSON(w, http.StatusOK, messageListResponse{Messages: out, NextCursor: next})
}

// messageDetail live-fetches the whole message (BODY.PEEK[]) and renders
// it. Reading marks \Seen by default — server side and in the local row —
// so the returned summary already carries it; ?markSeen=false skips both.
func (h *Handler) messageDetail(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64) {
	scope, ok := h.resolveScope(w, r, uid, accountID, mbid, mid)
	if !ok {
		return
	}
	ops := h.ops(w)
	if ops == nil {
		return
	}
	markSeen := r.URL.Query().Get("markSeen") != "false"
	raw, flags, err := ops.Detail(r.Context(), scope.account, scope.mailbox, scope.message, markSeen)
	if err != nil {
		mapMessageOpsError(w, err)
		return
	}
	parsed, err := ParseMessage(raw)
	if err != nil {
		mapMessageOpsError(w, err) // the size guard → 413
		return
	}
	atts := make([]attachmentResponse, 0, len(parsed.Attachments))
	for _, a := range parsed.Attachments {
		atts = append(atts, attachmentResponse(a))
	}
	writeJSON(w, http.StatusOK, messageDetailResponse{
		messageSummary: respondSummary(scope.message, flags),
		BodyPlain:      parsed.TextPlain,
		BodyHTML:       parsed.TextHTML,
		Attachments:    atts,
		HTMLSanitized:  false,
	})
}

// flagsRequest is the PUT flags body: any subset of the five system flags
// as *bool (present-and-true adds, present-and-false removes, absent keeps).
type flagsRequest struct {
	Seen     *bool `json:"seen"`
	Answered *bool `json:"answered"`
	Flagged  *bool `json:"flagged"`
	Deleted  *bool `json:"deleted"`
	Draft    *bool `json:"draft"`
}

// messageFlags applies the flag delta live (UID STORE) and to the local
// row, returning the updated summary. Unknown JSON keys are 400 — the fixed
// field set is the whole contract, and a typo'd flag name must not silently
// no-op.
func (h *Handler) messageFlags(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64) {
	scope, ok := h.resolveScope(w, r, uid, accountID, mbid, mid)
	if !ok {
		return
	}
	ops := h.ops(w)
	if ops == nil {
		return
	}
	var req flagsRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var add, remove []string
	for _, f := range []struct {
		val  *bool
		wire string
	}{
		{req.Seen, `\Seen`},
		{req.Answered, `\Answered`},
		{req.Flagged, `\Flagged`},
		{req.Deleted, `\Deleted`},
		{req.Draft, `\Draft`},
	} {
		if f.val == nil {
			continue
		}
		if *f.val {
			add = append(add, f.wire)
		} else {
			remove = append(remove, f.wire)
		}
	}
	if len(add) == 0 && len(remove) == 0 {
		writeError(w, http.StatusBadRequest, "at least one flag is required")
		return
	}
	tokens, err := ops.StoreFlags(r.Context(), scope.account, scope.mailbox, scope.message, add, remove)
	if err != nil {
		mapMessageOpsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, respondMessage(scope.message, tokens))
}

// messageDelete runs the trash flow: when the account has a synced trash
// mailbox (special_use='trash') other than the message's own, the message
// is UID COPYied there first; either way it is then \Deleted + EXPUNGEd.
// The local row is deleted; 200 {}.
func (h *Handler) messageDelete(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64) {
	scope, ok := h.resolveScope(w, r, uid, accountID, mbid, mid)
	if !ok {
		return
	}
	ops := h.ops(w)
	if ops == nil {
		return
	}
	var dest *Mailbox
	boxes, err := h.Svc.Store.ListMailboxes(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for i := range boxes {
		if boxes[i].SpecialUse == "trash" && boxes[i].ID != mbid {
			dest = &boxes[i]
			break
		}
	}
	if err := ops.RemoveMessage(r.Context(), scope.account, scope.mailbox, dest, scope.message); err != nil {
		mapMessageOpsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// moveRequest is the PUT move body; the destination must be a synced
// mailbox of the SAME account.
type moveRequest struct {
	DestMailboxID int64 `json:"destMailboxId"`
}

// messageMove copies the message to the destination mailbox, then expunges
// it here. The local row is DELETED (200 {}): the copy gets a new uid
// server-side, so the next sync rediscovers it in the destination rather
// than the row being re-pointed.
func (h *Handler) messageMove(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64) {
	scope, ok := h.resolveScope(w, r, uid, accountID, mbid, mid)
	if !ok {
		return
	}
	ops := h.ops(w)
	if ops == nil {
		return
	}
	var req moveRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.DestMailboxID <= 0 {
		writeError(w, http.StatusBadRequest, "destMailboxId is required")
		return
	}
	if req.DestMailboxID == mbid {
		writeError(w, http.StatusBadRequest, "cannot move a message to its own mailbox")
		return
	}
	dest, err := h.Svc.Store.GetMailbox(r.Context(), accountID, req.DestMailboxID)
	if err != nil {
		// A destination outside the account is the same 404 as a missing one.
		mapScopeError(w, err, "destination mailbox not found")
		return
	}
	if err := ops.RemoveMessage(r.Context(), scope.account, scope.mailbox, dest, scope.message); err != nil {
		mapMessageOpsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// messageAttachment live-fetches the message and streams one attachment's
// decoded bytes with its declared Content-Type, an attachment disposition
// (RFC 5987/2231 encoding for non-ASCII filenames via mime.FormatMediaType),
// and Content-Length.
func (h *Handler) messageAttachment(w http.ResponseWriter, r *http.Request, uid string, accountID, mbid, mid int64, index int) {
	scope, ok := h.resolveScope(w, r, uid, accountID, mbid, mid)
	if !ok {
		return
	}
	ops := h.ops(w)
	if ops == nil {
		return
	}
	raw, err := ops.FetchRaw(r.Context(), scope.account, scope.mailbox, scope.message)
	if err != nil {
		mapMessageOpsError(w, err)
		return
	}
	att, data, err := ExtractAttachment(raw, index)
	if err != nil {
		if errors.Is(err, ErrNoAttachment) {
			writeError(w, http.StatusNotFound, "attachment not found")
			return
		}
		mapMessageOpsError(w, err) // the size guard → 413
		return
	}
	disposition := "attachment"
	if att.Filename != "" {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": att.Filename})
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// mapScopeError renders store scope failures: a missing (or not-owned,
// cross-account, cross-mailbox) row is 404; anything else is 500.
func mapScopeError(w http.ResponseWriter, err error, notFoundMsg string) {
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, notFoundMsg)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal error")
}

// mapMessageOpsError renders live-op failures: the message gone is 404, an
// over-size body 413, an upstream dial/login/transport failure 502, and an
// allowlist rejection 400 (unreachable through this handler's fixed flag
// set — the mapping exists so the layer boundary stays total).
func mapMessageOpsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMessageGone):
		writeError(w, http.StatusNotFound, "message not found on the server")
	case errors.Is(err, ErrMessageTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "message exceeds the 32 MiB limit")
	case errors.Is(err, imap.ErrInvalidFlag), errors.Is(err, imap.ErrInjection):
		writeError(w, http.StatusBadRequest, "invalid flag")
	case errors.Is(err, ErrUpstream):
		writeError(w, http.StatusBadGateway, "imap operation failed")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
