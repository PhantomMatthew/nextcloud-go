package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// RichdocumentsProvider supplies the "richdocuments" capability block
// (ADR-0106): clients learn that Collabora Online editing exists, which
// mimetypes open in it by default, and which it can merely view. The lists
// are the curated common Collabora set — the capabilities surface is
// synchronous (no ctx), so it cannot consult the WOPI discovery document;
// the viewer remains the source of truth per file type (404 when
// discovery offers no action).
type RichdocumentsProvider struct {
	Version   string
	Product   string
	Mimetypes []string
	// ViewOnly lists mimetypes Collabora renders but does not default-open
	// (upstream's mimetypesNoDefaultOpen).
	ViewOnly []string
}

func DefaultRichdocumentsProvider() RichdocumentsProvider {
	return RichdocumentsProvider{
		Version: "1.0",
		Product: "Collabora Online",
		Mimetypes: []string{
			"application/vnd.oasis.opendocument.text",
			"application/vnd.oasis.opendocument.spreadsheet",
			"application/vnd.oasis.opendocument.presentation",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation",
			"application/msword",
			"application/vnd.ms-excel",
			"application/vnd.ms-powerpoint",
		},
		ViewOnly: []string{"application/pdf"},
	}
}

func (r RichdocumentsProvider) GetCapabilities() ocs.OrderedMap {
	mimetypes := make([]any, 0, len(r.Mimetypes))
	for _, m := range r.Mimetypes {
		mimetypes = append(mimetypes, m)
	}
	viewOnly := make([]any, 0, len(r.ViewOnly))
	for _, m := range r.ViewOnly {
		viewOnly = append(viewOnly, m)
	}
	return ocs.Obj(
		ocs.K("richdocuments", ocs.Obj(
			ocs.K("version", r.Version),
			ocs.K("mimetypes", mimetypes),
			ocs.K("mimetypesNoDefaultOpen", viewOnly),
			ocs.K("productName", r.Product),
			ocs.K("templates", false),
			ocs.K("direct_editing", false),
		)),
	)
}
