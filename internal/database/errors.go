package database

import (
	"database/sql"
	"errors"
)

var (
	// ErrUnsupportedDriver is returned by Open when Config.Driver is not a
	// known dialect.
	ErrUnsupportedDriver = errors.New("database: unsupported driver")

	// ErrNoRows is sql.ErrNoRows, re-exported so callers need not import
	// database/sql solely to test for an empty QueryRow.
	ErrNoRows = sql.ErrNoRows
)
