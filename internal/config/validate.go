package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// ValidationError describes a single invalid configuration field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config: %s: %s", e.Field, e.Reason)
}

var (
	validDrivers   = map[string]struct{}{"postgres": {}, "mysql": {}, "sqlite": {}}
	validLogLevels = map[string]struct{}{"debug": {}, "info": {}, "warn": {}, "error": {}}
	validLogFmts   = map[string]struct{}{"json": {}, "text": {}}
)

// Validate reports every constraint violation via errors.Join.
func (c *Config) Validate() error {
	if c == nil {
		return &ValidationError{Field: "", Reason: "config is nil"}
	}
	var errs []error

	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		errs = append(errs, &ValidationError{Field: "server.listen", Reason: "must be host:port"})
	}

	if _, ok := validDrivers[c.Database.Driver]; !ok {
		errs = append(errs, &ValidationError{Field: "database.driver", Reason: "must be postgres, mysql, or sqlite"})
	}
	if strings.TrimSpace(c.Database.DSN) == "" {
		errs = append(errs, &ValidationError{Field: "database.dsn", Reason: "must not be empty"})
	}

	if _, ok := validLogLevels[c.Observability.LogLevel]; !ok {
		errs = append(errs, &ValidationError{Field: "observability.log_level", Reason: "must be debug, info, warn, or error"})
	}
	if _, ok := validLogFmts[c.Observability.LogFormat]; !ok {
		errs = append(errs, &ValidationError{Field: "observability.log_format", Reason: "must be json or text"})
	}
	if c.Observability.OTelSampleRatio < 0 || c.Observability.OTelSampleRatio > 1 {
		errs = append(errs, &ValidationError{Field: "observability.otel_sample_ratio", Reason: "must be between 0 and 1"})
	}
	if c.Observability.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(c.Observability.MetricsListen); err != nil {
			errs = append(errs, &ValidationError{Field: "observability.metrics_listen", Reason: "must be host:port (an empty host, e.g. :9090, means all interfaces)"})
		}
		// A dedicated listener without metrics would serve nothing: the
		// same defined-but-dead key Phase 4k eliminated, so reject it.
		if !c.Observability.MetricsEnabled {
			errs = append(errs, &ValidationError{Field: "observability.metrics_listen", Reason: "requires observability.metrics_enabled (a listener without metrics would be a dead key)"})
		}
	}

	if c.Auth.Argon2id.MemoryKB == 0 {
		errs = append(errs, &ValidationError{Field: "auth.argon2id.memory_kb", Reason: "must be > 0"})
	}
	if c.Auth.Argon2id.Iterations == 0 {
		errs = append(errs, &ValidationError{Field: "auth.argon2id.iterations", Reason: "must be > 0"})
	}
	if c.Auth.Argon2id.Parallelism == 0 {
		errs = append(errs, &ValidationError{Field: "auth.argon2id.parallelism", Reason: "must be > 0"})
	}

	if c.Plugin.DefaultMemoryLimitMB > 256 {
		errs = append(errs, &ValidationError{Field: "plugin.default_memory_limit_mb", Reason: "must be <= 256"})
	}
	if c.Plugin.DefaultCPUTimeoutMS > 30000 {
		errs = append(errs, &ValidationError{Field: "plugin.default_cpu_timeout_ms", Reason: "must be <= 30000"})
	}
	if c.Plugin.HTTPRatePerMinute < 0 {
		errs = append(errs, &ValidationError{Field: "plugin.http_rate_per_minute", Reason: "must be >= 0"})
	}
	if c.Plugin.MaxHTTPResponseMB < 0 {
		errs = append(errs, &ValidationError{Field: "plugin.max_http_response_mb", Reason: "must be >= 0"})
	}
	if c.Plugin.DBMaxConcurrentPerPlugin < 0 {
		errs = append(errs, &ValidationError{Field: "plugin.db_max_concurrent_per_plugin", Reason: "must be >= 0"})
	}
	if c.Plugin.SystemStorageQuotaMB < 0 {
		errs = append(errs, &ValidationError{Field: "plugin.system_storage_quota_mb", Reason: "must be >= 0"})
	}
	if c.Plugin.RefreshInterval < 0 {
		errs = append(errs, &ValidationError{Field: "plugin.refresh_interval", Reason: "must be >= 0"})
	}

	if c.Storage.DefaultBackend == "" {
		errs = append(errs, &ValidationError{Field: "storage.default_backend", Reason: "must not be empty"})
	} else if _, ok := c.Storage.Backends[c.Storage.DefaultBackend]; !ok {
		errs = append(errs, &ValidationError{Field: "storage.default_backend", Reason: "must exist in storage.backends"})
	}

	if c.Encryption.Enabled && strings.TrimSpace(c.Encryption.MasterKeyPath) == "" {
		errs = append(errs, &ValidationError{Field: "encryption.master_key_path", Reason: "required when encryption is enabled"})
	}
	if c.Encryption.PerUserKeys && !c.Encryption.Enabled {
		// Same dead-key rule as metrics_listen (ADR-0076): per-user keys
		// without encryption would be a defined-but-dead key.
		errs = append(errs, &ValidationError{Field: "encryption.per_user_keys", Reason: "requires encryption.enabled (per-user keys without encryption would be a dead key)"})
	}
	if c.Encryption.PasswordWrappedKeys && !c.Encryption.PerUserKeys {
		// ADR-0100: password-wrapped keys are an enrollment mode of the
		// per-user key hierarchy, not a standalone mode.
		errs = append(errs, &ValidationError{Field: "encryption.password_wrapped_keys", Reason: "requires encryption.per_user_keys (password-wrapped keys without per-user keys would be a dead key)"})
	}
	if c.Encryption.FilenameEncryption && !c.Encryption.PerUserKeys {
		// ADR-0104: filename encryption wraps names under per-user directory
		// keys — without per-user keys there is no hierarchy to key them.
		errs = append(errs, &ValidationError{Field: "encryption.filename_encryption", Reason: "requires encryption.per_user_keys (filename encryption wraps names under per-user directory keys)"})
	}
	errs = append(errs, validatePreviousKeyPaths(c.Encryption)...)

	if c.Previews.MaxDimension < 32 || c.Previews.MaxDimension > 4096 {
		errs = append(errs, &ValidationError{Field: "previews.max_dimension", Reason: "must be between 32 and 4096"})
	}
	if c.Previews.PregenerateEnabled {
		// Same dead-key rule as metrics_listen (ADR-0076): pregeneration
		// consumes files.uploaded only when the generator exists, and the
		// generator exists only when previews are enabled.
		if !c.Previews.Enabled {
			errs = append(errs, &ValidationError{Field: "previews.pregenerate_enabled", Reason: "requires previews.enabled (pregeneration without previews would be a dead key)"})
		}
		if len(c.Previews.PregenerateSizes) == 0 {
			errs = append(errs, &ValidationError{Field: "previews.pregenerate_sizes", Reason: "must not be empty when previews.pregenerate_enabled is set"})
		}
	}
	for i, s := range c.Previews.PregenerateSizes {
		if s < 1 || s > 4096 {
			errs = append(errs, &ValidationError{Field: "previews.pregenerate_sizes", Reason: fmt.Sprintf("entry %d (%d) must be between 1 and 4096", i, s)})
		}
	}
	if c.Previews.CacheMaxAge != 0 && c.Previews.CacheMaxAge < time.Hour {
		errs = append(errs, &ValidationError{Field: "previews.cache_max_age", Reason: "must be at least 1h (0 selects the default)"})
	}
	if c.Previews.OfficeEnabled {
		// Same dead-key rule as pregenerate_enabled: the rasterizer exists
		// only on a generator, and the generator exists only with previews.
		if !c.Previews.Enabled {
			errs = append(errs, &ValidationError{Field: "previews.office_enabled", Reason: "requires previews.enabled (office rasterization without previews would be a dead key)"})
		}
		if !filepath.IsAbs(strings.TrimSpace(c.Previews.OfficeCommand)) {
			errs = append(errs, &ValidationError{Field: "previews.office_command", Reason: "must be an absolute path to the headless office converter (e.g. /usr/bin/soffice) when previews.office_enabled is set"})
		}
		if c.Previews.OfficeTimeout < time.Second || c.Previews.OfficeTimeout > 5*time.Minute {
			errs = append(errs, &ValidationError{Field: "previews.office_timeout", Reason: "must be between 1s and 5m"})
		}
	}

	if strings.TrimSpace(c.Web.StaticRoot) != "" && !filepath.IsAbs(c.Web.StaticRoot) {
		errs = append(errs, &ValidationError{Field: "web.static_root", Reason: "must be an absolute path"})
	}

	if c.Office.Enabled {
		u, err := url.Parse(strings.TrimSpace(c.Office.CollaboraURL))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, &ValidationError{Field: "office.collabora_url", Reason: "must be an absolute http(s) URL when office.enabled is set"})
		}
		if c.Office.TokenTTL < time.Minute || c.Office.TokenTTL > 24*time.Hour {
			errs = append(errs, &ValidationError{Field: "office.token_ttl", Reason: "must be between 1m and 24h"})
		}
	}

	if len(c.Mail.EgressAllowPrivate) > 0 && !c.Mail.Enabled {
		// Same dead-key rule as metrics_listen (ADR-0076): an egress
		// allowlist without the mail service would never be consulted.
		errs = append(errs, &ValidationError{Field: "mail.egress_allow_private", Reason: "requires mail.enabled (an egress allowlist without mail would be a dead key)"})
	}
	if _, err := c.Mail.EgressPrefixes(); err != nil {
		errs = append(errs, &ValidationError{Field: "mail.egress_allow_private", Reason: err.Error()})
	}

	return errors.Join(errs...)
}

// validatePreviousKeyPaths enforces the keyring invariants of ADR-0074:
// entries are non-blank and unique, the current master key is not also a
// previous key, and previous keys plus the current one fit the v2 key-ID
// byte (encrypt.MaxKeys = 256). Reordered or ambiguous rings would silently
// re-key files to the wrong ID, so these are startup-fatal.
func validatePreviousKeyPaths(enc EncryptionConfig) []error {
	seen := make(map[string]struct{}, len(enc.PreviousKeyPaths))
	var errs []error
	for i, p := range enc.PreviousKeyPaths {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, &ValidationError{Field: "encryption.previous_key_paths", Reason: fmt.Sprintf("entry %d must not be empty", i)})
			continue
		}
		if _, dup := seen[p]; dup {
			errs = append(errs, &ValidationError{Field: "encryption.previous_key_paths", Reason: fmt.Sprintf("duplicate entry %q", p)})
			continue
		}
		seen[p] = struct{}{}
		if p == enc.MasterKeyPath {
			errs = append(errs, &ValidationError{Field: "encryption.previous_key_paths", Reason: "must not contain encryption.master_key_path"})
		}
	}
	if len(enc.PreviousKeyPaths)+1 > 256 {
		errs = append(errs, &ValidationError{Field: "encryption.previous_key_paths", Reason: "previous keys plus the master key must be at most 256"})
	}
	return errs
}
