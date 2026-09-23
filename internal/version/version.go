package version

import "fmt"

var (
	Version       = [4]int{26, 0, 0, 6}
	VersionString = "26.0.0 beta 4"
	Edition       = ""
	ProductName   = "Nextcloud"
)

// String returns the dotted four-part server version ("26.0.0.6"), the shape
// upstream's implode('.', Util::getVersion()) produces.
func String() string {
	v := Version
	return fmt.Sprintf("%d.%d.%d.%d", v[0], v[1], v[2], v[3])
}
