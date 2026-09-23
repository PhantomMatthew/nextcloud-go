package web

import (
	"bytes"
	"encoding/json"
	"html"
	"regexp"
	"strconv"

	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

// BootstrapUser identifies the session user the SPA shell bootstrap is
// personalized with (the <head data-user/data-user-displayname> attributes
// upstream's layout.user.php emits and @nextcloud/auth reads).
type BootstrapUser struct {
	UID         string
	DisplayName string
}

// BootstrapState carries every value the SPA shell bootstrap injects
// (ADR-0069). User is nil for anonymous (login-page) responses; the zero
// value is a valid anonymous state with empty webroot and version.
type BootstrapState struct {
	// Webroot is the path prefix the UI is served under ("" = site root,
	// the only layout ncgo mounts).
	Webroot string
	// Version and VersionString mirror status.php (internal/version).
	Version       string
	VersionString string
	// ModRewriteWorking tells @nextcloud/router whether extensionless
	// (pretty) routes work; ncgo mounts OCS/DAV without /index.php, so the
	// wired value is true.
	ModRewriteWorking bool
	// SessionKeepalive and SessionLifetime (seconds) drive the frontend's
	// session heartbeat and must agree with the login handlers' TTL.
	SessionKeepalive bool
	SessionLifetime  int64
	// User personalizes the shell for a valid session; nil anonymous.
	User *BootstrapUser
}

// bootstrapConfig is the window._oc_config payload (upstream
// JSConfigHelper's $config subset ncgo can provide faithfully).
type bootstrapConfig struct {
	ModRewriteWorking bool   `json:"modRewriteWorking"`
	SessionKeepalive  bool   `json:"session_keepalive"`
	SessionLifetime   int64  `json:"session_lifetime"`
	Version           string `json:"version"`
	VersionString     string `json:"versionstring"`
}

// bootstrapShareConfig mirrors the oc_appconfig core key set upstream
// JSConfigHelper emits, in upstream key order, with the values ncgo's
// sharing implementation defaults to (ADR-0069 per-key rationale).
type bootstrapShareConfig struct {
	DefaultExpireDateEnabled          bool   `json:"defaultExpireDateEnabled"`
	DefaultExpireDate                 *int64 `json:"defaultExpireDate"`
	DefaultExpireDateEnforced         *bool  `json:"defaultExpireDateEnforced"`
	EnforcePasswordForPublicLink      bool   `json:"enforcePasswordForPublicLink"`
	EnableLinkPasswordByDefault       bool   `json:"enableLinkPasswordByDefault"`
	SharingDisabledForUser            bool   `json:"sharingDisabledForUser"`
	ResharingAllowed                  bool   `json:"resharingAllowed"`
	RemoteShareAllowed                bool   `json:"remoteShareAllowed"`
	FederatedCloudShareDoc            string `json:"federatedCloudShareDoc"`
	AllowGroupSharing                 bool   `json:"allowGroupSharing"`
	DefaultInternalExpireDateEnabled  bool   `json:"defaultInternalExpireDateEnabled"`
	DefaultInternalExpireDate         *int64 `json:"defaultInternalExpireDate"`
	DefaultInternalExpireDateEnforced *bool  `json:"defaultInternalExpireDateEnforced"`
	DefaultRemoteExpireDateEnabled    bool   `json:"defaultRemoteExpireDateEnabled"`
	DefaultRemoteExpireDate           *int64 `json:"defaultRemoteExpireDate"`
	DefaultRemoteExpireDateEnforced   *bool  `json:"defaultRemoteExpireDateEnforced"`
}

// shareDefaults returns the oc_appconfig core values faithful to ncgo: no
// expiry enforcement anywhere (nullable keys null, like upstream with the
// feature off), optional link passwords, unrestricted re/group/remote
// sharing, and the upstream manual link for the declared server major.
func shareDefaults() bootstrapShareConfig {
	return bootstrapShareConfig{
		ResharingAllowed:       true,
		RemoteShareAllowed:     true,
		FederatedCloudShareDoc: "https://docs.nextcloud.com/server/" + strconv.Itoa(version.Version[0]) + "/user_manual/en/files/federated_cloud_sharing.html",
		AllowGroupSharing:      true,
	}
}

func (s BootstrapState) config() bootstrapConfig {
	return bootstrapConfig{
		ModRewriteWorking: s.ModRewriteWorking,
		SessionKeepalive:  s.SessionKeepalive,
		SessionLifetime:   s.SessionLifetime,
		Version:           s.Version,
		VersionString:     s.VersionString,
	}
}

// appConfig is the window.oc_appconfig payload: the core share defaults
// only; app-level entries are a deliberate follow-up (ADR-0069).
type appConfig struct {
	Core bootstrapShareConfig `json:"core"`
}

var (
	headOpenPattern       = regexp.MustCompile(`(?i)<head\b`)
	headTokenDouble       = regexp.MustCompile(`(?i)data-requesttoken="[^"]*"`)
	headTokenSingle       = regexp.MustCompile(`(?i)data-requesttoken='[^']*'`)
	headUserDouble        = regexp.MustCompile(`(?i)data-user="[^"]*"`)
	headUserSingle        = regexp.MustCompile(`(?i)data-user='[^']*'`)
	headUserStripDouble   = regexp.MustCompile(`(?i)\s*data-user="[^"]*"`)
	headUserStripSingle   = regexp.MustCompile(`(?i)\s*data-user='[^']*'`)
	headUserDNDouble      = regexp.MustCompile(`(?i)data-user-displayname="[^"]*"`)
	headUserDNSingle      = regexp.MustCompile(`(?i)data-user-displayname='[^']*'`)
	headUserDNStripDouble = regexp.MustCompile(`(?i)\s*data-user-displayname="[^"]*"`)
	headUserDNStripSingle = regexp.MustCompile(`(?i)\s*data-user-displayname='[^']*'`)
)

// injectShell returns the SPA shell carrying the bootstrap requesttoken
// (ADR-0064) and the bootstrap state script (ADR-0069): the
// <head data-requesttoken> attribute plus the window.oc_requesttoken,
// window._oc_webroot, window._oc_config, and window.oc_appconfig globals.
// A session state additionally sets data-user/data-user-displayname; an
// anonymous state strips any operator-baked copies of those attributes so
// the login page never asserts a user. JSON payloads are encoding/json
// output, which escapes <, >, and & — a value can never close the script
// block; attribute values are HTML-escaped.
func injectShell(body []byte, token string, state BootstrapState) []byte {
	script := bootstrapScript(token, state)
	loc := headOpenPattern.FindIndex(body)
	if loc != nil {
		if gtRel := bytes.IndexByte(body[loc[0]:], '>'); gtRel >= 0 {
			gt := loc[0] + gtRel
			tag := string(body[loc[0]:gt])
			if token != "" {
				tag = upsertHeadAttr(tag, headTokenDouble, headTokenSingle, `data-requesttoken="`+token+`"`)
			}
			if state.User != nil {
				tag = upsertHeadAttr(tag, headUserDouble, headUserSingle, `data-user="`+html.EscapeString(state.User.UID)+`"`)
				tag = upsertHeadAttr(tag, headUserDNDouble, headUserDNSingle, `data-user-displayname="`+html.EscapeString(state.User.DisplayName)+`"`)
			} else {
				tag = headUserStripDouble.ReplaceAllString(headUserStripSingle.ReplaceAllString(tag, ""), "")
				tag = headUserDNStripDouble.ReplaceAllString(headUserDNStripSingle.ReplaceAllString(tag, ""), "")
			}
			out := make([]byte, 0, len(body)+len(tag)-(gt-loc[0])+len(script))
			out = append(out, body[:loc[0]]...)
			out = append(out, tag...)
			out = append(out, '>')
			out = append(out, script...)
			return append(out, body[gt+1:]...)
		}
	}
	// No usable <head> tag (e.g. a fragment shell): prepend the scripts so
	// the globals at least exist.
	out := make([]byte, 0, len(script)+len(body))
	out = append(out, script...)
	return append(out, body...)
}

// upsertHeadAttr rewrites the <head ...> tag content (without the closing
// '>') to carry attr: an existing attribute of that name (either quote
// style) is replaced, a missing one is appended.
func upsertHeadAttr(tag string, double, single *regexp.Regexp, attr string) string {
	if double.MatchString(tag) {
		return double.ReplaceAllString(tag, attr)
	}
	if single.MatchString(tag) {
		return single.ReplaceAllString(tag, attr)
	}
	return tag + " " + attr
}

// bootstrapScript assembles the injected script blocks: the legacy
// window.oc_requesttoken global (kept byte-compatible with ADR-0064) and
// one block of window.<name>=<json>; assignments in upstream read order.
func bootstrapScript(token string, state BootstrapState) []byte {
	var b bytes.Buffer
	if token != "" {
		b.WriteString(`<script>window.oc_requesttoken="`)
		b.WriteString(token)
		b.WriteString(`";</script>`)
	}
	b.WriteString("<script>")
	writeGlobal(&b, "_oc_webroot", state.Webroot)
	writeGlobal(&b, "_oc_config", state.config())
	writeGlobal(&b, "oc_appconfig", appConfig{Core: shareDefaults()})
	b.WriteString("</script>")
	return b.Bytes()
}

func writeGlobal(b *bytes.Buffer, name string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		// The payload types are static structs of strings/bools/ints, which
		// cannot fail to marshal; skip rather than emit a partial line.
		return
	}
	b.WriteString("window.")
	b.WriteString(name)
	b.WriteByte('=')
	b.Write(raw)
	b.WriteByte(';')
}
