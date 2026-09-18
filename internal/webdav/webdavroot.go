package webdav

import "strings"

func (h *Handler) parseOwnerPath(p, user string) (string, string, bool) {
	if user == "" || !strings.HasPrefix(p, h.Prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, h.Prefix)
	if rest == "" {
		return user, "/", true
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	if len(rest) > 1 {
		rest = strings.TrimSuffix(rest, "/")
		if rest == "" {
			rest = "/"
		}
	}
	return user, rest, true
}
