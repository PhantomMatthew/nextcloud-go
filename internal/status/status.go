package status

import (
	"fmt"
	"net/http"

	"github.com/PhantomMatthew/nextcloud-go/internal/version"
)

type Provider struct {
	Installed       bool
	Maintenance     bool
	NeedsDBUpgrade  bool
	ExtendedSupport bool
}

func (p Provider) versionString() string {
	v := version.Version
	return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3])
}

// Handler serves /status.php with byte-exact JSON field order matching
// upstream Nextcloud (lib/private/legacy/OC_Util.php) for client compatibility.
func (p Provider) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(
			`{"installed":%s,"maintenance":%s,"needsDbUpgrade":%s,"version":%q,"versionstring":%q,"edition":%q,"productname":%q,"extendedSupport":%s}`,
			boolJSON(p.Installed),
			boolJSON(p.Maintenance),
			boolJSON(p.NeedsDBUpgrade),
			p.versionString(),
			version.VersionString,
			version.Edition,
			version.ProductName,
			boolJSON(p.ExtendedSupport),
		)
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
