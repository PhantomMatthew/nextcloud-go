package webdav

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"time"
)

func ComputeETag(size int64, mtime time.Time, path string) string {
	h := sha1.New()
	h.Write([]byte(strconv.FormatInt(size, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(mtime.UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(path))
	return hex.EncodeToString(h.Sum(nil))
}
