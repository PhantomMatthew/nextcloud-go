package config

import "time"

// Config is the root server configuration surface.
type Config struct {
	Server        ServerConfig        `koanf:"server"`
	Database      DatabaseConfig      `koanf:"database"`
	Cache         CacheConfig         `koanf:"cache"`
	Storage       StorageConfig       `koanf:"storage"`
	Encryption    EncryptionConfig    `koanf:"encryption"`
	Previews      PreviewsConfig      `koanf:"previews"`
	Web           WebConfig           `koanf:"web"`
	Auth          AuthConfig          `koanf:"auth"`
	Jobs          JobsConfig          `koanf:"jobs"`
	Plugin        PluginConfig        `koanf:"plugin"`
	Observability ObservabilityConfig `koanf:"observability"`
	Maintenance   MaintenanceConfig   `koanf:"maintenance"`
	Instance      InstanceConfig      `koanf:"instance"`
	Sharing       SharingConfig       `koanf:"sharing"`
}

// SharingConfig controls sharing integrations. LookupServer is the base URL
// of a Nextcloud lookup server used for federated user search; empty
// disables lookup queries.
type SharingConfig struct {
	LookupServer string `koanf:"lookup_server"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Listen string `koanf:"listen"`
}

// DatabaseConfig selects the SQL driver and pool parameters.
type DatabaseConfig struct {
	Driver          string        `koanf:"driver"`
	DSN             string        `koanf:"dsn"`
	MaxOpenConns    int           `koanf:"max_open_conns"`
	MaxIdleConns    int           `koanf:"max_idle_conns"`
	ConnMaxLifetime time.Duration `koanf:"conn_max_lifetime"`
	AutoMigrate     bool          `koanf:"auto_migrate"`
}

// CacheConfig configures the in-process L1 cache and optional Redis L2.
type CacheConfig struct {
	L1MaxItems    int64  `koanf:"l1_max_items"`
	L1MaxCostMB   int    `koanf:"l1_max_cost_mb"`
	RedisAddr     string `koanf:"redis_addr"`
	RedisDB       int    `koanf:"redis_db"`
	RedisPassword string `koanf:"redis_password"`
}

// StorageConfig names the default backend and the configured backend map.
type StorageConfig struct {
	DefaultBackend string                   `koanf:"default_backend"`
	Backends       map[string]BackendConfig `koanf:"backends"`
}

// BackendConfig describes one storage backend (localfs or s3).
type BackendConfig struct {
	Type            string `koanf:"type"`
	Root            string `koanf:"root"`
	Endpoint        string `koanf:"endpoint"`
	Bucket          string `koanf:"bucket"`
	AccessKeyID     string `koanf:"access_key_id"`
	SecretAccessKey string `koanf:"secret_access_key"`
	Region          string `koanf:"region"`
}

// EncryptionConfig controls transparent server-side encryption at rest
// (ADR-0052). MasterKeyPath points at an operator-created file holding the
// base64-encoded 32-byte master key (see ncgo-cli encryption init); the key
// is never generated or stored by the server itself. PreviousKeyPaths holds
// retired master keys for rotation (ADR-0074): their positions are their
// key IDs, the key at MasterKeyPath seals new writes at the highest ID.
// The list is append-only forever — reordering or removing entries orphans
// the files sealed under those IDs.
type EncryptionConfig struct {
	Enabled          bool     `koanf:"enabled"`
	MasterKeyPath    string   `koanf:"master_key_path"`
	PreviousKeyPaths []string `koanf:"previous_key_paths"`
}

// PreviewsConfig controls server-side image preview generation (ADR-0053).
// MaxDimension caps the requested preview box edge (x/y are clamped to it).
// PregenerateEnabled (ADR-0084, opt-in) renders PregenerateSizes — square
// box edges for the hot client sizes — ahead of any request, from
// files.uploaded events; it requires Enabled. CacheMaxAge (ADR-0085) is
// the TTL the preview.gc sweep applies to cache entries; 0 selects the
// host default (720h).
type PreviewsConfig struct {
	Enabled            bool          `koanf:"enabled"`
	MaxDimension       int           `koanf:"max_dimension"`
	PregenerateEnabled bool          `koanf:"pregenerate_enabled"`
	PregenerateSizes   []int         `koanf:"pregenerate_sizes"`
	CacheMaxAge        time.Duration `koanf:"cache_max_age"`
}

// WebConfig controls serving the pre-compiled Nextcloud web frontend
// (ADR-0054). StaticRoot is an absolute path to a directory of compiled
// frontend assets (for example a Nextcloud release's web root); empty
// disables static serving entirely.
type WebConfig struct {
	StaticRoot string `koanf:"static_root"`
}

// AuthConfig covers password hashing and bootstrap admin.
type AuthConfig struct {
	Argon2id       Argon2idConfig       `koanf:"argon2id"`
	BootstrapAdmin BootstrapAdminConfig `koanf:"bootstrap_admin"`
}

// Argon2idConfig is the Argon2id parameter set used for password hashing.
type Argon2idConfig struct {
	MemoryKB    uint32 `koanf:"memory_kb"`
	Iterations  uint32 `koanf:"iterations"`
	Parallelism uint8  `koanf:"parallelism"`
}

// BootstrapAdminConfig, when UID is set, seeds the first administrator.
type BootstrapAdminConfig struct {
	UID         string `koanf:"uid"`
	Password    string `koanf:"password"`
	DisplayName string `koanf:"display_name"`
}

// JobsConfig controls the background job runner.
type JobsConfig struct {
	Workers      int           `koanf:"workers"`
	PollInterval time.Duration `koanf:"poll_interval"`
}

// PluginConfig controls the WASM plugin host. HTTPRatePerMinute and
// MaxHTTPResponseMB bound plugin outbound HTTP (ADR-0059);
// DBMaxConcurrentPerPlugin bounds one plugin's in-flight DB statements
// (ADR-0060); SystemStorageQuotaMB caps one plugin's system-scope storage
// tree (ADR-0061); zero selects the host default (120 calls/min with burst
// 30, 32 MiB per response, 4 concurrent statements, 1 GiB per tree).
// RefreshInterval is the plugin hot-reload poll (ADR-0062): registry
// changes (install/enable/disable/upgrade via ncgo-cli) take effect within
// one interval; 0 disables the poll so changes need a restart, like before.
type PluginConfig struct {
	Enabled                  bool          `koanf:"enabled"`
	InstallDir               string        `koanf:"install_dir"`
	DefaultMemoryLimitMB     int           `koanf:"default_memory_limit_mb"`
	DefaultCPUTimeoutMS      int           `koanf:"default_cpu_timeout_ms"`
	HTTPRatePerMinute        int           `koanf:"http_rate_per_minute"`
	MaxHTTPResponseMB        int           `koanf:"max_http_response_mb"`
	DBMaxConcurrentPerPlugin int           `koanf:"db_max_concurrent_per_plugin"`
	SystemStorageQuotaMB     int           `koanf:"system_storage_quota_mb"`
	RefreshInterval          time.Duration `koanf:"refresh_interval"`
}

// ObservabilityConfig covers logging, metrics, and tracing endpoints.
// MetricsEnabled mounts GET /metrics (ADR-0055); MetricsToken, when set,
// requires `Authorization: Bearer <token>` on scrapes (constant-time
// compared). Network-level restriction is recommended regardless.
// MetricsListen, when set, serves /metrics on a dedicated listener at that
// host:port address instead of the main listener (ADR-0076) — move, not
// copy: the separate listener exists for network-level restriction (bind
// localhost or an inner interface while the main listener is public), and
// serving both would defeat it. It requires MetricsEnabled.
// OTelEndpoint enables OpenTelemetry tracing when non-empty (ADR-0072): a
// bare host:port means a plaintext-HTTP OTLP collector, a full http(s)://
// URL keeps its scheme and path. OTelSampleRatio is the parent-based
// head-sampling probability in [0,1]; 1.0 keeps every trace.
type ObservabilityConfig struct {
	LogLevel        string  `koanf:"log_level"`
	LogFormat       string  `koanf:"log_format"`
	MetricsEnabled  bool    `koanf:"metrics_enabled"`
	MetricsToken    string  `koanf:"metrics_token"`
	MetricsListen   string  `koanf:"metrics_listen"`
	OTelEndpoint    string  `koanf:"otel_endpoint"`
	OTelSampleRatio float64 `koanf:"otel_sample_ratio"`
}

// MaintenanceConfig mirrors Nextcloud's maintenance and upgrade flags.
type MaintenanceConfig struct {
	Enabled        bool `koanf:"enabled"`
	NeedsDBUpgrade bool `koanf:"needs_db_upgrade"`
}

// InstanceConfig holds per-install identity material.
type InstanceConfig struct {
	ID     string `koanf:"id"`
	Secret string `koanf:"secret"`
}

// LoadOptions controls how Load overlays defaults, file, env, and flags.
type LoadOptions struct {
	Path      string
	EnvPrefix string
	Overrides map[string]any
}
