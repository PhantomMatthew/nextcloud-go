package plugins

import (
	"fmt"
	"io"
	"regexp"

	"github.com/pelletier/go-toml/v2"
)

const abiV1 = "ncgo-abi/1"

var pluginIDRe = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)

// Manifest is a parsed plugin.toml.
type Manifest struct {
	Plugin       PluginSection      `toml:"plugin"`
	Runtime      RuntimeSection     `toml:"runtime"`
	Capabilities map[string]any     `toml:"capabilities"`
	EntryPoints  EntryPointsSection `toml:"entry_points"`
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

// RuntimeSection is resource and instance policy.
type RuntimeSection struct {
	InstanceModel string `toml:"instance_model"`
	PoolSize      int    `toml:"pool_size"`
	MemoryLimitMB int    `toml:"memory_limit_mb"`
	CPUTimeoutMS  int    `toml:"cpu_timeout_ms"`
	FuelPerCall   uint64 `toml:"fuel_per_call"`
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
	if m.EntryPoints.Module == "" {
		return fmt.Errorf("%w: empty module", ErrManifestInvalid)
	}
	return nil
}
