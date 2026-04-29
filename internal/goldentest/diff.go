package goldentest

import (
	"bytes"
	"fmt"
	"strings"
)

func Diff(want, got *ParsedResponse) string {
	if want == nil || got == nil {
		return "diff: nil response"
	}
	var b strings.Builder
	if want.Status != got.Status {
		fmt.Fprintf(&b, "status: want %d, got %d\n", want.Status, got.Status)
	}
	wantHdrs := CanonicalHeaders(want.Headers)
	gotHdrs := CanonicalHeaders(got.Headers)
	if !equalLines(wantHdrs, gotHdrs) {
		b.WriteString("headers diff:\n")
		writeLineDiff(&b, wantHdrs, gotHdrs)
	}
	if !bytes.Equal(want.Body, got.Body) {
		fmt.Fprintf(&b, "body diff: want %d bytes, got %d bytes\n", len(want.Body), len(got.Body))
	}
	return b.String()
}

func equalLines(a, b []string) bool {
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

func writeLineDiff(b *strings.Builder, want, got []string) {
	w := indexLines(want)
	g := indexLines(got)
	for _, line := range want {
		if _, ok := g[line]; !ok {
			b.WriteString("- " + line + "\n")
		}
	}
	for _, line := range got {
		if _, ok := w[line]; !ok {
			b.WriteString("+ " + line + "\n")
		}
	}
}

func indexLines(lines []string) map[string]struct{} {
	m := make(map[string]struct{}, len(lines))
	for _, l := range lines {
		m[l] = struct{}{}
	}
	return m
}
