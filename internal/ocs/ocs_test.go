package ocs_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

const referenceRegex = `\/(\s|\n|^)(https?:\/\/)((?:[-A-Z0-9+_]+\.)+[-A-Z]+(?:\/[-A-Z0-9+&@#%?=~_|!:,.;()]*)*)(\s|\n|$)/i`

func capabilitiesPayload() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("capabilities", ocs.Obj(
			ocs.K("core", ocs.Obj(
				ocs.K("pollinterval", 60),
				ocs.K("webdav-root", "remote.php/webdav"),
				ocs.K("reference-api", true),
				ocs.K("reference-regex", referenceRegex),
			)),
		)),
	)
}

func TestRender_GoldenCapabilities(t *testing.T) {
	tests := []struct {
		name        string
		caseDir     string
		version     ocs.Version
		format      ocs.Format
		statusCode  int
		contentType string
	}{
		{
			name:        "v1_xml",
			caseDir:     "001-anonymous-v1",
			version:     ocs.V1,
			format:      ocs.FormatXML,
			statusCode:  ocs.StatusOKv1,
			contentType: "application/xml; charset=utf-8",
		},
		{
			name:        "v2_json",
			caseDir:     "002-anonymous-v2-json",
			version:     ocs.V2,
			format:      ocs.FormatJSON,
			statusCode:  ocs.StatusOKv2,
			contentType: "application/json; charset=utf-8",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "testdata", "golden", "capabilities", tc.caseDir)
			c, err := goldentest.Load(dir)
			if err != nil {
				t.Fatalf("load case: %v", err)
			}
			parsed, err := goldentest.ParseResponse(c.ResponseRaw)
			if err != nil {
				t.Fatalf("parse response: %v", err)
			}

			body, ct, err := ocs.Render(tc.version, tc.format, ocs.Meta{StatusCode: tc.statusCode}, capabilitiesPayload())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if ct != tc.contentType {
				t.Errorf("content-type mismatch:\n got: %q\nwant: %q", ct, tc.contentType)
			}
			if !bytes.Equal(body, parsed.Body) {
				t.Errorf("body mismatch (len got=%d want=%d):\n--- got ---\n%s\n--- want ---\n%s",
					len(body), len(parsed.Body), body, parsed.Body)
			}
		})
	}
}
