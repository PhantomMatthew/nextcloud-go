package sharing

import (
	"net/http"
	"sort"
	"strings"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocm"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
)

const (
	ocsShareesPrefixV1 = "/ocs/v1.php/apps/files_sharing/api/v1/sharees"
	ocsShareesPrefixV2 = "/ocs/v2.php/apps/files_sharing/api/v1/sharees"
)

// ShareesHandler serves GET files_sharing sharees: exact federated cloud
// ID, local user/group typeahead, optional lookup server search, and
// /recommended recipients derived from the user's current shares.
type ShareesHandler struct {
	Version ocs.Version
	Lookup  *LookupClient
	Users   users.Store      // nil disables local user/group typeahead
	Shares  files.ShareStore // nil disables /recommended
}

func (h ShareesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFromContext(r.Context()); !ok {
		writeOCS(w, r, h.Version, ocs.RespondUnauthorised, "Current user is not logged in", nil)
		return
	}
	rest, ok := shareesPathRemainder(r.URL.Path, h.Version)
	if !ok {
		writeOCS(w, r, h.Version, ocs.RespondNotFound, "Not found", nil)
		return
	}
	if rest == "/recommended" {
		writeOCS(w, r, h.Version, 0, "", h.recommendedPayload(r))
		return
	}
	if rest != "" && rest != "/" {
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
	writeOCS(w, r, h.Version, 0, "", h.shareesPayload(r, search, hits))
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

// shareesPayload builds the sharees data map. Local user/group matches
// come from the Users store: an exact uid/gid match lands in exact.*,
// other matches in the typeahead collections.
func (h ShareesHandler) shareesPayload(r *http.Request, search string, hits []LookupResult) ocs.OrderedMap {
	origin := ocm.RequestBaseURL(r)
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
	exactUsers := make([]any, 0)
	exactGroups := make([]any, 0)
	localUsers := make([]any, 0)
	localGroups := make([]any, 0)
	if h.Users != nil && search != "" {
		if found, err := h.Users.Search(r.Context(), search, 20); err == nil {
			for i := range found {
				entry := ocs.Obj(
					ocs.K("label", shareeLabel(found[i].DisplayName, found[i].UID)),
					ocs.K("value", ocs.Obj(
						ocs.K("shareType", files.ShareTypeUser),
						ocs.K("shareWith", found[i].UID),
					)),
				)
				if found[i].UID == search {
					exactUsers = append(exactUsers, entry)
				} else {
					localUsers = append(localUsers, entry)
				}
			}
		}
		if found, err := h.Users.SearchGroups(r.Context(), search, 20); err == nil {
			for i := range found {
				entry := ocs.Obj(
					ocs.K("label", shareeLabel(found[i].DisplayName, found[i].GID)),
					ocs.K("value", ocs.Obj(
						ocs.K("shareType", files.ShareTypeGroup),
						ocs.K("shareWith", found[i].GID),
					)),
				)
				if found[i].GID == search {
					exactGroups = append(exactGroups, entry)
				} else {
					localGroups = append(localGroups, entry)
				}
			}
		}
	}
	empty := []any{}
	return ocs.Obj(
		ocs.K("exact", ocs.Obj(
			ocs.K("users", exactUsers),
			ocs.K("groups", exactGroups),
			ocs.K("remotes", remotes),
			ocs.K("remote_groups", empty),
			ocs.K("emails", empty),
			ocs.K("circles", empty),
			ocs.K("rooms", empty),
		)),
		ocs.K("users", localUsers),
		ocs.K("groups", localGroups),
		ocs.K("emails", empty),
		ocs.K("circles", empty),
		ocs.K("rooms", empty),
		ocs.K("remotes", empty),
		ocs.K("remote_groups", empty),
		ocs.K("lookup", lookup),
	)
}

func shareeLabel(displayName, id string) string {
	if displayName != "" {
		return displayName
	}
	return id
}

// recommendedPayload builds the /recommended data map: distinct local
// user/group recipients of the caller's current shares, most recent
// first, capped at 20 each.
func (h ShareesHandler) recommendedPayload(r *http.Request) ocs.OrderedMap {
	usersList := make([]any, 0)
	groupsList := make([]any, 0)
	principal, _ := auth.UserFromContext(r.Context())
	if h.Shares != nil && h.Users != nil && principal != nil {
		if u, err := h.Users.GetByUID(r.Context(), principal.UID); err == nil {
			if shares, err := h.Shares.ListByOwner(r.Context(), u.ID, ""); err == nil {
				sort.SliceStable(shares, func(i, j int) bool { return shares[i].StimeMs > shares[j].StimeMs })
				seenUsers := map[string]bool{}
				seenGroups := map[string]bool{}
				for i := range shares {
					sh := &shares[i]
					switch sh.ShareType {
					case files.ShareTypeUser:
						if seenUsers[sh.ShareWith] || len(usersList) >= 20 {
							continue
						}
						seenUsers[sh.ShareWith] = true
						label := sh.ShareWith
						if tu, err := h.Users.GetByUID(r.Context(), sh.ShareWith); err == nil && tu.DisplayName != "" {
							label = tu.DisplayName
						}
						usersList = append(usersList, ocs.Obj(
							ocs.K("label", label),
							ocs.K("value", ocs.Obj(
								ocs.K("shareType", files.ShareTypeUser),
								ocs.K("shareWith", sh.ShareWith),
							)),
						))
					case files.ShareTypeGroup:
						if seenGroups[sh.ShareWith] || len(groupsList) >= 20 {
							continue
						}
						seenGroups[sh.ShareWith] = true
						label := sh.ShareWith
						if g, err := h.Users.GetGroupByGID(r.Context(), sh.ShareWith); err == nil && g.DisplayName != "" {
							label = g.DisplayName
						}
						groupsList = append(groupsList, ocs.Obj(
							ocs.K("label", label),
							ocs.K("value", ocs.Obj(
								ocs.K("shareType", files.ShareTypeGroup),
								ocs.K("shareWith", sh.ShareWith),
							)),
						))
					}
				}
			}
		}
	}
	empty := []any{}
	return ocs.Obj(
		ocs.K("exact", ocs.Obj(
			ocs.K("users", empty),
			ocs.K("groups", empty),
			ocs.K("remotes", empty),
			ocs.K("remote_groups", empty),
			ocs.K("emails", empty),
			ocs.K("circles", empty),
			ocs.K("rooms", empty),
		)),
		ocs.K("users", usersList),
		ocs.K("groups", groupsList),
		ocs.K("emails", empty),
		ocs.K("circles", empty),
		ocs.K("rooms", empty),
		ocs.K("remotes", empty),
		ocs.K("remote_groups", empty),
		ocs.K("lookup", empty),
	)
}
