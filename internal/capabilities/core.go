package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// CoreProvider supplies the "core" capability block: poll interval,
// WebDAV root, and reference-resolution metadata. Field ordering and
// values match upstream OCSController::getCapabilities (Phase 1
// minimum subset).
type CoreProvider struct {
	PollInterval   int
	WebDAVRoot     string
	ReferenceAPI   bool
	ReferenceRegex string
}

func DefaultCoreProvider() CoreProvider {
	return CoreProvider{
		PollInterval:   60,
		WebDAVRoot:     "remote.php/webdav",
		ReferenceAPI:   true,
		ReferenceRegex: `(\s|\n|^)(https?:\/\/)((?:[-A-Z0-9+_]+\.)+[-A-Z]+(?:\/[-A-Z0-9+&@#%?=~_|!:,.;()]*)*)(\s|\n|$)`,
	}
}

func (c CoreProvider) GetCapabilities() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("core", ocs.Obj(
			ocs.K("pollinterval", c.PollInterval),
			ocs.K("webdav-root", c.WebDAVRoot),
			ocs.K("reference-api", c.ReferenceAPI),
			ocs.K("reference-regex", c.ReferenceRegex),
		)),
	)
}
