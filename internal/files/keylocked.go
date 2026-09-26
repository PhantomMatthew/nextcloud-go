package files

import (
	"errors"

	"github.com/PhantomMatthew/nextcloud-go/internal/storage/encrypt"
	"github.com/PhantomMatthew/nextcloud-go/internal/webdav"
)

// keyLockedError carries an encrypt.ErrKeyLocked across the webdav package
// boundary (ADR-0101): the webdav package stays encrypt-free, so the
// wrapper dual-matches — errors.Is(err, webdav.ErrForbidden) is true (the
// handler maps it to 403 "encrypted: key locked"), and errors.Is(err,
// encrypt.ErrKeyLocked) is equally true so operator tooling never reports
// the lock as corruption. Unwrap preserves the underlying chain (a wrapped
// ErrIntegrity stays visible).
type keyLockedError struct{ err error }

func (e keyLockedError) Error() string { return e.err.Error() }
func (e keyLockedError) Unwrap() error { return e.err }
func (e keyLockedError) Is(target error) bool {
	return target == webdav.ErrForbidden || target == encrypt.ErrKeyLocked
}

// mapKeyLocked wraps an ErrKeyLocked for the webdav boundary; any other
// error passes through unchanged.
func mapKeyLocked(err error) error {
	if errors.Is(err, encrypt.ErrKeyLocked) {
		return keyLockedError{err: err}
	}
	return err
}
