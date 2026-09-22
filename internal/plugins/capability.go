package plugins

import (
	"path"
	"strings"
)

// globAny reports whether s matches any of the given globs ("*" and "?"
// wildcards) or equals an entry exactly.
func globAny(globs []string, s string) bool {
	for _, g := range globs {
		if g == s {
			return true
		}
		if ok, err := path.Match(g, s); err == nil && ok {
			return true
		}
	}
	return false
}

// prefixAny reports whether s falls under any listed path prefix. An entry
// ending in "*" matches the stripped prefix itself and everything below it;
// any other entry matches itself or paths directly below it.
func prefixAny(prefixes []string, s string) bool {
	for _, p := range prefixes {
		if strings.HasSuffix(p, "*") {
			base := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(s, base) || s == strings.TrimSuffix(base, "/") {
				return true
			}
			continue
		}
		if s == p || strings.HasPrefix(s, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// canDBRead reports whether the plugin holds any db.read grant covering table.
func (c *Capabilities) canDBRead(table string) bool {
	return c != nil && globAny(c.DB.Read, table)
}

// canDBWrite reports whether the plugin holds any db.write grant covering table.
func (c *Capabilities) canDBWrite(table string) bool {
	return c != nil && globAny(c.DB.Write, table)
}

func (c *Capabilities) hasAnyDB() bool {
	return c != nil && (len(c.DB.Read) > 0 || len(c.DB.Write) > 0)
}

func (c *Capabilities) hasStorageRead() bool {
	return c != nil && len(c.Storage.Read) > 0
}

// storageScopeGranted reports whether scope ("user"/"system") is listed.
func storageScopeGranted(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// canStorageUserRead reports whether the plugin may read the calling user's
// files (storage.read includes "user").
func (c *Capabilities) canStorageUserRead() bool {
	return c != nil && storageScopeGranted(c.Storage.Read, "user")
}

// canStorageUserWrite reports whether the plugin may write the calling
// user's files (storage.write includes "user").
func (c *Capabilities) canStorageUserWrite() bool {
	return c != nil && storageScopeGranted(c.Storage.Write, "user")
}

// canStorageSystemRead reports whether the plugin may read its system
// storage (storage.read includes "system").
func (c *Capabilities) canStorageSystemRead() bool {
	return c != nil && storageScopeGranted(c.Storage.Read, "system")
}

// canStorageSystemWrite reports whether the plugin may write its system
// storage (storage.write includes "system").
func (c *Capabilities) canStorageSystemWrite() bool {
	return c != nil && storageScopeGranted(c.Storage.Write, "system")
}

func (c *Capabilities) hasHTTPOutbound() bool {
	return c != nil && len(c.HTTP.Outbound) > 0
}

// httpOutboundAllowPrivate reports whether the plugin may dial loopback,
// private, link-local, and unspecified targets; a nil capabilities pointer
// denies, like every other check.
func (c *Capabilities) httpOutboundAllowPrivate() bool {
	return c != nil && c.HTTP.OutboundAllowPrivate
}

// canHTTPOutbound reports whether hostport ("host" or "host:port", already
// default-port-normalized by the caller) is covered by the http.outbound
// grants. Matching is exact and case-insensitive: a host-only grant covers
// the scheme-default ports only, a host:port grant matches that port only,
// and a grant never implies subdomains.
func (c *Capabilities) canHTTPOutbound(hostport string) bool {
	if c == nil {
		return false
	}
	hostport = strings.ToLower(hostport)
	for _, g := range c.HTTP.Outbound {
		if strings.ToLower(g) == hostport {
			return true
		}
	}
	return false
}

// canPublishEvent reports whether the plugin may publish to topic. core.*
// topics are reserved for the host and never publishable.
func (c *Capabilities) canPublishEvent(topic string) bool {
	return c != nil && !strings.HasPrefix(topic, "core.") && globAny(c.Events.Publish, topic)
}

// canSubscribeEvent reports whether the plugin may receive topic.
func (c *Capabilities) canSubscribeEvent(topic string) bool {
	return c != nil && globAny(c.Events.Subscribe, topic)
}

func (c *Capabilities) canRegisterJobs() bool {
	return c != nil && c.Jobs.Register
}

// canRegisterRoute reports whether path is covered by the plugin's
// routes.register grants.
func (c *Capabilities) canRegisterRoute(routePath string) bool {
	return c != nil && prefixAny(c.Routes.Register, routePath)
}

// canRegisterOCS reports whether path is covered by the plugin's
// ocs.register grants.
func (c *Capabilities) canRegisterOCS(routePath string) bool {
	return c != nil && prefixAny(c.OCS.Register, routePath)
}

// canProvideProp reports whether the plugin may provide the WebDAV property.
func (c *Capabilities) canProvideProp(name string) bool {
	return c != nil && globAny(c.WebDAV.Props, name)
}

func (c *Capabilities) hasConfigRead() bool {
	return c != nil && len(c.Config.Read) > 0
}

// canConfigRead reports whether the plugin holds any config.read grant
// covering the plugin-local key (pre-namespace).
func (c *Capabilities) canConfigRead(key string) bool {
	return c != nil && globAny(c.Config.Read, key)
}

// canConfigWrite reports whether the plugin holds any config.write grant
// covering the plugin-local key (pre-namespace).
func (c *Capabilities) canConfigWrite(key string) bool {
	return c != nil && globAny(c.Config.Write, key)
}
