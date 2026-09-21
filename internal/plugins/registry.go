package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PhantomMatthew/nextcloud-go/internal/database"
)

// ErrPluginNotFound is returned by registry lookups for unknown ids.
var ErrPluginNotFound = errors.New("plugins: not installed")

// RegistryRow is one installed plugin record.
type RegistryRow struct {
	ID             string
	Version        string
	Enabled        bool
	Capabilities   json.RawMessage // granted capability snapshot
	SignatureKeyID string
	ArchivePath    string
	InstalledAt    time.Time
	UpdatedAt      time.Time
}

// Registry persists installed plugin records.
type Registry struct {
	db database.DB
}

// NewRegistry returns a Registry backed by database.DB.
func NewRegistry(db database.DB) *Registry {
	return &Registry{db: db}
}

// Upsert inserts or replaces a plugin record, refreshing updated_at. The
// original installed_at is preserved on update.
func (r *Registry) Upsert(ctx context.Context, row *RegistryRow) error {
	now := time.Now().UTC().UnixMilli()
	enabled := 0
	if row.Enabled {
		enabled = 1
	}
	var query string
	if r.db.Dialect() == database.DialectMySQL {
		query = `
INSERT INTO plugins (id, version, enabled, capabilities_json, signature_keyid, archive_path, installed_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    version = VALUES(version),
    enabled = VALUES(enabled),
    capabilities_json = VALUES(capabilities_json),
    signature_keyid = VALUES(signature_keyid),
    archive_path = VALUES(archive_path),
    updated_at = VALUES(updated_at)`
	} else {
		query = `
INSERT INTO plugins (id, version, enabled, capabilities_json, signature_keyid, archive_path, installed_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    version = excluded.version,
    enabled = excluded.enabled,
    capabilities_json = excluded.capabilities_json,
    signature_keyid = excluded.signature_keyid,
    archive_path = excluded.archive_path,
    updated_at = excluded.updated_at`
	}
	_, err := r.db.Exec(ctx, query,
		row.ID, row.Version, enabled, string(row.Capabilities), row.SignatureKeyID,
		row.ArchivePath, now, now)
	if err != nil {
		return fmt.Errorf("plugins: registry upsert: %w", err)
	}
	return nil
}

// Get returns one plugin record.
func (r *Registry) Get(ctx context.Context, id string) (*RegistryRow, error) {
	row := r.db.QueryRow(ctx, `
SELECT id, version, enabled, capabilities_json, signature_keyid, archive_path, installed_at, updated_at
FROM plugins WHERE id = ?`, id)
	out, err := scanRegistry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("plugins: registry get: %w", err)
	}
	return out, nil
}

// List returns all plugin records ordered by id.
func (r *Registry) List(ctx context.Context) ([]RegistryRow, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, version, enabled, capabilities_json, signature_keyid, archive_path, installed_at, updated_at
FROM plugins ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("plugins: registry list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RegistryRow
	for rows.Next() {
		row, err := scanRegistry(rows)
		if err != nil {
			return nil, fmt.Errorf("plugins: registry list: %w", err)
		}
		out = append(out, *row)
	}
	return out, rows.Err()
}

// ListEnabled returns enabled plugin records ordered by id.
func (r *Registry) ListEnabled(ctx context.Context) ([]RegistryRow, error) {
	rows, err := r.db.Query(ctx, `
SELECT id, version, enabled, capabilities_json, signature_keyid, archive_path, installed_at, updated_at
FROM plugins WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("plugins: registry list enabled: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RegistryRow
	for rows.Next() {
		row, err := scanRegistry(rows)
		if err != nil {
			return nil, fmt.Errorf("plugins: registry list enabled: %w", err)
		}
		out = append(out, *row)
	}
	return out, rows.Err()
}

// SetEnabled flips a plugin's enabled flag.
func (r *Registry) SetEnabled(ctx context.Context, id string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := r.db.Exec(ctx, `UPDATE plugins SET enabled = ?, updated_at = ? WHERE id = ?`,
		v, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("plugins: registry set enabled: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	return nil
}

// Delete removes a plugin record.
func (r *Registry) Delete(ctx context.Context, id string) error {
	res, err := r.db.Exec(ctx, `DELETE FROM plugins WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("plugins: registry delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	return nil
}

type registryScanner interface {
	Scan(dest ...any) error
}

func scanRegistry(s registryScanner) (*RegistryRow, error) {
	var row RegistryRow
	var enabled int
	var caps string
	var installedMs, updatedMs int64
	if err := s.Scan(&row.ID, &row.Version, &enabled, &caps, &row.SignatureKeyID,
		&row.ArchivePath, &installedMs, &updatedMs); err != nil {
		return nil, err
	}
	row.Enabled = enabled != 0
	row.Capabilities = json.RawMessage(caps)
	row.InstalledAt = time.UnixMilli(installedMs).UTC()
	row.UpdatedAt = time.UnixMilli(updatedMs).UTC()
	return &row, nil
}
