package files

import "errors"

var (
	ErrNotFound      = errors.New("files: not found")
	ErrExists        = errors.New("files: already exists")
	ErrParentMissing = errors.New("files: parent missing")
	ErrNotDir        = errors.New("files: not a directory")
	ErrIsDir         = errors.New("files: is a directory")
	ErrInvalidPath   = errors.New("files: invalid path")
	ErrForbidden     = errors.New("files: forbidden")
	// ErrETagConflict reports a zero-row UpdateMetaIfETag: the etag read when
	// the write preconditions were evaluated no longer matches (ADR-0094).
	ErrETagConflict = errors.New("files: etag conflict")
)
