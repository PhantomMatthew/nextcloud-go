package plugins

import (
	"errors"
	"fmt"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

const (
	ErrCodeOK               = pluginsdk.ErrCodeOK
	ErrCodeInternal         = pluginsdk.ErrCodeInternal
	ErrCodeInvalidArgument  = pluginsdk.ErrCodeInvalidArgument
	ErrCodePermissionDenied = pluginsdk.ErrCodePermissionDenied
	ErrCodeNotFound         = pluginsdk.ErrCodeNotFound
	ErrCodeAlreadyExists    = pluginsdk.ErrCodeAlreadyExists
	ErrCodeTimeout          = pluginsdk.ErrCodeTimeout
	ErrCodeCanceled         = pluginsdk.ErrCodeCanceled
	ErrCodeQuotaExceeded    = pluginsdk.ErrCodeQuotaExceeded
	ErrCodeUnsupported      = pluginsdk.ErrCodeUnsupported
	ErrCodeConflict         = pluginsdk.ErrCodeConflict
	ErrCodeTooLarge         = pluginsdk.ErrCodeTooLarge
	ErrCodeUnavailable      = pluginsdk.ErrCodeUnavailable
)

var (
	ErrManifestInvalid = errors.New("plugins: invalid manifest")
	ErrABIMismatch     = errors.New("plugins: abi mismatch")
	ErrMissingExport   = errors.New("plugins: missing required export")
	ErrForbiddenImport = errors.New("plugins: forbidden import")
	ErrTrap            = errors.New("plugins: wasm trap")
)

// PluginError is a non-zero i32 returned by a plugin entry point.
type PluginError struct {
	Code int32
}

func (e *PluginError) Error() string {
	return fmt.Sprintf("plugins: plugin error %d", e.Code)
}
