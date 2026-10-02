package mail

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// htmlsan_test.go pins the M7 sanitizer policy (ADR-0108 §6): the drop/keep/
// unwrap tag sets, the per-tag attribute allowlists, the URL scheme checks
// (judged on the entity-decoded, control-stripped value), the <a> hardening,
// the size guard, idempotence, and a seeded byte-soup fuzz loop proving no
// panic and re-parseable output on arbitrary input.
//
// Note on expected attribute ORDER: the html parser sorts the attributes of
// formatting elements (a, b, code, em, font, i, s, small, strike, strong, u,
// big — the "active formatting elements" Noah's Ark optimization), so e.g.
// <font> renders color/face/size even when the source order differs. Other
// elements keep source order; the appended target/rel on <a> always come
// last, which keeps the output idempotent across re-parses.

func TestSanitizeHTMLTable(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Drop tag AND contents: script, style, iframe, form, svg, math and
		// the rest of the active/embedding set never reach the output.
		{"script gone", `<p>a</p><script>alert(1)</script><p>b</p>`, `<p>a</p><p>b</p>`},
		{"style gone", `<style>body{color:red}</style><p>x</p>`, `<p>x</p>`},
		{"iframe gone", `<iframe src="https://evil"></iframe><p>x</p>`, `<p>x</p>`},
		{"form subtree gone", `<form><input name=q><button>go</button><textarea>t</textarea><select><option>o</option></select></form><p>x</p>`, `<p>x</p>`},
		{"svg subtree gone", `<svg><script>alert(1)</script><circle/></svg><p>x</p>`, `<p>x</p>`},
		{"math subtree gone", `<math><mtext><script>alert(1)</script></mtext></math><p>x</p>`, `<p>x</p>`},
		{"object embed audio video gone", `<object data="x"><embed src="y"></object><audio src="a"></audio><video><source src="v"></video><p>x</p>`, `<p>x</p>`},
		{"head title meta link base gone", `<head><title>t</title><meta charset="x"><link href="y"><base href="z"></head><p>x</p>`, `<p>x</p>`},
		{"frameset frame applet canvas gone", `<frameset><frame src="x"></frameset><applet code="y"></applet><canvas>c</canvas><p>x</p>`, `<p>x</p>`},
		{"template noscript gone", `<template><p>x</p></template><noscript><p>y</p></noscript><p>z</p>`, `<p>z</p>`},
		{"xmp plaintext listing gone", `<xmp><b>raw</b></xmp><listing>raw</listing><p>x</p>`, `<p>x</p>`},
		{"plaintext swallows the rest", `<p>x</p><plaintext>never <b>rendered</b>`, `<p>x</p>`},
		{"noembed noframes gone", `<noembed>x</noembed><noframes>y</noframes><p>z</p>`, `<p>z</p>`},

		// Event handlers and non-allowlisted attributes are stripped; the
		// allowlisted ones survive (this covers on*, style, srcset,
		// background, formaction by default).
		{"onclick style stripped, title kept", `<p onclick="x" style="color:red" title="ok">hi</p>`, `<p title="ok">hi</p>`},
		{"img srcset onerror background stripped", `<img src="http://x/y.png" srcset="http://x 2x" onerror="pwn()" background="b" alt="a" width="10" height="20">`, `<img src="http://x/y.png" alt="a" width="10" height="20"/>`},
		{"div formaction stripped", `<div formaction="http://evil" data-x="1" dir="ltr">x</div>`, `<div dir="ltr">x</div>`},
		{"blockquote cite stripped", `<blockquote cite="http://x">q</blockquote>`, `<blockquote>q</blockquote>`},
		{"q cite stripped", `<q cite="http://x">q</q>`, `<q>q</q>`},
		{"namespaced attr dropped", `<p xml:lang="en" lang="de">x</p>`, `<p lang="de">x</p>`},
		{"uppercase folded", `<P TITLE="ok">x</P>`, `<p title="ok">x</p>`},
		{"global attrs on span", `<span dir="rtl" lang="ar" title="t" id="i" class="c">x</span>`, `<span dir="rtl" lang="ar" title="t">x</span>`},

		// URL policy: dangerous schemes lose the attribute, the element and
		// its text survive.
		{"javascript href dropped, text kept", `<a href="javascript:alert(1)">click me</a>`, `<a target="_blank" rel="noopener noreferrer">click me</a>`},
		{"tab-in-scheme neutralized", "<a href=\"java\tscript:alert(1)\">x</a>", `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"entity-tab-in-scheme neutralized", `<a href="jav&#x09;ascript:alert(1)">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"leading space scheme neutralized", `<a href=" javascript:alert(1)">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"entity-colon trick judged post-decode", `<a href="javascript&colon;alert(1)">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"uppercase scheme neutralized", `<a href=" JAVASCRIPT:alert(1)">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"newline-in-scheme neutralized", "<a href=\"java\nscript:alert(1)\">x</a>", `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"vbscript dropped", `<a href="vbscript:msgbox(1)">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"file dropped", `<a href="file:///etc/passwd">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"data text/html dropped", `<a href="data:text/html,<script>alert(1)</script>">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"scheme-relative dropped", `<a href="//example.com/x">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"relative path dropped", `<a href="/relative/path">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"fragment-only dropped", `<a href="#anchor">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"data image kept on img", `<img src="data:image/png;base64,iVBORw0KGgo=">`, `<img src="data:image/png;base64,iVBORw0KGgo="/>`},
		{"data image uppercase kept on img", `<img src="DATA:IMAGE/GIF;base64,R0lGODlh">`, `<img src="DATA:IMAGE/GIF;base64,R0lGODlh"/>`},
		{"data image dropped on a", `<a href="data:image/png;base64,iVBORw0KGgo=">x</a>`, `<a target="_blank" rel="noopener noreferrer">x</a>`},
		{"data text/html dropped on img", `<img src="data:text/html,<b>x</b>">`, `<img/>`},
		{"mailto kept on a", `<a href="mailto:bob@example.com">mail</a>`, `<a href="mailto:bob@example.com" target="_blank" rel="noopener noreferrer">mail</a>`},
		{"mailto dropped on img", `<img src="mailto:bob@example.com">`, `<img/>`},
		{"cid kept on img", `<img src="cid:part1@msg" alt="logo">`, `<img src="cid:part1@msg" alt="logo"/>`},
		{"cid kept on a", `<a href="cid:logo@msg">inline</a>`, `<a href="cid:logo@msg" target="_blank" rel="noopener noreferrer">inline</a>`},
		{"http kept", `<a href="http://example.com/x">x</a>`, `<a href="http://example.com/x" target="_blank" rel="noopener noreferrer">x</a>`},
		{"https uppercase scheme kept", `<a href="HTTPS://EXAMPLE.com/x">x</a>`, `<a href="HTTPS://EXAMPLE.com/x" target="_blank" rel="noopener noreferrer">x</a>`},
		{"query ampersand re-escaped", `<a href="http://x?a=1&b=2">q</a>`, `<a href="http://x?a=1&amp;b=2" target="_blank" rel="noopener noreferrer">q</a>`},

		// <a> hardening: every kept link opens a new tab without opener
		// access, whatever the source said.
		{"a gets target rel", `<a href="http://example.com">x</a>`, `<a href="http://example.com" target="_blank" rel="noopener noreferrer">x</a>`},
		{"existing target rel replaced", `<a href="http://x" target="_self" rel="nofollow">y</a>`, `<a href="http://x" target="_blank" rel="noopener noreferrer">y</a>`},
		{"anchor name kept and hardened", `<a name="top">x</a>`, `<a name="top" target="_blank" rel="noopener noreferrer">x</a>`},

		// Unknown elements unwrap: the tag goes, the children stay.
		{"unknown tag unwrapped", `<marquee><b>hi</b></marquee>`, `<b>hi</b>`},
		{"custom element unwrapped", `<foo bar="1">x</foo>`, `x`},
		{"nested unknown unwrapped", `<article><section><p>x</p></section></article>`, `<p>x</p>`},

		// Parser-level attacks: the tree builder defuses them before the
		// policy ever runs.
		{"nested angle script", `<<script>script>`, `&lt;`},
		{"unclosed tags auto-closed", `<div><p>hi`, `<div><p>hi</p></div>`},
		{"comment smuggling", `<!--<script>--><p>x</p>`, `<p>x</p>`},
		{"comment dropped", `<p>a<!-- note -->b</p>`, `<p>ab</p>`},
		{"doctype dropped", `<!DOCTYPE html><p>x</p>`, `<p>x</p>`},

		// Structure and per-tag attribute allowlists.
		{"table colspan survives", `<table><tr><td colspan="2">x</td></tr></table>`, `<table><tbody><tr><td colspan="2">x</td></tr></tbody></table>`},
		{"th td colspan rowspan", `<table><tr><th colspan="2" rowspan="3">h</th></tr><tr><td rowspan="2" width="9">d</td></tr></table>`, `<table><tbody><tr><th colspan="2" rowspan="3">h</th></tr><tr><td rowspan="2">d</td></tr></tbody></table>`},
		{"col colgroup span width", `<table><colgroup span="1" width="20"><col span="2" width="10"></colgroup><tr><td>x</td></tr></table>`, `<table><colgroup span="1" width="20"><col span="2" width="10"/></colgroup><tbody><tr><td>x</td></tr></tbody></table>`},
		{"ol ul li attrs", `<ol start="3" type="a"><li value="5">x</li></ol><ul type="disc"><li>y</li></ul>`, `<ol start="3" type="a"><li value="5">x</li></ol><ul type="disc"><li>y</li></ul>`},
		{"font attrs (parser sorts formatting attrs)", `<font color="red" size="3" face="serif" style="x">hi</font>`, `<font color="red" face="serif" size="3">hi</font>`},
		{"foster-parented table text", `<table>foster<td>x</td></table>`, `foster<table><tbody><tr><td>x</td></tr></tbody></table>`},
		{"full kept structure", `<h1>t</h1><h6>u</h6><pre>pre</pre><code>c</code><figure><figcaption>cap</figcaption></figure><center>c</center><dl><dt>d</dt><dd>e</dd></dl>`, `<h1>t</h1><h6>u</h6><pre>pre</pre><code>c</code><figure><figcaption>cap</figcaption></figure><center>c</center><dl><dt>d</dt><dd>e</dd></dl>`},
		{"inline kept set", `<mark>m</mark><abbr>a</abbr><cite>c</cite><q>q</q><kbd>k</kbd><samp>s</samp><var>v</var><time>t</time><wbr><sub>2</sub><sup>3</sup><big>b</big><small>s</small><strike>x</strike><u>u</u><s>y</s><em>e</em><strong>st</strong><hr><br>`, `<mark>m</mark><abbr>a</abbr><cite>c</cite><q>q</q><kbd>k</kbd><samp>s</samp><var>v</var><time>t</time><wbr/><sub>2</sub><sup>3</sup><big>b</big><small>s</small><strike>x</strike><u>u</u><s>y</s><em>e</em><strong>st</strong><hr/><br/>`},
		{"caption thead tfoot kept", `<table><caption>c</caption><thead><tr><th>h</th></tr></thead><tbody><tr><td>d</td></tr></tbody><tfoot><tr><td>f</td></tr></tfoot></table>`, `<table><caption>c</caption><thead><tr><th>h</th></tr></thead><tbody><tr><td>d</td></tr></tbody><tfoot><tr><td>f</td></tr></tfoot></table>`},

		// Text handling: escaped on render, whitespace untouched, entities
		// decoded by the parser and re-escaped canonically.
		{"text escaped", `a & b < c`, `a &amp; b &lt; c`},
		{"whitespace preserved", "<p>a  b\nc</p>", "<p>a  b\nc</p>"},
		{"entities re-escaped canonically", `<p title="a&quot;b">&lt;tag&gt;</p>`, `<p title="a&#34;b">&lt;tag&gt;</p>`},
		{"empty input", ``, ``},
		{"plain text only", `hello world`, `hello world`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeHTML(tt.in)
			if got != tt.want {
				t.Errorf("SanitizeHTML(%q)\n  = %q\nwant %q", tt.in, got, tt.want)
			}
			// Idempotence: sanitizing the sanitized output is a fixed point.
			if again := SanitizeHTML(got); again != got {
				t.Errorf("not idempotent: SanitizeHTML(%q) = %q", got, again)
			}
		})
	}
}

// TestSanitizeHTMLSizeGuard: an HTML part over 2MB sanitizes to "" (the
// caller falls back to the plain body); the boundary itself still sanitizes.
func TestSanitizeHTMLSizeGuard(t *testing.T) {
	over := strings.Repeat("x", sanMaxInput+1)
	if got := SanitizeHTML(over); got != "" {
		t.Errorf("oversized input must render empty, got %d bytes", len(got))
	}
	at := "<p>" + strings.Repeat("x", sanMaxInput-7) + "</p>" // exactly 2MB
	if got := SanitizeHTML(at); got == "" {
		t.Error("input at the limit must still sanitize")
	}
}

// TestSanitizeHTMLPathological: deep nesting and an attribute flood
// terminate without exhausting the stack (the walk and the render are
// iterative). The html parser itself caps the open-element stack at 512,
// so insane nesting fails closed to "" (the caller serves the plain body);
// nesting just under the cap still round-trips byte-exact.
func TestSanitizeHTMLPathological(t *testing.T) {
	insane := strings.Repeat("<div>", 100_000) + "x" + strings.Repeat("</div>", 100_000)
	if got := SanitizeHTML(insane); got != "" {
		t.Errorf("nesting past the parser's depth cap must fail closed, got %d bytes", len(got))
	}
	deep := strings.Repeat("<div>", 500) + "x" + strings.Repeat("</div>", 500)
	if got := SanitizeHTML(deep); got != deep {
		t.Errorf("deep nesting under the cap: got %d bytes, want %d", len(got), len(deep))
	}
	var b strings.Builder
	b.WriteString("<p")
	for i := range 100_000 {
		fmt.Fprintf(&b, " a%d=x", i)
	}
	b.WriteString(">hi</p>")
	if got := SanitizeHTML(b.String()); got != "<p>hi</p>" {
		t.Errorf("attribute flood: got %d bytes", len(got))
	}
}

// TestSanitizeHTMLFuzz: seeded random byte soup (10k iterations) — half raw
// bytes, half biased towards HTML metacharacters — must never panic, and the
// output must re-parse and be a fixed point.
func TestSanitizeHTMLFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(20261002))
	soup := []byte("<>&\"'=/:;&# \t\nscriptSCRIPThrefHREFsrcdivaA0\t9x")
	for i := range 10_000 {
		buf := make([]byte, rng.Intn(512))
		for j := range buf {
			if i%2 == 0 {
				buf[j] = byte(rng.Intn(256))
			} else {
				buf[j] = soup[rng.Intn(len(soup))]
			}
		}
		in := string(buf)
		out := SanitizeHTML(in)
		if _, err := html.Parse(strings.NewReader(out)); err != nil {
			t.Fatalf("iter %d: output of %q does not re-parse: %q: %v", i, in, out, err)
		}
		if again := SanitizeHTML(out); again != out {
			t.Fatalf("iter %d: not idempotent: %q -> %q -> %q", i, in, out, again)
		}
	}
}
