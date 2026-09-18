package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// DAVProvider supplies the "dav" capability block used by desktop chunking NG.
type DAVProvider struct {
	Chunking string
}

func DefaultDAVProvider() DAVProvider {
	return DAVProvider{Chunking: "1.0"}
}

func (d DAVProvider) GetCapabilities() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("dav", ocs.Obj(
			ocs.K("chunking", d.Chunking),
		)),
	)
}
