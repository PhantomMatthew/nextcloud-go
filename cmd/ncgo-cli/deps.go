package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/PhantomMatthew/nextcloud-go/internal/config"
	"github.com/PhantomMatthew/nextcloud-go/internal/database"
	"github.com/PhantomMatthew/nextcloud-go/internal/files"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/storage/localfs"
	s3store "github.com/PhantomMatthew/nextcloud-go/internal/storage/s3"
	"github.com/PhantomMatthew/nextcloud-go/internal/users"
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

// cliInstanceID mirrors app.New's instance-id fallback: the configured
// instance.id, else an ephemeral "oc"+random-hex id. The id namespaces
// per-instance system storage (appdata_<id>/...), so a configured id is
// required for the CLI to address the same tree the server uses.
func cliInstanceID(cfg *config.Config) string {
	if cfg.Instance.ID != "" {
		return cfg.Instance.ID
	}
	buf := make([]byte, 5)
	if _, err := rand.Read(buf); err != nil {
		return "oc" + hex.EncodeToString(make([]byte, 5))
	}
	return "oc" + hex.EncodeToString(buf)
}

// filesDAV wires the files DAV the same way production does in app.New
// (storage, filecache, props, locks, trash, versions) so CLI writes (file
// imports, plugin install hooks) pass through the same invariants;
// shares/incoming/remote/events are irrelevant to writes and stay nil (the
// DAV nil-guards them).
func filesDAV(st storage.Storage, db database.DB) *files.DAV {
	us := users.NewSQLStore(db)
	dav := files.NewDAV(st, files.NewSQLStore(db), us)
	dav.Props = files.NewSQLPropertyStore(db)
	dav.Locks = files.NewSQLLockStore(db)
	dav.Trash = files.NewTrash(st, files.NewSQLTrashStore(db), dav, us)
	dav.Versions = files.NewVersions(st, files.NewSQLVersionStore(db), dav, us)
	return dav
}

// openRawBackend opens the configured default storage backend without the
// encryption wrapper; the caller does not close it (backends hold no
// resources beyond what the process lifetime covers). This mirrors the
// backend switch in app.openStorage; the helper cannot live in
// internal/storage without an import cycle (the s3/localfs backends import
// the storage package), so the small switch is duplicated here. Callers
// that rewrite file encodings (the encryption sweep commands) need the raw
// backend to sniff sealed content themselves.
func openRawBackend(cfg *config.Config) (storage.Storage, error) {
	name := cfg.Storage.DefaultBackend
	if name == "" {
		name = "local"
	}
	b, ok := cfg.Storage.Backends[name]
	if !ok {
		return nil, fmt.Errorf("ncgo-cli: storage backend %q not configured", name)
	}
	switch b.Type {
	case "localfs":
		return localfs.New(b.Root)
	case "s3":
		return s3store.New(b)
	default:
		return nil, fmt.Errorf("ncgo-cli: storage backend %q type %q unsupported", name, b.Type)
	}
}

// openStorage opens the configured default storage backend exactly as the
// server does: when encryption is enabled the raw backend is wrapped as in
// app.openStorage, so CLI writes (import-nextcloud files) are sealed too.
func openStorage(cfg *config.Config) (storage.Storage, error) {
	st, err := openRawBackend(cfg)
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
