package capabilities

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

type Handler struct {
	Manager *Manager
}

func (h Handler) buildPayload() ocs.OrderedMap {
	v := version.Version
	return ocs.Obj(
		ocs.K("version", ocs.Obj(
			ocs.K("major", v[0]),
			ocs.K("minor", v[1]),
			ocs.K("micro", v[2]),
			ocs.K("string", version.VersionString),
			ocs.K("edition", version.Edition),
			ocs.K("extendedSupport", false),
		)),
		ocs.K("capabilities", h.Manager.Collect()),
	)
}

func (h Handler) ServeOCS(ver ocs.Version) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := h.buildPayload()
		etag, err := computeETag(payload)
		if err != nil {
			http.Error(w, "etag", http.StatusInternalServerError)
			return
		}
		format := ocs.NegotiateFormat(r.URL.Query().Get("format"), r.Header.Get("Accept"))
		body, contentType, err := ocs.Render(ver, format, ocs.Meta{}, payload)
		if err != nil {
			http.Error(w, "render", http.StatusInternalServerError)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Type", contentType)
		hdr.Set("ETag", `"`+etag+`"`)
		okCode := ocs.StatusOKv1
		if ver == ocs.V2 {
			okCode = ocs.StatusOKv2
		}
		w.WriteHeader(ocs.Map(ver, okCode))
		_, _ = w.Write(body)
	})
}

func computeETag(payload ocs.OrderedMap) (string, error) {
	body, err := payload.MarshalJSON()
	if err != nil {
		return "", err
	}
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:]), nil
}
