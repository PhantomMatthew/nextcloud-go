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

func TestStorageScopeGrants(t *testing.T) {
	var nilCaps *Capabilities
	if nilCaps.canStorageUserRead() || nilCaps.canStorageUserWrite() ||
		nilCaps.canStorageSystemRead() || nilCaps.canStorageSystemWrite() {
		t.Fatal("nil capabilities must deny every storage scope")
	}
	c := &Capabilities{Storage: StorageCapabilities{Read: []string{"user"}, Write: []string{"system"}}}
	if !c.canStorageUserRead() || c.canStorageSystemRead() {
		t.Fatal("read=[user] must grant user reads only")
	}
	if c.canStorageUserWrite() || !c.canStorageSystemWrite() {
		t.Fatal("write=[system] must grant system writes only")
	}
}

func TestCanHTTPOutbound(t *testing.T) {
	var nilCaps *Capabilities
	if nilCaps.canHTTPOutbound("example.com") {
		t.Fatal("nil capabilities must deny outbound HTTP")
	}
	c := &Capabilities{HTTP: HTTPCapabilities{Outbound: []string{"example.com", "api.other.test:8443"}}}
	for _, tc := range []struct {
		hostport string
		want     bool
	}{
		{"example.com", true},
		{"EXAMPLE.com", true},       // case-insensitive
		{"example.com:80", false},   // host-only grant covers no explicit port form
		{"example.com:8080", false}, // non-default port needs its own grant
		{"sub.example.com", false},  // subdomains are not implied
		{"otherexample.com", false}, // no suffix matching
		{"api.other.test:8443", true},
		{"api.other.test", false}, // host:port grant covers no port-less form
		{"api.other.test:443", false},
		{"unlisted.test", false},
	} {
		if got := c.canHTTPOutbound(tc.hostport); got != tc.want {
			t.Errorf("canHTTPOutbound(%q) = %v, want %v", tc.hostport, got, tc.want)
		}
	}
}

func TestHTTPOutboundAllowPrivate(t *testing.T) {
	var nilCaps *Capabilities
	if nilCaps.httpOutboundAllowPrivate() {
		t.Fatal("nil capabilities must deny private-target egress")
	}
	if (&Capabilities{}).httpOutboundAllowPrivate() {
		t.Fatal("absent grant must deny private-target egress")
	}
	c := &Capabilities{HTTP: HTTPCapabilities{OutboundAllowPrivate: true}}
	if !c.httpOutboundAllowPrivate() {
		t.Fatal("outbound_allow_private grant must allow private-target egress")
	}
}

func TestCanSubscribeEvent(t *testing.T) {
	var nilCaps *Capabilities
	if nilCaps.canSubscribeEvent("demo.x") {
		t.Fatal("nil capabilities must deny subscriptions")
	}
	c := &Capabilities{}
	if c.canSubscribeEvent("demo.x") {
		t.Fatal("empty grants must deny subscriptions")
	}
	c.Events.Subscribe = []string{"demo.*", "exact.topic"}
	for _, tc := range []struct {
		topic string
		want  bool
	}{
		{"demo.hello", true},
		{"demo.", true},
		{"exact.topic", true},
		{"other.hello", false},
		{"demo.hello.extra", true}, // * crosses dots (path.Match)
		{"core.user.deleted", false},
	} {
		if got := c.canSubscribeEvent(tc.topic); got != tc.want {
			t.Errorf("canSubscribeEvent(%q) = %v, want %v", tc.topic, got, tc.want)
		}
	}
}
