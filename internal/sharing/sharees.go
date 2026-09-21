package sharing

import (
	"net/http"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

const (
	ocsShareesPrefixV1 = "/ocs/v1.php/apps/files_sharing/api/v1/sharees"
	ocsShareesPrefixV2 = "/ocs/v2.php/apps/files_sharing/api/v1/sharees"
)

// ShareesHandler serves GET files_sharing sharees (federated cloud ID plus
// optional lookup server search).
type ShareesHandler struct {
	Version ocs.Version
	Lookup  *LookupClient
}

func (h ShareesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFromContext(r.Context()); !ok {
		writeOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	rest, ok := shareesPathRemainder(r.URL.Path, h.Version)
	if !ok || (rest != "" && rest != "/") {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	q := r.URL.Query()
	search := q.Get("search")
	var hits []LookupResult
	if lookupEnabled(q.Get("lookup")) && strings.TrimSpace(search) != "" {
		if res, err := h.Lookup.Search(r.Context(), search); err == nil {
			hits = res
		}
	}
	writeOCS(w, r, h.Version, 0, "", shareesPayload(search, ocm.RequestBaseURL(r), hits))
}

func lookupEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1":
		return true
	}
	return false
}

func shareesPathRemainder(path string, version ocs.Version) (string, bool) {
	prefix := ocsShareesPrefixV2
	if version == ocs.V1 {
		prefix = ocsShareesPrefixV1
	}
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return strings.TrimPrefix(path, prefix), true
}

func shareesPayload(search, origin string, hits []LookupResult) ocs.OrderedMap {
	search = strings.TrimSpace(search)
	remotes := make([]any, 0)
	uid, remote := ocm.SplitCloudID(search)
	if uid != "" && remote != "" {
		server := ocm.NormalizeOrigin(remote)
		if origin == "" || !sameHTTPHost(origin, server) {
			remotes = append(remotes, ocs.Obj(
				ocs.K("label", search),
				ocs.K("value", ocs.Obj(
					ocs.K("shareType", files.ShareTypeRemote),
					ocs.K("shareWith", search),
					ocs.K("server", server),
				)),
			))
		}
	}
	lookup := make([]any, 0, len(hits))
	for _, hit := range hits {
		uid, remote := ocm.SplitCloudID(hit.FederationID)
		if uid == "" || remote == "" {
			continue
		}
		server := ocm.NormalizeOrigin(remote)
		if origin != "" && sameHTTPHost(origin, server) {
			continue
		}
		label := hit.Name
		if label == "" {
			label = hit.FederationID
		}
		lookup = append(lookup, ocs.Obj(
			ocs.K("label", label),
			ocs.K("value", ocs.Obj(
				ocs.K("shareType", files.ShareTypeRemote),
				ocs.K("shareWith", hit.FederationID),
				ocs.K("server", server),
			)),
		))
	}
	empty := []any{}
	return ocs.Obj(
		ocs.K("exact", ocs.Obj(
			ocs.K("users", empty),
			ocs.K("groups", empty),
			ocs.K("remotes", remotes),
			ocs.K("remote_groups", empty),
			ocs.K("emails", empty),
			ocs.K("circles", empty),
			ocs.K("rooms", empty),
		)),
		ocs.K("users", empty),
		ocs.K("groups", empty),
		ocs.K("emails", empty),
		ocs.K("circles", empty),
		ocs.K("rooms", empty),
		ocs.K("remotes", empty),
		ocs.K("remote_groups", empty),
		ocs.K("lookup", lookup),
	)
}
