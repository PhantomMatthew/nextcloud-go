package app

import (
	"os"
	"path/filepath"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
)

// DevConfig returns an in-memory SQLite config with bootstrap admin/admin.
func DevConfig() *config.Config {
	cfg := config.Default()
	cfg.Server.Listen = "127.0.0.1:8080"
	cfg.Database.Driver = "sqlite"
	cfg.Database.DSN = "file:ncgo-dev?mode=memory&cache=shared"
	cfg.Database.AutoMigrate = true
	cfg.Auth.BootstrapAdmin = config.BootstrapAdminConfig{
		UID:         "admin",
		Password:    "admin",
		DisplayName: "admin",
	}
	cfg.Auth.Argon2id = config.Argon2idConfig{MemoryKB: 8, Iterations: 1, Parallelism: 1}
	cfg.Observability.LogLevel = "debug"
	cfg.Observability.LogFormat = "text"
	cfg.Plugin.Enabled = false
	cfg.Cache.RedisAddr = ""
	cfg.Instance.Secret = "dev-secret-not-for-production"
	cfg.Instance.ID = "ocdev00001"
	cfg.Storage.DefaultBackend = "local"
	cfg.Storage.Backends = map[string]config.BackendConfig{
		"local": {Type: "localfs", Root: filepath.Join(os.TempDir(), "ncgo-dev")},
	}
	return cfg
}
