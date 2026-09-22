package config

import (
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// defaultPostgresDSN is the documented local-development example DSN, not a
// production credential.
const defaultPostgresDSN = "postgres://ncgo:ncgo@127.0.0.1:5432/ncgo?sslmode=disable" //nolint:gosec // G101: example DSN from Phase 0 blueprint

// Default returns the built-in configuration used when no file or env overlay
// is supplied. Values match docs/plans/01-phase-0-blueprint.md plus the
// Phase 0 fields added in the approved plan (auto_migrate, bootstrap_admin,
// maintenance, instance, redis_password, conn_max_lifetime).
func Default() *Config {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(defaultFlat(), "."), nil); err != nil {
		return hardcodedDefault()
	}
	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return hardcodedDefault()
	}
	return &cfg
}

func hardcodedDefault() *Config {
	return &Config{
		Server: ServerConfig{
			Listen: "0.0.0.0:8080",
		},
		Database: DatabaseConfig{
			Driver:       "postgres",
			DSN:          defaultPostgresDSN,
			MaxOpenConns: 50,
			MaxIdleConns: 10,
			AutoMigrate:  true,
		},
		Cache: CacheConfig{
			L1MaxItems:  100000,
			L1MaxCostMB: 256,
		},
		Storage: StorageConfig{
			DefaultBackend: "local",
			Backends: map[string]BackendConfig{
				"local": {Type: "localfs", Root: "/var/lib/ncgo/data"},
			},
		},
		Previews: PreviewsConfig{Enabled: true, MaxDimension: 2048},
		Auth: AuthConfig{
			Argon2id: Argon2idConfig{MemoryKB: 65536, Iterations: 3, Parallelism: 4},
		},
		Jobs: JobsConfig{Workers: 4, PollInterval: 5 * time.Second},
		Plugin: PluginConfig{
			Enabled:              true,
			InstallDir:           "/var/lib/ncgo/plugins",
			DefaultMemoryLimitMB: 32,
			DefaultCPUTimeoutMS:  5000,
			HTTPRatePerMinute:    120,
			MaxHTTPResponseMB:    32,
		},
		Observability: ObservabilityConfig{
			LogLevel:  "info",
			LogFormat: "json",
		},
	}
}

func defaultFlat() map[string]any {
	return map[string]any{
		"server.listen":                     "0.0.0.0:8080",
		"database.driver":                   "postgres",
		"database.dsn":                      defaultPostgresDSN,
		"database.max_open_conns":           50,
		"database.max_idle_conns":           10,
		"database.conn_max_lifetime":        time.Duration(0),
		"database.auto_migrate":             true,
		"cache.l1_max_items":                int64(100000),
		"cache.l1_max_cost_mb":              256,
		"cache.redis_addr":                  "",
		"cache.redis_db":                    0,
		"cache.redis_password":              "",
		"storage.default_backend":           "local",
		"storage.backends.local.type":       "localfs",
		"storage.backends.local.root":       "/var/lib/ncgo/data",
		"encryption.enabled":                false,
		"encryption.master_key_path":        "",
		"previews.enabled":                  true,
		"previews.max_dimension":            2048,
		"web.static_root":                   "",
		"auth.argon2id.memory_kb":           uint32(65536),
		"auth.argon2id.iterations":          uint32(3),
		"auth.argon2id.parallelism":         uint8(4),
		"auth.bootstrap_admin.uid":          "",
		"auth.bootstrap_admin.password":     "",
		"auth.bootstrap_admin.display_name": "",
		"jobs.workers":                      4,
		"jobs.poll_interval":                5 * time.Second,
		"plugin.enabled":                    true,
		"plugin.install_dir":                "/var/lib/ncgo/plugins",
		"plugin.default_memory_limit_mb":    32,
		"plugin.default_cpu_timeout_ms":     5000,
		"plugin.http_rate_per_minute":       120,
		"plugin.max_http_response_mb":       32,
		"observability.log_level":           "info",
		"observability.log_format":          "json",
		"observability.metrics_enabled":     false,
		"observability.metrics_token":       "",
		"maintenance.enabled":               false,
		"maintenance.needs_db_upgrade":      false,
		"instance.id":                       "",
		"instance.secret":                   "",
		"sharing.lookup_server":             "https://lookup.nextcloud.com",
	}
}
