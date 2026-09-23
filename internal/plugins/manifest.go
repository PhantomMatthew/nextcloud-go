package plugins

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const abiV1 = "ncgo-abi/1"

var (
	pluginIDRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
	hostPortRe = regexp.MustCompile(`^[^\s:/]+(:\d+)?$`)
)

// Manifest is a parsed plugin.toml.
type Manifest struct {
	Plugin       PluginSection      `toml:"plugin"`
	Runtime      RuntimeSection     `toml:"runtime"`
	Capabilities Capabilities       `toml:"capabilities"`
	EntryPoints  EntryPointsSection `toml:"entry_points"`
}

// Capabilities is the typed [capabilities] section: permissions the plugin
// requests and an admin grants at install. Anything not listed is denied.
// TOML dotted keys (db.read = [...]) decode into the nested structs below.
type Capabilities struct {
	DB      DBCapabilities      `toml:"db"`
	Storage StorageCapabilities `toml:"storage"`
	HTTP    HTTPCapabilities    `toml:"http"`
	Events  EventsCapabilities  `toml:"events"`
	Jobs    JobsCapabilities    `toml:"jobs"`
	Routes  RoutesCapabilities  `toml:"routes"`
	OCS     OCSCapabilities     `toml:"ocs"`
	WebDAV  WebDAVCapabilities  `toml:"webdav"`
	Config  ConfigCapabilities  `toml:"config"`
}

// DBCapabilities grants table access by name glob.
type DBCapabilities struct {
	Read  []string `toml:"read"`
	Write []string `toml:"write"`
}

// StorageCapabilities grants file access; scopes are "user" or "system".
type StorageCapabilities struct {
	Read  []string `toml:"read"`
	Write []string `toml:"write"`
}

// HTTPCapabilities grants outbound HTTP to host[:port] entries.
// OutboundAllowPrivate additionally lifts the dial-time block on loopback,
// private, link-local, and unspecified target IPs (ADR-0057); without it
// those targets are refused even when the hostname allowlist matches.
type HTTPCapabilities struct {
	Outbound             []string `toml:"outbound"`
	OutboundAllowPrivate bool     `toml:"outbound_allow_private"`
}

// EventsCapabilities grants event bus topics by glob.
type EventsCapabilities struct {
	Publish   []string `toml:"publish"`
	Subscribe []string `toml:"subscribe"`
}

// JobsCapabilities grants background job registration.
type JobsCapabilities struct {
	Register bool `toml:"register"`
}

// RoutesCapabilities grants HTTP route prefixes under /apps/.
type RoutesCapabilities struct {
	Register []string `toml:"register"`
}

// OCSCapabilities grants OCS endpoint prefixes under /apps/.
type OCSCapabilities struct {
	Register []string `toml:"register"`
}

// WebDAVCapabilities grants custom WebDAV property names.
type WebDAVCapabilities struct {
	Props []string `toml:"props"`
}

// ConfigCapabilities grants plugin config keys by glob.
type ConfigCapabilities struct {
	Read  []string `toml:"read"`
	Write []string `toml:"write"`
}

// PluginSection is identity metadata.
type PluginSection struct {
	ID          string `toml:"id"`
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	ABI         string `toml:"abi"`
	Description string `toml:"description"`
	Author      string `toml:"author"`
	Homepage    string `toml:"homepage"`
	License     string `toml:"license"`
}

// RuntimeSection is resource and instance policy. RequestBodyStream opts the
// plugin into the spec §7 request map: the body arrives as a stream handle
// (body_handle, pulled via request_body_read) instead of inline body_bytes,
// lifting the 1 MiB inline cap; unset keeps the legacy inline map.
type RuntimeSection struct {
	InstanceModel     string `toml:"instance_model"`
	PoolSize          int    `toml:"pool_size"`
	MemoryLimitMB     int    `toml:"memory_limit_mb"`
	CPUTimeoutMS      int    `toml:"cpu_timeout_ms"`
	FuelPerCall       uint64 `toml:"fuel_per_call"`
	RequestBodyStream bool   `toml:"request_body_stream"`
}

// EntryPointsSection names exported WASM functions.
type EntryPointsSection struct {
	Module      string `toml:"module"`
	OnInstall   string `toml:"on_install"`
	OnUninstall string `toml:"on_uninstall"`
	OnUpgrade   string `toml:"on_upgrade"`
	OnRequest   string `toml:"on_request"`
	OnJob       string `toml:"on_job"`
	OnEvent     string `toml:"on_event"`
}

// ParseManifest decodes plugin.toml from r.
func ParseManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := toml.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrManifestInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks Phase 0 manifest constraints.
func (m *Manifest) Validate() error {
	if m == nil {
		return ErrManifestInvalid
	}
	if !pluginIDRe.MatchString(m.Plugin.ID) {
		return fmt.Errorf("%w: id %q", ErrManifestInvalid, m.Plugin.ID)
	}
	if m.Plugin.ABI != abiV1 {
		return fmt.Errorf("%w: abi %q", ErrManifestInvalid, m.Plugin.ABI)
	}
	if m.Runtime.InstanceModel == "" {
		m.Runtime.InstanceModel = "per_request"
	}
	switch m.Runtime.InstanceModel {
	case "per_request", "pooled", "singleton":
	default:
		return fmt.Errorf("%w: instance_model %q", ErrManifestInvalid, m.Runtime.InstanceModel)
	}
	if m.Runtime.MemoryLimitMB > 256 {
		return fmt.Errorf("%w: memory_limit_mb %d", ErrManifestInvalid, m.Runtime.MemoryLimitMB)
	}
	if m.Runtime.CPUTimeoutMS > 30000 {
		return fmt.Errorf("%w: cpu_timeout_ms %d", ErrManifestInvalid, m.Runtime.CPUTimeoutMS)
	}
	if m.Runtime.InstanceModel == "pooled" && m.Runtime.PoolSize <= 0 {
		return fmt.Errorf("%w: pooled requires pool_size >= 1", ErrManifestInvalid)
	}
	if m.EntryPoints.Module == "" {
		return fmt.Errorf("%w: empty module", ErrManifestInvalid)
	}
	return m.Capabilities.validate()
}

var reservedPropPrefixes = []string{"oc:", "nc:", "core."}

func (c *Capabilities) validate() error {
	if c == nil {
		return nil
	}
	for _, scope := range append(append([]string{}, c.Storage.Read...), c.Storage.Write...) {
		if scope != "user" && scope != "system" {
			return fmt.Errorf("%w: storage scope %q", ErrManifestInvalid, scope)
		}
	}
	for _, host := range c.HTTP.Outbound {
		if !hostPortRe.MatchString(host) {
			return fmt.Errorf("%w: http.outbound %q", ErrManifestInvalid, host)
		}
	}
	for _, topic := range c.Events.Publish {
		if strings.HasPrefix(topic, "core.") {
			return fmt.Errorf("%w: events.publish reserved topic %q", ErrManifestInvalid, topic)
		}
	}
	for _, path := range append(append([]string{}, c.Routes.Register...), c.OCS.Register...) {
		if !strings.HasPrefix(path, "/apps/") {
			return fmt.Errorf("%w: route %q must start with /apps/", ErrManifestInvalid, path)
		}
	}
	for _, prop := range c.WebDAV.Props {
		for _, prefix := range reservedPropPrefixes {
			if strings.HasPrefix(prop, prefix) {
				return fmt.Errorf("%w: webdav prop %q uses reserved prefix", ErrManifestInvalid, prop)
			}
		}
	}
	return nil
}
