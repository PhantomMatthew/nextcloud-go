package webdav

import (
	"crypto/sha1" //nolint:gosec // non-cryptographic: ETag fingerprint, not a security primitive
	"encoding/hex"
	"strconv"
	"time"
)

func ComputeETag(size int64, mtime time.Time, path string) string {
	h := sha1.New() //nolint:gosec // non-cryptographic: ETag fingerprint, not a security primitive
	h.Write([]byte(strconv.FormatInt(size, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(mtime.UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil))
}
