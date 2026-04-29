package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const baseDir = "testdata/golden"

type file struct {
	path    string
	content string
}

func crlf(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func httpResp(status string, headers []string, body string) string {
	wireBody := crlf(body)
	hdrs := append([]string{}, headers...)
	hdrs = append(hdrs, fmt.Sprintf("Content-Length: %d", len(wireBody)))
	head := strings.Join(append([]string{"HTTP/1.1 " + status}, hdrs...), "\r\n")
	return head + "\r\n\r\n" + wireBody
}

// httpReq builds a wire request.
func httpReq(line string, headers []string) string {
	head := strings.Join(append([]string{line}, headers...), "\r\n")
	return head + "\r\n\r\n"
}

func main() {
	files := []file{}

	// Common headers used across responses.
	jsonHdrs := []string{
		"Server: nginx",
		"Content-Type: application/json; charset=utf-8",
	}
	xmlHdrs := []string{
		"Server: nginx",
		"Content-Type: application/xml; charset=utf-8",
	}

	// Reference regex from IURLGenerator::URL_REGEX_NO_MODIFIERS.
	const refRegex = `\/(\s|\n|^)(https?:\/\/)((?:[-A-Z0-9+_]+\.)+[-A-Z]+(?:\/[-A-Z0-9+&@#%?=~_|!:,.;()]*)*)(\s|\n|$)/i`
	refRegexXML := strings.ReplaceAll(refRegex, `&`, `&amp;`)

	// =====================================================================
	// Case 1: capabilities/001-anonymous-v1 (XML, OCS v1)
	// case.yaml + request.http already exist; only response.http needed.
	// =====================================================================
	cap1Body := `<?xml version="1.0"?>
<ocs>
 <meta>
  <status>ok</status>
  <statuscode>100</statuscode>
  <message>OK</message>
 </meta>
 <data>
  <capabilities>
   <core>
    <pollinterval>60</pollinterval>
    <webdav-root>remote.php/webdav</webdav-root>
    <reference-api>1</reference-api>
    <reference-regex>` + refRegexXML + `</reference-regex>
   </core>
  </capabilities>
 </data>
</ocs>
`
	files = append(files, file{
		path:    "capabilities/001-anonymous-v1/response.http",
		content: httpResp("200 OK", xmlHdrs, cap1Body),
	})

	// =====================================================================
	// Case 2: capabilities/002-anonymous-v2-json
	// =====================================================================
	cap2YAML := `schema_version: 1
id: capabilities/002-anonymous-v2-json
description: |
  Anonymous GET of OCS v2 capabilities endpoint with format=json. Returns
  JSON envelope (status=ok, statuscode=200) wrapping the core capabilities
  block. Modern Nextcloud clients prefer this endpoint over the v1 XML
  variant for token negotiation and feature discovery.
provenance:
  capture: synthetic
  source: hand-authored from upstream Nextcloud Server 29.x (lib/private/OCS/CoreCapabilities.php, V2Response.php, BaseResponse.php)
  captured_at: "2025-01-01T00:00:00Z"
  notes: |
    OCS v2 passes through 2xx status codes verbatim and emits statuscode 200
    on success. ?format=json switches BaseResponse from XML to JSON. Replace
    with mitmproxy HAR once a reference instance is available.
request:
  body_kind: empty
response:
  body_kind: json
  normalize:
    - drop_headers: [Date, X-Request-Id, Set-Cookie]
    - replace_header:
        name: Server
        with: nginx
assertions:
  - kind: status_eq
    value: 200
  - kind: header_eq
    name: Content-Type
    value: application/json; charset=utf-8
  - kind: ocs_status_eq
    value: 200
  - kind: json_field_eq
    pointer: /ocs/data/capabilities/core/webdav-root
    value: remote.php/webdav
  - kind: json_field_present
    pointer: /ocs/data/capabilities/core/reference-api
replayable: false
mutating: false
synthetic: true
tags: [capabilities, ocs, v2, anonymous, json, phase1]
`
	cap2Req := httpReq(
		"GET /ocs/v2.php/cloud/capabilities?format=json HTTP/1.1",
		[]string{
			"Host: cloud.example.com",
			"User-Agent: ncgo-goldentest/0.0",
			"Accept: */*",
			"OCS-APIRequest: true",
		},
	)
	cap2Body := `{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},"data":{"capabilities":{"core":{"pollinterval":60,"webdav-root":"remote.php\/webdav","reference-api":true,"reference-regex":"` +
		strings.ReplaceAll(refRegex, `\`, `\\`) + `"}}}}}`
	files = append(files,
		file{path: "capabilities/002-anonymous-v2-json/case.yaml", content: cap2YAML},
		file{path: "capabilities/002-anonymous-v2-json/request.http", content: cap2Req},
		file{path: "capabilities/002-anonymous-v2-json/response.http", content: httpResp("200 OK", jsonHdrs, cap2Body)},
	)

	// =====================================================================
	// Case 3: cloud-user/001-self-v2-json (Basic auth happy path)
	// =====================================================================
	cu1YAML := `schema_version: 1
id: cloud-user/001-self-v2-json
description: |
  Authenticated self-lookup of OCS v2 provisioning_api /cloud/user endpoint
  using HTTP Basic auth. Returns the caller's own user record (id, email,
  display name, quota, language, locale, group memberships). This is the
  bootstrap call clients make right after capabilities to populate the
  user profile sheet.
provenance:
  capture: synthetic
  source: hand-authored from upstream Nextcloud Server 29.x (apps/provisioning_api/lib/Controller/UsersController::getCurrentUser, V2Response.php)
  captured_at: "2025-01-01T00:00:00Z"
  notes: |
    Authorization: Basic dGVzdHVzZXI6cGFzc3dvcmQ= decodes to testuser:password.
    Fixture credentials only; never reuse against a real server. Replace with
    mitmproxy HAR once a reference instance is available.
request:
  body_kind: empty
response:
  body_kind: json
  normalize:
    - drop_headers: [Date, X-Request-Id, Set-Cookie]
    - replace_header:
        name: Server
        with: nginx
    - json_pointer_redact:
        pointer: /ocs/data/quota/used
        with: 0
assertions:
  - kind: status_eq
    value: 200
  - kind: ocs_status_eq
    value: 200
  - kind: json_field_eq
    pointer: /ocs/data/id
    value: testuser
  - kind: json_field_eq
    pointer: /ocs/data/email
    value: testuser@example.com
  - kind: json_field_present
    pointer: /ocs/data/quota/total
replayable: true
mutating: false
synthetic: true
tags: [cloud-user, provisioning, ocs, v2, json, basic-auth, phase1]
`
	cu1Req := httpReq(
		"GET /ocs/v2.php/cloud/user?format=json HTTP/1.1",
		[]string{
			"Host: cloud.example.com",
			"User-Agent: ncgo-goldentest/0.0",
			"Accept: */*",
			"OCS-APIRequest: true",
			"Authorization: Basic dGVzdHVzZXI6cGFzc3dvcmQ=",
		},
	)
	cu1Body := `{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},` +
		`"data":{"enabled":true,"storageLocation":"\/var\/www\/html\/data\/testuser",` +
		`"id":"testuser","lastLogin":1714377600000,"backend":"Database",` +
		`"subadmin":[],"quota":{"free":10737418240,"used":0,"total":10737418240,` +
		`"relative":0,"quota":10737418240},"manager":"","avatarScope":"v2-federated",` +
		`"email":"testuser@example.com","emailScope":"v2-federated",` +
		`"additional_mail":[],"additional_mailScope":[],` +
		`"displayname":"Test User","display-name":"Test User","displaynameScope":"v2-federated",` +
		`"phone":"","phoneScope":"v2-local","address":"","addressScope":"v2-local",` +
		`"website":"","websiteScope":"v2-local","twitter":"","twitterScope":"v2-local",` +
		`"fediverse":"","fediverseScope":"v2-local","organisation":"","organisationScope":"v2-local",` +
		`"role":"","roleScope":"v2-local","headline":"","headlineScope":"v2-local",` +
		`"biography":"","biographyScope":"v2-local","profile_enabled":"1","profile_enabledScope":"v2-local",` +
		`"groups":["users"],"language":"en","locale":"en_US","notify_email":null,` +
		`"backendCapabilities":{"setDisplayName":true,"setPassword":true},"display_name":"Test User"}}}`
	files = append(files,
		file{path: "cloud-user/001-self-v2-json/case.yaml", content: cu1YAML},
		file{path: "cloud-user/001-self-v2-json/request.http", content: cu1Req},
		file{path: "cloud-user/001-self-v2-json/response.http", content: httpResp("200 OK", jsonHdrs, cu1Body)},
	)

	// =====================================================================
	// Case 4: cloud-user/004-unauthenticated-v2-json (401 / OCS 997)
	// =====================================================================
	cu4YAML := `schema_version: 1
id: cloud-user/004-unauthenticated-v2-json
description: |
  Unauthenticated GET of OCS v2 /cloud/user endpoint. The framework's
  OCSMiddleware catches the auth failure and emits OCS statuscode 997
  (UNAUTHORISED), which V2Response maps to HTTP 401. Authoritative
  fixture for client retry/relogin logic.
provenance:
  capture: synthetic
  source: hand-authored from upstream Nextcloud Server 29.x (V2Response::getStatus, OCSController::RESPOND_UNAUTHORISED, OCSMiddleware::afterException)
  captured_at: "2025-01-01T00:00:00Z"
  notes: |
    OCSController::RESPOND_UNAUTHORISED = 997. V2Response maps it to HTTP 401.
    Replace with mitmproxy HAR once a reference instance is available.
request:
  body_kind: empty
response:
  body_kind: json
  normalize:
    - drop_headers: [Date, X-Request-Id, Set-Cookie, WWW-Authenticate]
    - replace_header:
        name: Server
        with: nginx
assertions:
  - kind: status_eq
    value: 401
  - kind: ocs_status_eq
    value: 997
  - kind: ocs_message_contains
    value: Current user is not logged in
replayable: true
mutating: false
synthetic: true
tags: [cloud-user, provisioning, ocs, v2, json, unauthenticated, phase1]
`
	cu4Req := httpReq(
		"GET /ocs/v2.php/cloud/user?format=json HTTP/1.1",
		[]string{
			"Host: cloud.example.com",
			"User-Agent: ncgo-goldentest/0.0",
			"Accept: */*",
			"OCS-APIRequest: true",
		},
	)
	cu4Body := `{"ocs":{"meta":{"status":"failure","statuscode":997,"message":"Current user is not logged in"},"data":[]}}`
	files = append(files,
		file{path: "cloud-user/004-unauthenticated-v2-json/case.yaml", content: cu4YAML},
		file{path: "cloud-user/004-unauthenticated-v2-json/request.http", content: cu4Req},
		file{path: "cloud-user/004-unauthenticated-v2-json/response.http", content: httpResp("401 Unauthorized", jsonHdrs, cu4Body)},
	)

	// =====================================================================
	// Case 5: ocs/002-maintenance-503-v2-json
	// =====================================================================
	ocs2YAML := `schema_version: 1
id: ocs/002-maintenance-503-v2-json
description: |
  OCS v2 request served while the instance is in maintenance mode.
  ocs/v1.php (require_once'd by v2.php) short-circuits all routing and
  emits HTTP 503 with the X-Nextcloud-Maintenance-Mode: 1 header plus an
  OCS Result envelope carrying statuscode 503 and message "Service
  unavailable". Clients use the header to display a maintenance banner
  rather than treating the failure as a server error.
provenance:
  capture: synthetic
  source: hand-authored from upstream Nextcloud Server 29.x (ocs/v1.php maintenance branch, ocs/v2.php loader)
  captured_at: "2025-01-01T00:00:00Z"
  notes: |
    Maintenance check fires before authentication, so this case is anonymous.
    Replace with mitmproxy HAR once a reference instance is available.
request:
  body_kind: empty
response:
  body_kind: json
  normalize:
    - drop_headers: [Date, X-Request-Id, Set-Cookie]
    - replace_header:
        name: Server
        with: nginx
assertions:
  - kind: status_eq
    value: 503
  - kind: header_eq
    name: X-Nextcloud-Maintenance-Mode
    value: "1"
  - kind: ocs_status_eq
    value: 503
  - kind: ocs_message_contains
    value: Service unavailable
replayable: true
mutating: false
synthetic: true
tags: [ocs, v2, json, maintenance, anonymous, phase1]
`
	ocs2Req := httpReq(
		"GET /ocs/v2.php/cloud/capabilities?format=json HTTP/1.1",
		[]string{
			"Host: cloud.example.com",
			"User-Agent: ncgo-goldentest/0.0",
			"Accept: */*",
			"OCS-APIRequest: true",
		},
	)
	ocs2Body := `{"ocs":{"meta":{"status":"failure","statuscode":503,"message":"Service unavailable"},"data":null}}`
	ocs2Hdrs := append([]string{}, jsonHdrs...)
	ocs2Hdrs = append(ocs2Hdrs, "X-Nextcloud-Maintenance-Mode: 1")
	files = append(files,
		file{path: "ocs/002-maintenance-503-v2-json/case.yaml", content: ocs2YAML},
		file{path: "ocs/002-maintenance-503-v2-json/request.http", content: ocs2Req},
		file{path: "ocs/002-maintenance-503-v2-json/response.http", content: httpResp("503 Service Unavailable", ocs2Hdrs, ocs2Body)},
	)

	// Apply CRLF to all .http files; .yaml stays LF.
	for _, f := range files {
		full := filepath.Join(baseDir, f.path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
		out := f.content
		if strings.HasSuffix(f.path, ".http") {
			out = crlf(strings.ReplaceAll(out, "\r\n", "\n"))
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote %d bytes -> %s\n", len(out), full)
	}
}
