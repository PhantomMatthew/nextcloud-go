package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// FilesProvider supplies files.chunked_upload limits advertised to clients.
type FilesProvider struct {
	ChunkedMaxSize     int64
	ChunkedMaxParallel int
	Undelete           bool
	Versioning         bool
}

func DefaultFilesProvider() FilesProvider {
	return FilesProvider{
		ChunkedMaxSize:     5368709120,
		ChunkedMaxParallel: 20,
		Undelete:           true,
		Versioning:         true,
	}
}

func (f FilesProvider) GetCapabilities() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("files", ocs.Obj(
			ocs.K("chunked_upload", ocs.Obj(
				ocs.K("max_size", f.ChunkedMaxSize),
				ocs.K("max_parallel_count", f.ChunkedMaxParallel),
			)),
			ocs.K("undelete", f.Undelete),
			ocs.K("versioning", f.Versioning),
		)),
	)
}
