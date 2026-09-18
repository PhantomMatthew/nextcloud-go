package files

import (
	"crypto/sha1" //nolint:gosec // non-cryptographic: ETag fingerprint
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxTransferIDLen = 64

// ValidTransferID reports whether tid is a chunked-upload v2 transfer folder name.
func ValidTransferID(tid string) bool {
	if tid == "" || len(tid) > maxTransferIDLen {
		return false
	}
	for _, r := range tid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// NormalizePath returns a canonical DAV path starting with / and without a
// trailing slash except for the root.
func NormalizePath(p string) (string, error) {
	if strings.ContainsRune(p, 0) || !utf8.ValidString(p) {
		return "", ErrInvalidPath
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
		if p == "" {
			p = "/"
		}
	}
	if p == "/" {
		return "/", nil
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for i, seg := range parts {
		if i == 0 {
			continue
		}
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrInvalidPath
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(out, "/"), nil
}

// ComputeFileETag fingerprints a file by id, mtime, and size.
func ComputeFileETag(id int64, mtime time.Time, size int64) string {
	h := sha1.New() //nolint:gosec // non-cryptographic: ETag fingerprint
	h.Write([]byte(strconv.FormatInt(id, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(mtime.UTC().UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(size, 10)))
	return hex.EncodeToString(h.Sum(nil))
}

// ComputeDirETag fingerprints a directory from its direct children.
func ComputeDirETag(children []File) string {
	sorted := append([]File(nil), children...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha1.New() //nolint:gosec // non-cryptographic: ETag fingerprint
	for _, c := range sorted {
		h.Write([]byte(c.ETag))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
