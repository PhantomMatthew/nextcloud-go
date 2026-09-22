package main

import (
	"context"
	"fmt"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	s3store "github.com/PhantomMatthew/nextcloud-go/internal/storage/s3"
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

// openStorage opens the configured default storage backend; the caller does
// not close it (backends hold no resources beyond what the process lifetime
// covers). This mirrors app.openStorage; the helper cannot live in
// internal/storage without an import cycle (the s3/localfs backends import
// the storage package), so the small switch is duplicated here. When
// encryption is enabled the backend is wrapped exactly as in the server, so
// CLI writes (import-nextcloud files) are sealed too.
func openStorage(cfg *config.Config) (storage.Storage, error) {
	name := cfg.Storage.DefaultBackend
	if name == "" {
		name = "local"
	}
	b, ok := cfg.Storage.Backends[name]
	if !ok {
		return nil, fmt.Errorf("ncgo-cli: storage backend %q not configured", name)
	}
	var st storage.Storage
	var err error
	switch b.Type {
	case "localfs":
		st, err = localfs.New(b.Root)
	case "s3":
		st, err = s3store.New(b)
	default:
		return nil, fmt.Errorf("ncgo-cli: storage backend %q type %q unsupported", name, b.Type)
	}
	if err != nil {
		return nil, err
	}
	if cfg.Encryption.Enabled {
		key, err := encrypt.LoadMasterKey(cfg.Encryption.MasterKeyPath)
		if err != nil {
			return nil, fmt.Errorf("ncgo-cli: encryption: %w", err)
		}
		st, err = encrypt.New(key, st)
		if err != nil {
			return nil, fmt.Errorf("ncgo-cli: encryption: %w", err)
		}
	}
	return st, nil
}
