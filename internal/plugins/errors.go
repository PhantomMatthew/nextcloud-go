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
	// ErrFuelExhausted kills a plugin call that exceeds its manifest's
	// runtime.fuel_per_call budget of wasm function entries (ADR-0103). The
	// instance is destroyed exactly like a trap, but the error is never
	// wrapped in ErrTrap.
	ErrFuelExhausted = errors.New("plugins: fuel exhausted")
)

// PluginError is a non-zero i32 returned by a plugin entry point.
type PluginError struct {
	Code int32
}

func (e *PluginError) Error() string {
	return fmt.Sprintf("plugins: plugin error %d", e.Code)
}
