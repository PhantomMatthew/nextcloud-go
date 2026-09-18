package files

import "time"

// File is one filecache row.
type File struct {
	ID          int64
	UserID      int64
	ParentID    *int64
	Name        string
	Path        string
	IsDir       bool
	Size        int64
	Mtime       time.Time
	ETag        string
	Checksum    string
	MIME        string
	Permissions int
}
