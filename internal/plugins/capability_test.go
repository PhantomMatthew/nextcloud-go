package plugins

import "testing"

func TestGlobAny(t *testing.T) {
	cases := []struct {
		globs []string
		s     string
		want  bool
	}{
		{[]string{"calendar_*"}, "calendar_events", true},
		{[]string{"calendar_*"}, "files", false},
		{[]string{"exact"}, "exact", true},
		{[]string{"*.icloud.com:443"}, "caldav.icloud.com:443", true},
		{[]string{"*.icloud.com:443"}, "caldav.icloud.com:8443", false},
		{[]string{"files.*"}, "files.uploaded", true},
		{nil, "anything", false},
	}
	for _, tc := range cases {
		if got := globAny(tc.globs, tc.s); got != tc.want {
			t.Errorf("globAny(%v, %q) = %v, want %v", tc.globs, tc.s, got, tc.want)
		}
	}
}

func TestPrefixAny(t *testing.T) {
	cases := []struct {
		prefixes []string
		s        string
		want     bool
	}{
		{[]string{"/apps/tagger/*"}, "/apps/tagger/api/v1/list", true},
		{[]string{"/apps/tagger/*"}, "/apps/tagger", true},
		{[]string{"/apps/tagger/*"}, "/apps/other/api", false},
		{[]string{"/apps/tagger/api"}, "/apps/tagger/api", true},
		{[]string{"/apps/tagger/api"}, "/apps/tagger/api/list", true},
		{[]string{"/apps/tagger/api"}, "/apps/tagger/api2", false},
		{nil, "/apps/tagger", false},
	}
	for _, tc := range cases {
		if got := prefixAny(tc.prefixes, tc.s); got != tc.want {
			t.Errorf("prefixAny(%v, %q) = %v, want %v", tc.prefixes, tc.s, got, tc.want)
		}
	}
}

func TestCapabilityDefaults(t *testing.T) {
	var nilCaps *Capabilities
	if nilCaps.canDBRead("t") || nilCaps.hasAnyDB() || nilCaps.canRegisterJobs() ||
		nilCaps.canPublishEvent("x") || nilCaps.canRegisterRoute("/apps/x") ||
		nilCaps.hasConfigRead() || nilCaps.hasStorageRead() || nilCaps.hasHTTPOutbound() {
		t.Fatal("nil capabilities must deny everything")
	}
	c := &Capabilities{}
	if c.canPublishEvent("core.user.deleted") {
		t.Fatal("core.* topics must be denied even with empty grants")
	}
	c.Events.Publish = []string{"*"}
	if c.canPublishEvent("core.user.deleted") {
		t.Fatal("core.* topics must be denied even with wildcard grant")
	}
	if !c.canPublishEvent("files.uploaded") {
		t.Fatal("wildcard grant should allow non-core topics")
	}
}
