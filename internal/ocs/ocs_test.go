package ocs_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/PhantomMatthew/nextcloud-go/internal/goldentest"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

const referenceRegex = `(\s|\n|^)(https?:\/\/)((?:[-A-Z0-9+_]+\.)+[-A-Z]+(?:\/[-A-Z0-9+&@#%?=~_|!:,.;()]*)*)(\s|\n|$)`

func capabilitiesPayload() ocs.OrderedMap {
	v := version.Version
	return ocs.Obj(
		ocs.K("version", ocs.Obj(
			ocs.K("major", v[0]),
			ocs.K("minor", v[1]),
			ocs.K("micro", v[2]),
			ocs.K("string", version.VersionString),
			ocs.K("edition", version.Edition),
			ocs.K("extendedSupport", false),
		)),
		ocs.K("capabilities", ocs.Obj(
			ocs.K("core", ocs.Obj(
				ocs.K("pollinterval", 60),
				ocs.K("webdav-root", "remote.php/webdav"),
				ocs.K("reference-api", true),
				ocs.K("reference-regex", referenceRegex),
			)),
			ocs.K("dav", ocs.Obj(
				ocs.K("chunking", "1.0"),
			)),
			ocs.K("files", ocs.Obj(
				ocs.K("chunked_upload", ocs.Obj(
					ocs.K("max_size", int64(5368709120)),
					ocs.K("max_parallel_count", 20),
				)),
				ocs.K("undelete", true),
				ocs.K("versioning", true),
			)),
			ocs.K("files_sharing", ocs.Obj(
				ocs.K("api_enabled", true),
				ocs.K("public", ocs.Obj(
					ocs.K("enabled", true),
					ocs.K("password", ocs.Obj(
						ocs.K("enforced", false),
					)),
					ocs.K("upload", true),
				)),
				ocs.K("user", true),
				ocs.K("group_sharing", true),
				ocs.K("resharing", false),
				ocs.K("federation", ocs.Obj(
					ocs.K("outgoing", true),
					ocs.K("incoming", true),
					ocs.K("expire_date", ocs.Obj(
						ocs.K("enabled", false),
					)),
					ocs.K("expire_date_supported", ocs.Obj(
						ocs.K("enabled", false),
					)),
				)),
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
			want, err := goldentest.Normalize(c, parsed)
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}

			body, ct, err := ocs.Render(tc.version, tc.format, ocs.Meta{StatusCode: tc.statusCode}, capabilitiesPayload())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if ct != tc.contentType {
				t.Errorf("content-type mismatch:\n got: %q\nwant: %q", ct, tc.contentType)
			}
			gotBody := bytes.TrimRight(body, "\r\n")
			if !bytes.Equal(gotBody, want.Body) {
				t.Errorf("body mismatch (len got=%d want=%d):\n--- got ---\n%s\n--- want ---\n%s",
					len(gotBody), len(want.Body), gotBody, want.Body)
			}
		})
	}
}
