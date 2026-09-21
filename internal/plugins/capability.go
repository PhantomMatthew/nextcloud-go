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

func (c *Capabilities) hasStorageWrite() bool {
	return c != nil && len(c.Storage.Write) > 0
}

func (c *Capabilities) hasHTTPOutbound() bool {
	return c != nil && len(c.HTTP.Outbound) > 0
}

// canPublishEvent reports whether the plugin may publish to topic. core.*
// topics are reserved for the host and never publishable.
func (c *Capabilities) canPublishEvent(topic string) bool {
	return c != nil && !strings.HasPrefix(topic, "core.") && globAny(c.Events.Publish, topic)
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

func (c *Capabilities) hasConfigWrite() bool {
	return c != nil && len(c.Config.Write) > 0
}
