package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// SharingProvider supplies the files_sharing capability block.
type SharingProvider struct {
	APIEnabled       bool
	Public           bool
	PublicUpload     bool
	PasswordEnforced bool
	UserSharing      bool
	GroupSharing     bool
	Resharing        bool
	Federation       bool
}

func DefaultSharingProvider() SharingProvider {
	return SharingProvider{
		APIEnabled:   true,
		Public:       true,
		PublicUpload: true,
		UserSharing:  true,
		GroupSharing: true,
	}
}

func (s SharingProvider) GetCapabilities() ocs.OrderedMap {
	return ocs.Obj(
		ocs.K("files_sharing", ocs.Obj(
			ocs.K("api_enabled", s.APIEnabled),
			ocs.K("public", ocs.Obj(
				ocs.K("enabled", s.Public),
				ocs.K("password", ocs.Obj(
					ocs.K("enforced", s.PasswordEnforced),
				)),
				ocs.K("upload", s.PublicUpload),
			)),
			ocs.K("user", s.UserSharing),
			ocs.K("group_sharing", s.GroupSharing),
			ocs.K("resharing", s.Resharing),
			ocs.K("federation", s.Federation),
		)),
	)
}
