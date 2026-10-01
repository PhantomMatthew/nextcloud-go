package wopi

import (
	"bytes"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
)

//go:embed ui/viewer.html
var viewerHTML string

var viewerTemplate = template.Must(template.New("viewer").Parse(viewerHTML))

// ViewerHandler serves GET /index.php/apps/richdocuments/index?fileId=N —
// the upstream richdocuments viewer path, session-authed. It mints a WOPI
// token for the caller, resolves the Collabora editor URL from discovery,
// and renders the embedded shell: a form that POSTs the token into the
// editor iframe (the standard WOPI bootstrap), so the browser session with
// Collabora never sees a Nextcloud credential. The minted token itself is
// TTL-bound and file-scoped (ADR-0106).
type ViewerHandler struct {
	Svc  *Service
	Disc *Discovery
}

type viewerData struct {
	FileName string
	Action   string
	Token    string
	TokenTTL int64
}

func (h *ViewerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.UserFromContext(r.Context())
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw := r.URL.Query().Get("fileId")
	fileID, err := strconv.ParseInt(raw, 10, 64)
	if raw == "" || err != nil || fileID <= 0 {
		http.Error(w, "bad fileId", http.StatusBadRequest)
		return
	}
	_, fsPath, _, err := h.Svc.resolve(r.Context(), p.UID, fileID)
	if err != nil {
		if errors.Is(err, errIsDir) {
			http.Error(w, "file id is a directory", http.StatusBadRequest)
			return
		}
		mapError(w, err)
		return
	}
	tok, expiresAt, canWrite, err := h.Svc.Mint(r.Context(), p.UID, fileID)
	if err != nil {
		mapError(w, err)
		return
	}
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(fsPath)), ".")
	actionURL, err := h.Disc.ActionURL(r.Context(), ext, canWrite)
	if err != nil {
		if errors.Is(err, ErrNoDiscoveryAction) {
			http.Error(w, "file type not editable", http.StatusNotFound)
			return
		}
		// A discovery fetch/parse failure is upstream infrastructure, not a
		// file condition.
		http.Error(w, "office discovery unavailable", http.StatusBadGateway)
		return
	}
	wopiSrc := absoluteBase(r) + WopiFilesPrefix + strconv.FormatInt(fileID, 10)
	data := viewerData{
		FileName: path.Base(fsPath),
		Action:   appendWopiSrc(actionURL, wopiSrc),
		Token:    tok,
		TokenTTL: expiresAt.UnixMilli(),
	}
	var buf bytes.Buffer
	if err := viewerTemplate.Execute(&buf, data); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// appendWopiSrc adds the WOPISrc query parameter to a discovery urlsrc,
// respecting whatever query string the urlsrc already carries (Collabora's
// urlsrc conventionally ends in '?').
func appendWopiSrc(actionURL, wopiSrc string) string {
	sep := "?"
	if strings.Contains(actionURL, "?") {
		sep = "&"
		if strings.HasSuffix(actionURL, "?") || strings.HasSuffix(actionURL, "&") {
			sep = ""
		}
	}
	return actionURL + sep + "WOPISrc=" + url.QueryEscape(wopiSrc)
}
