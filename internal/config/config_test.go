package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestDefaultSnapshot(t *testing.T) {
	t.Parallel()
	got := Default()
	if got.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("listen = %q", got.Server.Listen)
	}
	if got.Database.Driver != "postgres" || got.Database.MaxOpenConns != 50 || !got.Database.AutoMigrate {
		t.Errorf("database defaults: %+v", got.Database)
	}
	if got.Cache.L1MaxItems != 100000 || got.Cache.L1MaxCostMB != 256 {
		t.Errorf("cache defaults: %+v", got.Cache)
	}
	if got.Storage.DefaultBackend != "local" {
		t.Errorf("storage.default_backend = %q", got.Storage.DefaultBackend)
	}
	if !got.Previews.Enabled || got.Previews.MaxDimension != 2048 ||
		got.Previews.PregenerateEnabled || !slices.Equal(got.Previews.PregenerateSizes, []int{32, 256}) ||
		got.Previews.CacheMaxAge != 720*time.Hour {
		t.Errorf("previews defaults: %+v", got.Previews)
	}
	if got.Web.StaticRoot != "" {
		t.Errorf("web defaults: %+v", got.Web)
	}
	local, ok := got.Storage.Backends["local"]
	if !ok || local.Type != "localfs" || local.Root != "/var/lib/ncgo/data" {
		t.Errorf("local backend = %+v ok=%v", local, ok)
	}
	if got.Auth.Argon2id != (Argon2idConfig{MemoryKB: 65536, Iterations: 3, Parallelism: 4}) {
		t.Errorf("argon2id = %+v", got.Auth.Argon2id)
	}
	if got.Jobs.Workers != 4 || got.Jobs.PollInterval != 5*time.Second {
		t.Errorf("jobs defaults: %+v", got.Jobs)
	}
	if !got.Plugin.Enabled || got.Plugin.DefaultMemoryLimitMB != 32 || got.Plugin.DefaultCPUTimeoutMS != 5000 ||
		got.Plugin.HTTPRatePerMinute != 120 || got.Plugin.MaxHTTPResponseMB != 32 || got.Plugin.DBMaxConcurrentPerPlugin != 4 ||
		got.Plugin.SystemStorageQuotaMB != 1024 || got.Plugin.RefreshInterval != 10*time.Second {
		t.Errorf("plugin defaults: %+v", got.Plugin)
	}
	if got.Observability.LogLevel != "info" || got.Observability.LogFormat != "json" {
		t.Errorf("observability defaults: %+v", got.Observability)
	}
	if got.Observability.MetricsEnabled || got.Observability.MetricsToken != "" {
		t.Errorf("metrics defaults: %+v", got.Observability)
	}
	if got.Observability.OTelEndpoint != "" || got.Observability.OTelSampleRatio != 1.0 {
		t.Errorf("otel defaults: %+v", got.Observability)
	}
	if got.Maintenance.Enabled || got.Instance.Secret != "" {
		t.Errorf("maintenance/instance defaults: %+v %+v", got.Maintenance, got.Instance)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v", err)
	}
}

func TestLoadFileOverridesDefaults(t *testing.T) {
	cfg, err := Load(LoadOptions{Path: testdata(t, "minimal.yaml"), EnvPrefix: unusedEnvPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "sqlite" || cfg.Database.DSN != "file:ncgo.db" {
		t.Errorf("database = %+v", cfg.Database)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("listen should remain default, got %q", cfg.Server.Listen)
	}
}

func TestLoadFullFile(t *testing.T) {
	cfg, err := Load(LoadOptions{Path: testdata(t, "full.yaml"), EnvPrefix: unusedEnvPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9000" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Cache.RedisAddr != "redis:6379" || cfg.Cache.RedisPassword != "s3cret" {
		t.Errorf("cache redis = %+v", cfg.Cache)
	}
	if cfg.Storage.Backends["s3_primary"].Type != "s3" {
		t.Errorf("s3 backend = %+v", cfg.Storage.Backends["s3_primary"])
	}
	if !cfg.Encryption.Enabled || cfg.Encryption.MasterKeyPath != "/var/lib/ncgo/master.key" {
		t.Errorf("encryption = %+v", cfg.Encryption)
	}
	if len(cfg.Encryption.PreviousKeyPaths) != 2 ||
		cfg.Encryption.PreviousKeyPaths[0] != "/var/lib/ncgo/master-2024.key" ||
		cfg.Encryption.PreviousKeyPaths[1] != "/var/lib/ncgo/master-2025.key" {
		t.Errorf("previous key paths = %v", cfg.Encryption.PreviousKeyPaths)
	}
	if cfg.Encryption.PerUserKeys {
		t.Error("full.yaml per_user_keys = true, want false")
	}
	if cfg.Auth.BootstrapAdmin.UID != "admin" {
		t.Errorf("bootstrap uid = %q", cfg.Auth.BootstrapAdmin.UID)
	}
	if cfg.Instance.ID != "ocabcdef0123" || cfg.Instance.Secret != "supersecret" {
		t.Errorf("instance = %+v", cfg.Instance)
	}
	if cfg.Plugin.HTTPRatePerMinute != 240 || cfg.Plugin.MaxHTTPResponseMB != 64 || cfg.Plugin.DBMaxConcurrentPerPlugin != 8 ||
		cfg.Plugin.SystemStorageQuotaMB != 2048 || cfg.Plugin.RefreshInterval != 30*time.Second {
		t.Errorf("plugin limits = %+v", cfg.Plugin)
	}
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Errorf("conn_max_lifetime = %s", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Observability.OTelEndpoint != "http://otel-collector:4318" || cfg.Observability.OTelSampleRatio != 0.5 {
		t.Errorf("otel = %+v", cfg.Observability)
	}
	if !cfg.Observability.MetricsEnabled || cfg.Observability.MetricsListen != "127.0.0.1:9090" {
		t.Errorf("metrics listener = %+v", cfg.Observability)
	}
}

func TestLoadPrecedenceFileEnvOverrides(t *testing.T) {
	path := testdata(t, "full.yaml")

	t.Run("file", func(t *testing.T) {
		cfg, err := Load(LoadOptions{Path: path, EnvPrefix: unusedEnvPrefix})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Listen != "127.0.0.1:9000" {
			t.Errorf("got %q", cfg.Server.Listen)
		}
	})

	t.Run("env over file", func(t *testing.T) {
		t.Setenv("NCGO_SERVER__LISTEN", "env.example:1111")
		cfg, err := Load(LoadOptions{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Listen != "env.example:1111" {
			t.Errorf("got %q", cfg.Server.Listen)
		}
	})

	t.Run("overrides over env", func(t *testing.T) {
		t.Setenv("NCGO_SERVER__LISTEN", "env.example:1111")
		cfg, err := Load(LoadOptions{
			Path: path,
			Overrides: map[string]any{
				"server.listen": "override.example:2222",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Listen != "override.example:2222" {
			t.Errorf("got %q", cfg.Server.Listen)
		}
	})
}

func TestLoadDoubleUnderscoreEnvMapping(t *testing.T) {
	t.Setenv("NCGO_DATABASE__DSN", "postgres://env/ncgo")
	t.Setenv("NCGO_DATABASE__DRIVER", "postgres")
	t.Setenv("NCGO_CACHE__L1_MAX_ITEMS", "42")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.DSN != "postgres://env/ncgo" {
		t.Errorf("dsn = %q", cfg.Database.DSN)
	}
	if cfg.Cache.L1MaxItems != 42 {
		t.Errorf("l1_max_items = %d", cfg.Cache.L1MaxItems)
	}
}

func TestLoadAliases(t *testing.T) {
	t.Setenv("NCGO_SECRET", "alias-secret")
	t.Setenv("NCGO_INSTANCE_ID", "ocalias")
	t.Setenv("NCGO_MAINTENANCE", "1")
	cfg, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instance.Secret != "alias-secret" {
		t.Errorf("secret = %q", cfg.Instance.Secret)
	}
	if cfg.Instance.ID != "ocalias" {
		t.Errorf("id = %q", cfg.Instance.ID)
	}
	if !cfg.Maintenance.Enabled {
		t.Error("maintenance.enabled = false, want true")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(LoadOptions{Path: filepath.Join(t.TempDir(), "nope.yaml"), EnvPrefix: unusedEnvPrefix})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadUnknownKeysIgnored pins the lenient unmarshal contract: keys
// removed in Phase 4k (server.trusted_proxies, auth.session_ttl, ...) may
// still sit in existing config files and must not fail startup — they are
// silently ignored. observability.otel_endpoint left this list in Phase 4z,
// when the OTel SDK landed and the key went live again (ADR-0072);
// observability.metrics_listen left it in Phase 5d, when the dedicated
// metrics listener landed (ADR-0076).
func TestLoadUnknownKeysIgnored(t *testing.T) {
	cfg, err := Load(LoadOptions{
		EnvPrefix: unusedEnvPrefix,
		Overrides: map[string]any{
			"server.trusted_proxies": []string{"10.0.0.0/8"},
			"auth.session_ttl":       "24h",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
}

// TestLoadMetricsListen pins that the restored key parses into the struct
// (it is live again — ADR-0076 — not silently ignored).
func TestLoadMetricsListen(t *testing.T) {
	cfg, err := Load(LoadOptions{
		EnvPrefix: unusedEnvPrefix,
		Overrides: map[string]any{
			"observability.metrics_enabled": true,
			"observability.metrics_listen":  "127.0.0.1:9090",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Observability.MetricsEnabled || cfg.Observability.MetricsListen != "127.0.0.1:9090" {
		t.Errorf("observability = %+v", cfg.Observability)
	}
}

func TestLoadEncryptionPreviousKeyPaths(t *testing.T) {
	// Default: empty list, valid.
	cfg, err := Load(LoadOptions{EnvPrefix: unusedEnvPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Encryption.PreviousKeyPaths) != 0 {
		t.Errorf("default previous key paths = %v", cfg.Encryption.PreviousKeyPaths)
	}

	// Overrides carry a native string list.
	cfg, err = Load(LoadOptions{
		EnvPrefix: unusedEnvPrefix,
		Overrides: map[string]any{
			"encryption.previous_key_paths": []string{"/keys/a.key", "/keys/b.key"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Encryption.PreviousKeyPaths) != 2 ||
		cfg.Encryption.PreviousKeyPaths[0] != "/keys/a.key" ||
		cfg.Encryption.PreviousKeyPaths[1] != "/keys/b.key" {
		t.Errorf("previous key paths = %v", cfg.Encryption.PreviousKeyPaths)
	}

	// A valid keyring passes validation.
	c := Default()
	c.Encryption.MasterKeyPath = "/keys/current.key"
	c.Encryption.PreviousKeyPaths = []string{"/keys/a.key", "/keys/b.key"}
	if err := c.Validate(); err != nil {
		t.Errorf("valid keyring Validate() = %v", err)
	}
}

func TestLoadEncryptionPerUserKeys(t *testing.T) {
	// Default: off.
	cfg, err := Load(LoadOptions{EnvPrefix: unusedEnvPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.PerUserKeys {
		t.Error("default per_user_keys = true, want false")
	}

	// Overrides parse a native bool (encryption must be enabled for a true
	// value to validate).
	cfg, err = Load(LoadOptions{
		EnvPrefix: unusedEnvPrefix,
		Overrides: map[string]any{
			"encryption.enabled":         true,
			"encryption.master_key_path": "/keys/current.key",
			"encryption.per_user_keys":   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Encryption.PerUserKeys {
		t.Error("override per_user_keys = false, want true")
	}

	// Valid alongside an enabled encryption section.
	c := Default()
	c.Encryption.Enabled = true
	c.Encryption.MasterKeyPath = "/keys/current.key"
	c.Encryption.PerUserKeys = true
	if err := c.Validate(); err != nil {
		t.Errorf("per-user keys with encryption enabled Validate() = %v", err)
	}
}

func TestLoadEmptySecretIsValid(t *testing.T) {
	cfg, err := Load(LoadOptions{EnvPrefix: unusedEnvPrefix})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instance.Secret != "" {
		t.Errorf("secret = %q", cfg.Instance.Secret)
	}
}

func TestValidateRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mut   func(*Config)
		field string
	}{
		{"listen", func(c *Config) { c.Server.Listen = "not-a-addr" }, "server.listen"},
		{"driver", func(c *Config) { c.Database.Driver = "oracle" }, "database.driver"},
		{"dsn", func(c *Config) { c.Database.DSN = "  " }, "database.dsn"},
		{"log_level", func(c *Config) { c.Observability.LogLevel = "fatal" }, "observability.log_level"},
		{"log_format", func(c *Config) { c.Observability.LogFormat = "pretty" }, "observability.log_format"},
		{"otel_ratio_low", func(c *Config) { c.Observability.OTelSampleRatio = -0.1 }, "observability.otel_sample_ratio"},
		{"otel_ratio_high", func(c *Config) { c.Observability.OTelSampleRatio = 1.1 }, "observability.otel_sample_ratio"},
		{"metrics_listen_garbage", func(c *Config) {
			c.Observability.MetricsEnabled = true
			c.Observability.MetricsListen = "not-an-addr"
		}, "observability.metrics_listen"},
		{"metrics_listen_no_port", func(c *Config) {
			c.Observability.MetricsEnabled = true
			c.Observability.MetricsListen = "127.0.0.1"
		}, "observability.metrics_listen"},
		{"metrics_listen_without_enabled", func(c *Config) {
			c.Observability.MetricsListen = "127.0.0.1:9090"
		}, "observability.metrics_listen"},
		{"argon_memory", func(c *Config) { c.Auth.Argon2id.MemoryKB = 0 }, "auth.argon2id.memory_kb"},
		{"argon_iter", func(c *Config) { c.Auth.Argon2id.Iterations = 0 }, "auth.argon2id.iterations"},
		{"argon_par", func(c *Config) { c.Auth.Argon2id.Parallelism = 0 }, "auth.argon2id.parallelism"},
		{"plugin_mem", func(c *Config) { c.Plugin.DefaultMemoryLimitMB = 257 }, "plugin.default_memory_limit_mb"},
		{"plugin_timeout", func(c *Config) { c.Plugin.DefaultCPUTimeoutMS = 30001 }, "plugin.default_cpu_timeout_ms"},
		{"plugin_http_rate_negative", func(c *Config) { c.Plugin.HTTPRatePerMinute = -1 }, "plugin.http_rate_per_minute"},
		{"plugin_http_response_negative", func(c *Config) { c.Plugin.MaxHTTPResponseMB = -1 }, "plugin.max_http_response_mb"},
		{"plugin_db_concurrent_negative", func(c *Config) { c.Plugin.DBMaxConcurrentPerPlugin = -1 }, "plugin.db_max_concurrent_per_plugin"},
		{"plugin_system_quota_negative", func(c *Config) { c.Plugin.SystemStorageQuotaMB = -1 }, "plugin.system_storage_quota_mb"},
		{"plugin_refresh_interval_negative", func(c *Config) { c.Plugin.RefreshInterval = -time.Second }, "plugin.refresh_interval"},
		{"storage_backend", func(c *Config) { c.Storage.DefaultBackend = "s3" }, "storage.default_backend"},
		{"encryption_no_key", func(c *Config) { c.Encryption.Enabled = true }, "encryption.master_key_path"},
		{"encryption_per_user_keys_without_enabled", func(c *Config) {
			c.Encryption.PerUserKeys = true
		}, "encryption.per_user_keys"},
		{"encryption_previous_empty", func(c *Config) {
			c.Encryption.PreviousKeyPaths = []string{"/var/lib/ncgo/old.key", "  "}
		}, "encryption.previous_key_paths"},
		{"encryption_previous_duplicate", func(c *Config) {
			c.Encryption.PreviousKeyPaths = []string{"/var/lib/ncgo/old.key", "/var/lib/ncgo/old.key"}
		}, "encryption.previous_key_paths"},
		{"encryption_previous_contains_master", func(c *Config) {
			c.Encryption.MasterKeyPath = "/var/lib/ncgo/master.key"
			c.Encryption.PreviousKeyPaths = []string{"/var/lib/ncgo/old.key", "/var/lib/ncgo/master.key"}
		}, "encryption.previous_key_paths"},
		{"encryption_previous_too_many", func(c *Config) {
			paths := make([]string, 256)
			for i := range paths {
				paths[i] = fmt.Sprintf("/var/lib/ncgo/key-%03d.key", i)
			}
			c.Encryption.PreviousKeyPaths = paths
		}, "encryption.previous_key_paths"},
		{"previews_dim_low", func(c *Config) { c.Previews.MaxDimension = 31 }, "previews.max_dimension"},
		{"previews_dim_high", func(c *Config) { c.Previews.MaxDimension = 4097 }, "previews.max_dimension"},
		{"previews_pregenerate_size_zero", func(c *Config) { c.Previews.PregenerateSizes = []int{32, 0} }, "previews.pregenerate_sizes"},
		{"previews_pregenerate_size_high", func(c *Config) { c.Previews.PregenerateSizes = []int{4097} }, "previews.pregenerate_sizes"},
		{"previews_pregenerate_without_enabled", func(c *Config) {
			c.Previews.Enabled = false
			c.Previews.PregenerateEnabled = true
		}, "previews.pregenerate_enabled"},
		{"previews_pregenerate_empty_sizes", func(c *Config) {
			c.Previews.PregenerateEnabled = true
			c.Previews.PregenerateSizes = nil
		}, "previews.pregenerate_sizes"},
		{"previews_cache_max_age_low", func(c *Config) { c.Previews.CacheMaxAge = 30 * time.Minute }, "previews.cache_max_age"},
		{"web_static_root_relative", func(c *Config) { c.Web.StaticRoot = "relative/web" }, "web.static_root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := Default()
			tt.mut(c)
			err := c.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			var found bool
			for _, e := range unwrapJoin(err) {
				var ve *ValidationError
				if errors.As(e, &ve) && ve.Field == tt.field {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("error %v does not contain field %q", err, tt.field)
			}
		})
	}
}

func TestValidateJoinMultiple(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Database.Driver = "x"
	c.Database.DSN = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	n := 0
	for range unwrapJoin(err) {
		n++
	}
	if n < 2 {
		t.Errorf("joined errors = %d, want >= 2; err=%v", n, err)
	}
}

func TestValidateWebStaticRootAbsoluteOK(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Web.StaticRoot = t.TempDir()
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with absolute web.static_root = %v", err)
	}
}

func TestValidateMetricsListenOK(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:9090", ":9090"} {
		c := Default()
		c.Observability.MetricsEnabled = true
		c.Observability.MetricsListen = addr
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() with metrics_listen %q = %v", addr, err)
		}
	}
}

func TestValidatePregenerateOK(t *testing.T) {
	t.Parallel()
	c := Default()
	c.Previews.PregenerateEnabled = true
	c.Previews.PregenerateSizes = []int{64, 1024}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with valid pregeneration = %v", err)
	}
}

func TestValidateCacheMaxAgeOK(t *testing.T) {
	t.Parallel()
	for _, d := range []time.Duration{0, time.Hour, 24 * time.Hour} {
		c := Default()
		c.Previews.CacheMaxAge = d
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() with cache_max_age %s = %v", d, err)
		}
	}
}

const unusedEnvPrefix = "NCGOCFGTESTUNUSED_"

func testdata(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func unwrapJoin(err error) []error {
	var u interface{ Unwrap() []error }
	if errors.As(err, &u) {
		return u.Unwrap()
	}
	return []error{err}
}
