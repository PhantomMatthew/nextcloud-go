package config

import (
	"errors"
	"os"
	"path/filepath"
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
	if !got.Previews.Enabled || got.Previews.MaxDimension != 2048 {
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
		got.Plugin.SystemStorageQuotaMB != 1024 {
		t.Errorf("plugin defaults: %+v", got.Plugin)
	}
	if got.Observability.LogLevel != "info" || got.Observability.LogFormat != "json" {
		t.Errorf("observability defaults: %+v", got.Observability)
	}
	if got.Observability.MetricsEnabled || got.Observability.MetricsToken != "" {
		t.Errorf("metrics defaults: %+v", got.Observability)
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
	if cfg.Auth.BootstrapAdmin.UID != "admin" {
		t.Errorf("bootstrap uid = %q", cfg.Auth.BootstrapAdmin.UID)
	}
	if cfg.Instance.ID != "ocabcdef0123" || cfg.Instance.Secret != "supersecret" {
		t.Errorf("instance = %+v", cfg.Instance)
	}
	if cfg.Plugin.HTTPRatePerMinute != 240 || cfg.Plugin.MaxHTTPResponseMB != 64 || cfg.Plugin.DBMaxConcurrentPerPlugin != 8 ||
		cfg.Plugin.SystemStorageQuotaMB != 2048 {
		t.Errorf("plugin limits = %+v", cfg.Plugin)
	}
	if cfg.Database.ConnMaxLifetime != time.Hour {
		t.Errorf("conn_max_lifetime = %s", cfg.Database.ConnMaxLifetime)
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
// removed in Phase 4k (server.trusted_proxies, observability.metrics_listen,
// observability.otel_endpoint, ...) may still sit in existing config files
// and must not fail startup — they are silently ignored.
func TestLoadUnknownKeysIgnored(t *testing.T) {
	cfg, err := Load(LoadOptions{
		EnvPrefix: unusedEnvPrefix,
		Overrides: map[string]any{
			"server.trusted_proxies":       []string{"10.0.0.0/8"},
			"auth.session_ttl":             "24h",
			"observability.metrics_listen": "127.0.0.1:9090",
			"observability.otel_endpoint":  "http://otel:4317",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:8080" {
		t.Errorf("listen = %q", cfg.Server.Listen)
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
		{"argon_memory", func(c *Config) { c.Auth.Argon2id.MemoryKB = 0 }, "auth.argon2id.memory_kb"},
		{"argon_iter", func(c *Config) { c.Auth.Argon2id.Iterations = 0 }, "auth.argon2id.iterations"},
		{"argon_par", func(c *Config) { c.Auth.Argon2id.Parallelism = 0 }, "auth.argon2id.parallelism"},
		{"plugin_mem", func(c *Config) { c.Plugin.DefaultMemoryLimitMB = 257 }, "plugin.default_memory_limit_mb"},
		{"plugin_timeout", func(c *Config) { c.Plugin.DefaultCPUTimeoutMS = 30001 }, "plugin.default_cpu_timeout_ms"},
		{"plugin_http_rate_negative", func(c *Config) { c.Plugin.HTTPRatePerMinute = -1 }, "plugin.http_rate_per_minute"},
		{"plugin_http_response_negative", func(c *Config) { c.Plugin.MaxHTTPResponseMB = -1 }, "plugin.max_http_response_mb"},
		{"plugin_db_concurrent_negative", func(c *Config) { c.Plugin.DBMaxConcurrentPerPlugin = -1 }, "plugin.db_max_concurrent_per_plugin"},
		{"plugin_system_quota_negative", func(c *Config) { c.Plugin.SystemStorageQuotaMB = -1 }, "plugin.system_storage_quota_mb"},
		{"storage_backend", func(c *Config) { c.Storage.DefaultBackend = "s3" }, "storage.default_backend"},
		{"encryption_no_key", func(c *Config) { c.Encryption.Enabled = true }, "encryption.master_key_path"},
		{"previews_dim_low", func(c *Config) { c.Previews.MaxDimension = 31 }, "previews.max_dimension"},
		{"previews_dim_high", func(c *Config) { c.Previews.MaxDimension = 4097 }, "previews.max_dimension"},
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
