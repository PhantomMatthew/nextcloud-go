package capabilities

import "github.com/PhantomMatthew/nextcloud-go/internal/ocs"

// Provider mirrors PHP ICapability::getCapabilities(): the returned
// OrderedMap is merged into the top-level capabilities object under
// the keys it declares.
//
// IPublicCapability filtering (anonymous vs authenticated) is deferred
// until the auth middleware lands; every provider is treated as public
// for now. See docs/adr/0005-auth-strategy.md.
type Provider interface {
	GetCapabilities() ocs.OrderedMap
}
