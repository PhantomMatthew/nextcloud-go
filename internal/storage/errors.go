package storage

import "errors"

var (
	ErrNotFound    = errors.New("storage: not found")
	ErrExists      = errors.New("storage: already exists")
	ErrNotEmpty    = errors.New("storage: directory not empty")
	ErrIsDir       = errors.New("storage: is a directory")
	ErrNotDir      = errors.New("storage: not a directory")
	ErrInvalidPath = errors.New("storage: invalid path")
)
