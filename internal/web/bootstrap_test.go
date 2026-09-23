package web

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// testState is the fully populated session-shaped state the assembler tests
// snapshot against.
var testState = BootstrapState{
	Version:           "26.0.0.6",
	VersionString:     "26.0.0 beta 4",
	ModRewriteWorking: true,
	SessionKeepalive:  true,
	SessionLifetime:   86400,
	User:              &BootstrapUser{UID: "alice", DisplayName: "Alice"},
}

// stateScript extracts the bootstrap state script block (the one that is
// not the oc_requesttoken block) from an injected shell body.
func stateScript(t *testing.T, body string) string {
	t.Helper()
	var blocks []string
	rest := body
	for {
		start := strings.Index(rest, "<script>")
		if start < 0 {
			break
		}
		rest = rest[start+len("<script>"):]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			t.Fatalf("unterminated script block in %q", body)
		}
		blocks = append(blocks, rest[:end])
		rest = rest[end:]
	}
	var state []string
	for _, b := range blocks {
		if !strings.Contains(b, "oc_requesttoken") {
			state = append(state, b)
		}
	}
	if len(state) != 1 {
		t.Fatalf("state script blocks = %d, want 1: %q", len(state), body)
	}
	return state[0]
}

// parseGlobals decodes the window.<name>=<json>; assignments of a state
// script block, proving every value is parseable JSON.
func parseGlobals(t *testing.T, script string) map[string]json.RawMessage {
	t.Helper()
	globals := map[string]json.RawMessage{}
	rest := script
	for rest != "" {
		if !strings.HasPrefix(rest, "window.") {
			t.Fatalf("malformed bootstrap script at %q", rest)
		}
		rest = strings.TrimPrefix(rest, "window.")
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			t.Fatalf("malformed bootstrap assignment at %q", rest)
		}
		name := rest[:eq]
		var raw json.RawMessage
		dec := json.NewDecoder(strings.NewReader(rest[eq+1:]))
		if err := dec.Decode(&raw); err != nil {
			t.Fatalf("bootstrap value for window.%s does not parse: %v", name, err)
		}
		globals[name] = raw
		rest = rest[eq+1+int(dec.InputOffset()):]
		if !strings.HasPrefix(rest, ";") {
			t.Fatalf("bootstrap assignment window.%s missing terminator at %q", name, rest)
		}
		rest = rest[1:]
	}
	return globals
}

func jsonKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("not a JSON object: %v", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBootstrapScriptKeySnapshot(t *testing.T) {
	t.Parallel()
	script := stateScript(t, string(injectShell([]byte(indexBody), "", testState)))
	globals := parseGlobals(t, script)

	names := make([]string, 0, len(globals))
	for name := range globals {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"_oc_config", "_oc_webroot", "oc_appconfig"}; !equalStrings(names, want) {
		t.Errorf("injected globals = %v, want %v", names, want)
	}

	if want := []string{"modRewriteWorking", "session_keepalive", "session_lifetime", "version", "versionstring"}; !equalStrings(jsonKeys(t, globals["_oc_config"]), want) {
		t.Errorf("_oc_config keys = %v, want %v", jsonKeys(t, globals["_oc_config"]), want)
	}

	if got := jsonKeys(t, globals["oc_appconfig"]); !equalStrings(got, []string{"core"}) {
		t.Errorf("oc_appconfig keys = %v, want [core]", got)
	}
	var appCfg struct {
		Core json.RawMessage `json:"core"`
	}
	if err := json.Unmarshal(globals["oc_appconfig"], &appCfg); err != nil {
		t.Fatal(err)
	}
	wantCore := []string{
		"allowGroupSharing",
		"defaultExpireDate",
		"defaultExpireDateEnabled",
		"defaultExpireDateEnforced",
		"defaultInternalExpireDate",
		"defaultInternalExpireDateEnabled",
		"defaultInternalExpireDateEnforced",
		"defaultRemoteExpireDate",
		"defaultRemoteExpireDateEnabled",
		"defaultRemoteExpireDateEnforced",
		"enableLinkPasswordByDefault",
		"enforcePasswordForPublicLink",
		"federatedCloudShareDoc",
		"remoteShareAllowed",
		"resharingAllowed",
		"sharingDisabledForUser",
	}
	if got := jsonKeys(t, appCfg.Core); !equalStrings(got, wantCore) {
		t.Errorf("oc_appconfig.core keys = %v, want %v", got, wantCore)
	}
}

func TestBootstrapScriptExactSnapshot(t *testing.T) {
	t.Parallel()
	want := `<script>window._oc_webroot="";` +
		`window._oc_config={"modRewriteWorking":true,"session_keepalive":true,"session_lifetime":86400,"version":"26.0.0.6","versionstring":"26.0.0 beta 4"};` +
		`window.oc_appconfig={"core":{` +
		`"defaultExpireDateEnabled":false,"defaultExpireDate":null,"defaultExpireDateEnforced":null,` +
		`"enforcePasswordForPublicLink":false,"enableLinkPasswordByDefault":false,"sharingDisabledForUser":false,` +
		`"resharingAllowed":true,"remoteShareAllowed":true,` +
		`"federatedCloudShareDoc":"https://docs.nextcloud.com/server/26/user_manual/en/files/federated_cloud_sharing.html",` +
		`"allowGroupSharing":true,` +
		`"defaultInternalExpireDateEnabled":false,"defaultInternalExpireDate":null,"defaultInternalExpireDateEnforced":null,` +
		`"defaultRemoteExpireDateEnabled":false,"defaultRemoteExpireDate":null,"defaultRemoteExpireDateEnforced":null` +
		`}};</script>`
	if got := string(bootstrapScript("", testState)); got != want {
		t.Errorf("bootstrap script:\n got %q\nwant %q", got, want)
	}
}

func TestBootstrapScriptIdenticalAcrossSessionStates(t *testing.T) {
	t.Parallel()
	anon := testState
	anon.User = nil
	if gotSession, gotAnon := bootstrapScript("tok", testState), bootstrapScript("tok", anon); string(gotSession) != string(gotAnon) {
		t.Errorf("state script must not differ by session:\nsession %q\nanon    %q", gotSession, gotAnon)
	}
}

func TestBootstrapScriptTokenBlockShape(t *testing.T) {
	t.Parallel()
	got := string(bootstrapScript("tok-abc", testState))
	if !strings.HasPrefix(got, `<script>window.oc_requesttoken="tok-abc";</script><script>window._oc_webroot=`) {
		t.Errorf("token block must lead, state block follow: %q", got)
	}
}

func TestBootstrapScriptNoScriptBreakout(t *testing.T) {
	t.Parallel()
	// Every value-bearing field is probed; encoding/json escapes <, >, and
	// &, so no payload may contain a literal script-closing tag.
	hostile := `</script><script>alert(1)</script>`
	state := testState
	state.Webroot = hostile
	state.Version = hostile
	state.VersionString = hostile
	state.User = &BootstrapUser{UID: hostile, DisplayName: hostile}
	body := string(injectShell([]byte(indexBody), "", state))
	if strings.Contains(body, hostile) {
		t.Fatalf("hostile payload survived injection: %q", body)
	}
	if got := strings.Count(body, "</script>"); got != 1 {
		t.Errorf("</script> count = %d, want 1: %q", got, body)
	}
	// The JSON form escapes the angle brackets; the attribute form
	// HTML-escapes them.
	if !strings.Contains(body, `\u003c/script\u003e`) {
		t.Error("JSON escaping missing from the state script")
	}
	if !strings.Contains(body, `&lt;/script&gt;`) {
		t.Error("HTML escaping missing from the head attribute")
	}
}

func TestInjectShellWithoutHeadTagPrependsScripts(t *testing.T) {
	t.Parallel()
	body := string(injectShell([]byte("<title>fragment</title>"), "tok-frag", testState))
	if !strings.HasPrefix(body, `<script>window.oc_requesttoken="tok-frag";</script><script>window._oc_webroot=`) {
		t.Errorf("headless shell must get both scripts prepended: %q", body)
	}
}

func TestInjectShellSessionReplacesBakedUserAttributes(t *testing.T) {
	t.Parallel()
	shell := `<!doctype html><html><head data-user-displayname='Stale' data-user="stale"><title>x</title></head><body/></html>`
	body := string(injectShell([]byte(shell), "", testState))
	if strings.Contains(body, "stale") || strings.Contains(body, "Stale") {
		t.Errorf("baked user attributes must be replaced: %q", body)
	}
	if got := strings.Count(body, `data-user="alice"`); got != 1 {
		t.Errorf("data-user occurrences = %d, want 1: %q", got, body)
	}
	if got := strings.Count(body, `data-user-displayname="Alice"`); got != 1 {
		t.Errorf("data-user-displayname occurrences = %d, want 1: %q", got, body)
	}
}
