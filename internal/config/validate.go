package config

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
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

	if c.Storage.DefaultBackend == "" {
		errs = append(errs, &ValidationError{Field: "storage.default_backend", Reason: "must not be empty"})
	} else if _, ok := c.Storage.Backends[c.Storage.DefaultBackend]; !ok {
		errs = append(errs, &ValidationError{Field: "storage.default_backend", Reason: "must exist in storage.backends"})
	}

	if c.Encryption.Enabled && strings.TrimSpace(c.Encryption.MasterKeyPath) == "" {
		errs = append(errs, &ValidationError{Field: "encryption.master_key_path", Reason: "required when encryption is enabled"})
	}

	if c.Previews.MaxDimension < 32 || c.Previews.MaxDimension > 4096 {
		errs = append(errs, &ValidationError{Field: "previews.max_dimension", Reason: "must be between 32 and 4096"})
	}

	if strings.TrimSpace(c.Web.StaticRoot) != "" && !filepath.IsAbs(c.Web.StaticRoot) {
		errs = append(errs, &ValidationError{Field: "web.static_root", Reason: "must be an absolute path"})
	}

	return errors.Join(errs...)
}
