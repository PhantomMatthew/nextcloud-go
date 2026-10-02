package mail

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlsan.go is the M7 HTML sanitizer (ADR-0108 §6): the message detail
// endpoint runs every HTML body through SanitizeHTML before responding, so
// first-party rendering is unblocked. The sanitizer is allowlist-based and
// built on golang.org/x/net/html — the input is parsed as a body-context
// fragment (never string/regex mangling), the tree is walked once with an
// explicit stack, and the kept nodes are rendered back out. The walk and the
// render are both iterative and allocate proportionally to the input, so
// pathological input (deep nesting, attribute floods) cannot exhaust the
// stack and always terminates.
//
// Policy (pinned by htmlsan_test.go):
//   - DROP tag AND contents: script/style/iframe/object/embed/form and the
//     other active-or-embedding elements (sanDropTags).
//   - KEEP: the formatting/structure set (sanKeepTags) with a per-tag
//     attribute allowlist — everything else (on*, style, srcset, background,
//     formaction, …) is stripped by default.
//   - UNWRAP: any other element — the tag goes, the children stay.
//   - URL attributes (a.href, img.src) are judged on the PARSED value
//     (html.Parse already entity-decoded it) with ASCII control chars and
//     whitespace stripped and the scheme lower-cased: http/https/cid always,
//     mailto on <a> only, and data: on <img> only for image/* payloads.
//     Everything else (javascript:, vbscript:, file:, data:text/html,
//     scheme-less weirdness that does not parse) loses the attribute.
//   - Every kept <a> gets target="_blank" rel="noopener noreferrer".
//   - Comments and the doctype never survive the parse/render path.

// sanMaxInput bounds the sanitizer input; a larger HTML part renders as ""
// and the caller falls back to the plain-text body.
const sanMaxInput = 2 << 20

// sanDropTags lose the element and its whole subtree.
var sanDropTags = map[string]struct{}{
	"script": {}, "style": {}, "iframe": {}, "object": {}, "embed": {},
	"form": {}, "input": {}, "button": {}, "textarea": {}, "select": {},
	"option": {}, "link": {}, "meta": {}, "base": {}, "title": {}, "head": {},
	"applet": {}, "audio": {}, "video": {}, "source": {}, "track": {},
	"canvas": {}, "svg": {}, "math": {}, "template": {}, "noscript": {},
	"frameset": {}, "frame": {}, "noembed": {}, "noframes": {}, "xmp": {},
	"plaintext": {}, "listing": {},
}

// sanKeepTags survive with the attribute policy below; anything else is
// unwrapped (the children are re-homed in place).
var sanKeepTags = map[string]struct{}{
	"p": {}, "br": {}, "hr": {}, "div": {}, "span": {}, "b": {}, "i": {},
	"em": {}, "strong": {}, "u": {}, "s": {}, "strike": {}, "a": {},
	"ul": {}, "ol": {}, "li": {}, "dl": {}, "dt": {}, "dd": {},
	"blockquote": {}, "pre": {}, "code": {},
	"h1": {}, "h2": {}, "h3": {}, "h4": {}, "h5": {}, "h6": {},
	"table": {}, "thead": {}, "tbody": {}, "tfoot": {}, "tr": {}, "td": {},
	"th": {}, "caption": {}, "colgroup": {}, "col": {}, "img": {}, "font": {},
	"small": {}, "big": {}, "sub": {}, "sup": {}, "center": {}, "figure": {},
	"figcaption": {}, "mark": {}, "abbr": {}, "cite": {}, "q": {}, "kbd": {},
	"samp": {}, "var": {}, "time": {}, "wbr": {},
}

// sanVoidTags are the kept elements that render without an end tag (the
// HTML void set intersected with sanKeepTags).
var sanVoidTags = map[string]struct{}{
	"br": {}, "hr": {}, "img": {}, "col": {}, "wbr": {},
}

// sanGlobalAttrs are allowed on every kept element.
var sanGlobalAttrs = map[string]struct{}{
	"title": {}, "dir": {}, "lang": {},
}

// sanTagAttrs is the per-tag attribute allowlist; sanURLTags' href/src
// values additionally pass the scheme check.
var sanTagAttrs = map[string]map[string]struct{}{
	"a":        {"href": {}, "name": {}},
	"img":      {"src": {}, "alt": {}, "width": {}, "height": {}},
	"td":       {"colspan": {}, "rowspan": {}},
	"th":       {"colspan": {}, "rowspan": {}},
	"col":      {"span": {}, "width": {}},
	"colgroup": {"span": {}, "width": {}},
	"ol":       {"start": {}, "type": {}},
	"ul":       {"type": {}},
	"li":       {"value": {}},
	"font":     {"color": {}, "size": {}, "face": {}},
}

// SanitizeHTML renders untrusted HTML safe for first-party rendering per the
// policy in the file comment. An input over sanMaxInput — or one the parser
// rejects (its open-element stack cap) — returns "" and the caller falls
// back to the plain-text body. The output is idempotent:
// SanitizeHTML(SanitizeHTML(x)) == SanitizeHTML(x).
func SanitizeHTML(in string) string {
	if len(in) > sanMaxInput {
		return ""
	}
	ctx := &html.Node{Type: html.ElementNode, DataAtom: atom.Body, Data: "body"}
	roots, err := html.ParseFragment(strings.NewReader(in), ctx)
	if err != nil {
		// The parser fails only on a reader error (impossible here) or when
		// its open-element stack cap (512) is exceeded — insane nesting fails
		// closed and the caller falls back to the plain-text body.
		return ""
	}
	var b strings.Builder
	b.Grow(len(in))
	// One explicit-stack depth-first walk. An end frame re-emits its
	// element's end tag after the children have been processed.
	type frame struct {
		n   *html.Node
		end bool
	}
	stack := make([]frame, 0, 64)
	pushKids := func(n *html.Node) {
		for c := n.LastChild; c != nil; c = c.PrevSibling {
			stack = append(stack, frame{n: c})
		}
	}
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, frame{n: roots[i]})
	}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.end {
			b.WriteString("</")
			b.WriteString(f.n.Data)
			b.WriteByte('>')
			continue
		}
		n := f.n
		switch n.Type {
		case html.ElementNode:
			tag := n.Data
			if _, drop := sanDropTags[tag]; drop {
				continue
			}
			if _, keep := sanKeepTags[tag]; !keep {
				pushKids(n) // unwrap: drop the tag, keep the children
				continue
			}
			writeSanStart(&b, tag, n.Attr)
			if _, void := sanVoidTags[tag]; !void {
				stack = append(stack, frame{n: n, end: true})
			}
			pushKids(n)
		case html.TextNode:
			b.WriteString(html.EscapeString(n.Data))
		default:
			// Comments and doctypes are dropped.
		}
	}
	return b.String()
}

// writeSanStart emits one kept element's start tag with the attribute
// allowlist applied. Kept <a> elements are hardened for a new-tab open.
func writeSanStart(b *strings.Builder, tag string, attrs []html.Attribute) {
	b.WriteByte('<')
	b.WriteString(tag)
	for _, a := range attrs {
		if a.Namespace != "" || !sanAttrOK(tag, a.Key) {
			continue
		}
		if (a.Key == "href" || a.Key == "src") && !sanURLOK(tag, a.Val) {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteString(`="`)
		b.WriteString(html.EscapeString(a.Val))
		b.WriteByte('"')
	}
	if tag == "a" {
		b.WriteString(` target="_blank" rel="noopener noreferrer"`)
	}
	if _, void := sanVoidTags[tag]; void {
		b.WriteString("/>")
		return
	}
	b.WriteByte('>')
}

// sanAttrOK reports whether attr is allowed on tag (global allowlist plus
// the per-tag one).
func sanAttrOK(tag, attr string) bool {
	if _, ok := sanGlobalAttrs[attr]; ok {
		return true
	}
	per, ok := sanTagAttrs[tag]
	if !ok {
		return false
	}
	_, ok = per[attr]
	return ok
}

// sanURLOK judges a href/src value. The value arrived entity-decoded from
// the parser; ASCII control chars and whitespace are stripped and the scheme
// lower-cased before judging, so tab/entity/casing tricks cannot smuggle a
// forbidden scheme past the check.
func sanURLOK(tag, val string) bool {
	v := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, val)
	if tag == "img" && strings.HasPrefix(strings.ToLower(v), "data:image/") {
		return true
	}
	u, err := url.Parse(v)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "cid":
		return true
	case "mailto":
		return tag == "a"
	}
	return false
}
