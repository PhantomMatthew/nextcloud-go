package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	migratedb "github.com/golang-migrate/migrate/v4/database"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	migratesqlite "github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	ncdb "github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// ErrDirty is returned when schema_migrations.dirty is set.
var ErrDirty = errors.New("migrations: schema is dirty")

type instance struct {
	m      *migrate.Migrate
	source source.Driver
}

func newInstance(db *sql.DB, d ncdb.Dialect) (*instance, error) {
	dir, err := dialectDir(d)
	if err != nil {
		return nil, err
	}
	src, err := iofs.New(sqlFS, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: source: %w", err)
	}
	drv, err := newDriver(db, d)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, string(d), drv)
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("migrations: migrate: %w", err)
	}
	return &instance{m: m, source: src}, nil
}

func (in *instance) close() {
	// migrate.Close closes the *sql.DB pool. Drop only the iofs source so
	// the caller's pool stays usable after Up/Down/Version.
	if in.source != nil {
		_ = in.source.Close()
	}
}

func dialectDir(d ncdb.Dialect) (string, error) {
	switch d {
	case ncdb.DialectPostgres:
		return "sql/postgres", nil
	case ncdb.DialectMySQL:
		return "sql/mysql", nil
	case ncdb.DialectSQLite:
		return "sql/sqlite", nil
	default:
		return "", fmt.Errorf("migrations: %w: %q", ncdb.ErrUnsupportedDriver, d)
	}
}

func newDriver(db *sql.DB, d ncdb.Dialect) (migratedb.Driver, error) {
	switch d {
	case ncdb.DialectPostgres:
		return migratepgx.WithInstance(db, &migratepgx.Config{})
	case ncdb.DialectMySQL:
		return migratemysql.WithInstance(db, &migratemysql.Config{})
	case ncdb.DialectSQLite:
		return migratesqlite.WithInstance(db, &migratesqlite.Config{})
	default:
		return nil, fmt.Errorf("migrations: %w: %q", ncdb.ErrUnsupportedDriver, d)
	}
}

func current(m *migrate.Migrate) (version uint, nilVersion bool, err error) {
	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if dirty {
		return version, false, ErrDirty
	}
	return version, false, nil
}

// Up applies all pending migrations and returns how many versions were applied.
func Up(ctx context.Context, db *sql.DB, d ncdb.Dialect, logger *slog.Logger) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	in, err := newInstance(db, d)
	if err != nil {
		return 0, err
	}
	defer in.close()

	before, nilBefore, err := current(in.m)
	if err != nil {
		return 0, err
	}
	if err := in.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, err
	}
	after, _, err := current(in.m)
	if err != nil {
		return 0, err
	}
	applied := int(after - before)
	if nilBefore {
		applied = int(after)
	}
	if logger != nil {
		logger.InfoContext(ctx, "migrations applied", slog.Int("count", applied), slog.String("dialect", string(d)))
	}
	return applied, nil
}

// Down rolls back up to steps migrations.
func Down(ctx context.Context, db *sql.DB, d ncdb.Dialect, steps int, logger *slog.Logger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if steps <= 0 {
		return fmt.Errorf("migrations: steps must be > 0")
	}
	in, err := newInstance(db, d)
	if err != nil {
		return err
	}
	defer in.close()

	if _, _, err := current(in.m); err != nil {
		return err
	}
	if err := in.m.Steps(-steps); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	if logger != nil {
		logger.InfoContext(ctx, "migrations rolled back", slog.Int("steps", steps), slog.String("dialect", string(d)))
	}
	return nil
}

// Version returns the current schema version and dirty flag.
func Version(ctx context.Context, db *sql.DB, d ncdb.Dialect) (uint, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	in, err := newInstance(db, d)
	if err != nil {
		return 0, false, err
	}
	defer in.close()

	v, dirty, err := in.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, dirty, nil
	}
	return v, dirty, err
}
