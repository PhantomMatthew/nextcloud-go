package mail

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"path"
	"strconv"
)

// handler_send.go is the M5 send endpoint (ADR-0108 §1):
// POST /apps/mail/api/accounts/{id}/send. The account lookup enforces
// ownership first (cross-user is the same 404 as a missing account); the
// body is the official-style JSON {to[], cc[], bcc[], subject, bodyPlain,
// bodyHtml, inReplyTo, references, attachments[]} where every address
// element parses via net/mail.ParseAddressList ("Name <a@b>" forms
// allowed) and attachments carry inline base64 content XOR a files-app
// path read through the share/ownership-aware file seam.

// sendRequest is the POST .../send JSON body.
type sendRequest struct {
	To          []string         `json:"to"`
	Cc          []string         `json:"cc"`
	Bcc         []string         `json:"bcc"`
	Subject     string           `json:"subject"`
	BodyPlain   string           `json:"bodyPlain"`
	BodyHTML    string           `json:"bodyHtml"`
	InReplyTo   string           `json:"inReplyTo"`
	References  string           `json:"references"`
	Attachments []sendAttachment `json:"attachments"`
}

// sendAttachment is one attachment: inline content (contentBase64) XOR a
// files-app path (path) — exactly one is required. path resolves against
// the SENDER's files through the Handler.ReadFile seam (the same
// share/ownership-aware read WOPI's GetFile uses).
type sendAttachment struct {
	Filename      string `json:"filename"`
	ContentType   string `json:"contentType"`
	ContentBase64 string `json:"contentBase64"`
	Path          string `json:"path"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request, uid string, id int64) {
	a, err := h.Svc.Get(r.Context(), uid, id)
	if err != nil {
		mapServiceError(w, err)
		return
	}
	if h.Sender == nil {
		writeError(w, http.StatusInternalServerError, "send is not configured")
		return
	}
	var req sendRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	in, verr := h.buildSendInput(r, uid, &req)
	if verr != "" {
		writeError(w, http.StatusBadRequest, verr)
		return
	}
	messageID, err := h.Sender.Send(r.Context(), a, in)
	if err != nil {
		mapSendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		MessageID string `json:"messageId"`
	}{MessageID: messageID})
}

// buildSendInput validates the request into a SendInput: every address
// element parses (a parse failure names its field), inline attachments
// base64-decode, and path attachments read through the files seam.
func (h *Handler) buildSendInput(r *http.Request, uid string, req *sendRequest) (SendInput, string) {
	in := SendInput{
		Subject:    req.Subject,
		BodyPlain:  req.BodyPlain,
		BodyHTML:   req.BodyHTML,
		InReplyTo:  req.InReplyTo,
		References: req.References,
	}
	parse := func(field string, raw []string) ([]mail.Address, string) {
		var out []mail.Address
		for _, elem := range raw {
			addrs, err := mail.ParseAddressList(elem)
			if err != nil || len(addrs) == 0 {
				return nil, field + ": invalid address"
			}
			for _, a := range addrs {
				out = append(out, *a)
			}
		}
		return out, ""
	}
	var verr string
	if in.To, verr = parse("to", req.To); verr != "" {
		return SendInput{}, verr
	}
	if in.Cc, verr = parse("cc", req.Cc); verr != "" {
		return SendInput{}, verr
	}
	if in.Bcc, verr = parse("bcc", req.Bcc); verr != "" {
		return SendInput{}, verr
	}
	for i, at := range req.Attachments {
		field := "attachments[" + strconv.Itoa(i) + "]: "
		if (at.ContentBase64 == "") == (at.Path == "") {
			return SendInput{}, field + "exactly one of contentBase64 or path is required"
		}
		out := OutgoingAttachment{Filename: at.Filename, ContentType: at.ContentType}
		if at.ContentBase64 != "" {
			data, err := base64.StdEncoding.DecodeString(at.ContentBase64)
			if err != nil {
				return SendInput{}, field + "invalid base64 content"
			}
			out.Data = data
		} else {
			data, name, rerr := h.readPathAttachment(r, uid, at.Path)
			if rerr != "" {
				return SendInput{}, field + rerr
			}
			out.Data = data
			if out.Filename == "" {
				out.Filename = name
			}
		}
		in.Attachments = append(in.Attachments, out)
	}
	return in, ""
}

// readPathAttachment resolves a files-app path against the SENDER's files
// through the ReadFile seam (share/ownership-aware: an incoming share the
// sender can read works, anything else does not) and returns the content
// capped at the per-attachment limit. Every failure is a 400 — the path was
// part of the request.
func (h *Handler) readPathAttachment(r *http.Request, uid, p string) (data []byte, name, verr string) {
	if h.ReadFile == nil {
		return nil, "", "files attachments are not configured"
	}
	rc, err := h.ReadFile(r.Context(), uid, p)
	if err != nil {
		return nil, "", "cannot read the file"
	}
	defer func() { _ = rc.Close() }()
	data, err = io.ReadAll(io.LimitReader(rc, maxAttachmentSize+1))
	if err != nil {
		return nil, "", "cannot read the file"
	}
	if len(data) > maxAttachmentSize {
		return nil, "", "the file exceeds the 10 MiB attachment limit"
	}
	return data, path.Base(p), ""
}

// mapSendError renders send failures: a refused recipient is 400 naming the
// address, a validation/compose failure 400, an over-size composed message
// 413, an SMTP dial/auth/transport failure 502, and anything else 500.
func mapSendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRecipientRefused):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrSendValidation), errors.Is(err, ErrCompose):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrMessageTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "message exceeds the 25 MiB limit")
	case errors.Is(err, ErrUpstream):
		writeError(w, http.StatusBadGateway, "smtp send failed")
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
