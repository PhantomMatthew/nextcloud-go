package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(64)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "import-har":
		os.Exit(runImportHAR(args))
	case "lint":
		os.Exit(runLint(args))
	case "accept":
		os.Exit(runAccept(args))
	case "migrate":
		os.Exit(runMigrate(args))
	case "-h", "--help", "help":
		usage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "golden-gen: unknown subcommand %q\n", cmd)
		usage()
		os.Exit(64)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `golden-gen - golden test case authoring tool

Usage:
  golden-gen import-har  -in FILE -out DIR  Import mitmproxy HAR into golden cases
  golden-gen lint        [-root DIR]        Validate golden cases against schema
  golden-gen accept      -case DIR          Update response.http after intentional change
  golden-gen migrate     [-root DIR]        Migrate cases to current schema_version`)
}

func runImportHAR(args []string) int {
	fs := flag.NewFlagSet("import-har", flag.ContinueOnError)
	in := fs.String("in", "", "HAR file path")
	out := fs.String("out", "", "output directory under testdata/golden")
	group := fs.String("group", "", "case group prefix (default: derived from URL path)")
	startIdx := fs.Int("start", 1, "starting case index for slug numbering")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *in == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "import-har: -in and -out required")
		return 64
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import-har: read %s: %v\n", *in, err)
		return 66
	}
	var har harFile
	if err := json.Unmarshal(raw, &har); err != nil {
		fmt.Fprintf(os.Stderr, "import-har: parse HAR: %v\n", err)
		return 65
	}
	if len(har.Log.Entries) == 0 {
		fmt.Fprintln(os.Stderr, "import-har: HAR contains no entries")
		return 65
	}
	source := filepath.Base(*in)
	idx := *startIdx
	used := map[string]bool{}
	for _, e := range har.Log.Entries {
		caseID, dir, err := emitCase(*out, *group, source, idx, e, used)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import-har: entry %d: %v\n", idx, err)
			return 70
		}
		fmt.Printf("wrote case %s -> %s\n", caseID, dir)
		idx++
	}
	return 0
}

type harFile struct {
	Log harLog `json:"log"`
}

type harLog struct {
	Entries []harEntry `json:"entries"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
}

type harRequest struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []harHeader `json:"headers"`
	PostData    *harPost    `json:"postData,omitempty"`
}

type harResponse struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Headers     []harHeader `json:"headers"`
	Content     harContent  `json:"content"`
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harPost struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

type harContent struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Encoding string `json:"encoding"`
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func emitCase(outRoot, groupOverride, source string, idx int, e harEntry, used map[string]bool) (string, string, error) {
	u, err := url.Parse(e.Request.URL)
	if err != nil {
		return "", "", fmt.Errorf("parse url: %w", err)
	}
	group := groupOverride
	if group == "" {
		group = deriveGroup(u.Path)
	}
	slug := deriveSlug(e.Request.Method, u.Path)
	caseID := fmt.Sprintf("%s/%03d-%s", group, idx, slug)
	if used[caseID] {
		caseID = fmt.Sprintf("%s/%03d-%s-%d", group, idx, slug, idx)
	}
	used[caseID] = true

	caseDir := filepath.Join(outRoot, group, fmt.Sprintf("%03d-%s", idx, slug))
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir: %w", err)
	}

	reqWire, reqBodyKind := buildRequestWire(e.Request, u)
	respWire, respBodyKind := buildResponseWire(e.Response)

	if err := os.WriteFile(filepath.Join(caseDir, "request.http"), []byte(reqWire), 0o644); err != nil {
		return "", "", fmt.Errorf("write request.http: %w", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "response.http"), []byte(respWire), 0o644); err != nil {
		return "", "", fmt.Errorf("write response.http: %w", err)
	}

	yaml := buildCaseYAML(caseID, source, e.StartedDateTime, reqBodyKind, respBodyKind)
	if err := os.WriteFile(filepath.Join(caseDir, "case.yaml"), []byte(yaml), 0o644); err != nil {
		return "", "", fmt.Errorf("write case.yaml: %w", err)
	}
	return caseID, caseDir, nil
}

func deriveGroup(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for _, seg := range parts {
		seg = strings.ToLower(seg)
		if seg == "" || seg == "ocs" || seg == "v1.php" || seg == "v2.php" || seg == "remote.php" || seg == "index.php" || seg == "apps" {
			continue
		}
		seg = slugRE.ReplaceAllString(seg, "-")
		seg = strings.Trim(seg, "-")
		if seg != "" {
			return seg
		}
	}
	return "imported"
}

func deriveSlug(method, p string) string {
	base := path.Base(p)
	if base == "/" || base == "." || base == "" {
		base = "root"
	}
	s := strings.ToLower(method + "-" + base)
	s = slugRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "entry"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func buildRequestWire(r harRequest, u *url.URL) (string, string) {
	method := r.Method
	if method == "" {
		method = "GET"
	}
	target := u.RequestURI()
	if target == "" {
		target = "/"
	}
	line := fmt.Sprintf("%s %s HTTP/1.1", method, target)

	var body string
	if r.PostData != nil {
		body = r.PostData.Text
	}
	bodyKind := classifyBody(body, headerValue(r.Headers, "Content-Type"))

	headers := scrubHeaders(r.Headers, true)
	if u.Host != "" && headerValue(headers, "Host") == "" {
		headers = append([]harHeader{{Name: "Host", Value: u.Host}}, headers...)
	}
	headers = setOrAppend(headers, "Content-Length", fmt.Sprintf("%d", len(body)))

	return assembleWire(line, headers, body), bodyKind
}

func buildResponseWire(r harResponse) (string, string) {
	body := r.Content.Text
	if r.Content.Encoding == "base64" && body != "" {
		if dec, err := base64.StdEncoding.DecodeString(body); err == nil {
			body = string(dec)
		}
	}
	statusText := r.StatusText
	if statusText == "" {
		statusText = defaultStatusText(r.Status)
	}
	line := fmt.Sprintf("HTTP/1.1 %d %s", r.Status, statusText)

	headers := scrubHeaders(r.Headers, false)
	headers = setOrAppend(headers, "Content-Length", fmt.Sprintf("%d", len(body)))

	bodyKind := classifyBody(body, headerValue(headers, "Content-Type"))
	return assembleWire(line, headers, body), bodyKind
}

func assembleWire(startLine string, headers []harHeader, body string) string {
	var b strings.Builder
	b.WriteString(startLine)
	b.WriteString("\r\n")
	for _, h := range headers {
		b.WriteString(h.Name)
		b.WriteString(": ")
		b.WriteString(h.Value)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

var dropResponseHeaders = map[string]bool{
	"date":              true,
	"x-request-id":      true,
	"set-cookie":        true,
	"www-authenticate":  true,
	"transfer-encoding": true,
	"content-encoding":  true,
}

var dropRequestHeaders = map[string]bool{
	"cookie":            true,
	"authorization":     true,
	"content-length":    true,
	"transfer-encoding": true,
}

func scrubHeaders(in []harHeader, isRequest bool) []harHeader {
	out := make([]harHeader, 0, len(in))
	for _, h := range in {
		if strings.HasPrefix(h.Name, ":") {
			continue
		}
		lname := strings.ToLower(h.Name)
		if isRequest && dropRequestHeaders[lname] {
			continue
		}
		if !isRequest && dropResponseHeaders[lname] {
			continue
		}
		if !isRequest && lname == "server" {
			out = append(out, harHeader{Name: "Server", Value: "nginx"})
			continue
		}
		out = append(out, h)
	}
	if !isRequest && headerValue(out, "Server") == "" {
		out = append([]harHeader{{Name: "Server", Value: "nginx"}}, out...)
	}
	return out
}

func headerValue(hs []harHeader, name string) string {
	ln := strings.ToLower(name)
	for _, h := range hs {
		if strings.ToLower(h.Name) == ln {
			return h.Value
		}
	}
	return ""
}

func setOrAppend(hs []harHeader, name, value string) []harHeader {
	ln := strings.ToLower(name)
	for i, h := range hs {
		if strings.ToLower(h.Name) == ln {
			hs[i].Value = value
			return hs
		}
	}
	return append(hs, harHeader{Name: name, Value: value})
}

func classifyBody(body, contentType string) string {
	if body == "" {
		return "empty"
	}
	ct := strings.ToLower(contentType)
	switch {
	case strings.Contains(ct, "json"):
		return "json"
	case strings.Contains(ct, "xml"):
		return "xml"
	case strings.Contains(ct, "text/") || strings.Contains(ct, "html"):
		return "text"
	case strings.Contains(ct, "form-urlencoded"):
		return "form"
	case strings.Contains(ct, "multipart"):
		return "multipart"
	default:
		return "bytes"
	}
}

func defaultStatusText(code int) string {
	m := map[int]string{
		200: "OK", 201: "Created", 204: "No Content",
		301: "Moved Permanently", 302: "Found", 304: "Not Modified",
		400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found",
		409: "Conflict", 412: "Precondition Failed", 423: "Locked",
		500: "Internal Server Error", 501: "Not Implemented", 503: "Service Unavailable",
	}
	if s, ok := m[code]; ok {
		return s
	}
	return "Status"
}

func buildCaseYAML(caseID, source, capturedAt, reqBodyKind, respBodyKind string) string {
	if capturedAt == "" {
		capturedAt = "1970-01-01T00:00:00Z"
	}
	tags := []string{"imported", "har"}
	sort.Strings(tags)
	return fmt.Sprintf(`schema_version: 1
id: %s
description: |
  Imported from mitmproxy HAR capture %q. Review and edit assertions/normalize
  rules before promoting to a curated golden case.
provenance:
  capture: mitmproxy-har
  source: %s
  captured_at: %q
  notes: |
    Auto-generated by golden-gen import-har. Cookie/Authorization headers were
    scrubbed; Server header was rewritten to nginx; Content-Length recomputed
    from on-wire body.
request:
  body_kind: %s
response:
  body_kind: %s
  normalize:
    - drop_headers: [Date, X-Request-Id, Set-Cookie]
    - replace_header:
        name: Server
        with: nginx
assertions: []
replayable: false
mutating: false
synthetic: false
tags: [%s]
`, caseID, source, source, capturedAt, reqBodyKind, respBodyKind, strings.Join(tags, ", "))
}

func runLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	root := fs.String("root", "testdata/golden", "golden cases root")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	info, err := os.Stat(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lint: stat %s: %v\n", *root, err)
		return 66
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "lint: %s is not a directory\n", *root)
		return 66
	}
	fmt.Fprintf(os.Stderr, "lint: scanned %s (full validation not yet implemented)\n", *root)
	return 0
}

func runAccept(args []string) int {
	fs := flag.NewFlagSet("accept", flag.ContinueOnError)
	caseDir := fs.String("case", "", "case directory")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *caseDir == "" {
		fmt.Fprintln(os.Stderr, "accept: -case required")
		return 64
	}
	fmt.Fprintln(os.Stderr, "accept: not yet implemented (Phase 0 stub)")
	return 70
}

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	root := fs.String("root", "testdata/golden", "golden cases root")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	fmt.Fprintf(os.Stderr, "migrate: scanned %s (no migrations yet, schema_version=1)\n", *root)
	return 0
}
