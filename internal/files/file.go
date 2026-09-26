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
	// KeyUUID is the 16-byte key UUID of the file's v3 encryption envelope
	// (ADR-0097); nil for plaintext-on-backend or v1/v2-sealed files.
	KeyUUID []byte
}
