package main

import (
	"context"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// loadConfig loads the server configuration, honoring the --config flag.
func loadConfig() (*config.Config, error) {
	return config.Load(config.LoadOptions{Path: cfgPath})
}

// openDB opens the configured database pool; the caller closes it.
func openDB(ctx context.Context, cfg *config.Config) (database.DB, error) {
	return database.Open(ctx, database.Config{
		Driver:          database.Dialect(cfg.Database.Driver),
		DSN:             cfg.Database.DSN,
		MaxOpenConns:    cfg.Database.MaxOpenConns,
		MaxIdleConns:    cfg.Database.MaxIdleConns,
		ConnMaxLifetime: cfg.Database.ConnMaxLifetime,
	})
}
