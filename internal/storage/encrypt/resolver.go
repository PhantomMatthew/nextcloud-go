package encrypt

import (
	"context"
	"errors"
)

// ErrUnresolvableKey reports a v3-sealed file whose key cannot be resolved:
// the key UUID names no known file key, the owning user key row is missing,
// or a wrap fails authentication because the keyring does not match. It is
// an operator key/configuration problem, deliberately distinct from
// ErrIntegrity's wrong-key/corruption signal (the ADR-0074 ErrUnknownKeyID
// precedent); the two must never be conflated in operator tooling.
var ErrUnresolvableKey = errors.New("encrypt: unresolvable file key")

// KeyResolver is the narrow seam between the storage decorator and the
// per-user key hierarchy (ADR-0096 phase 1, implemented in ADR-0097). The
// decorator sees only storage keys; the resolver owns the master→UK→FK
// chain and its persistence. A nil resolver keeps the FS v1/v2-only and
// bit-identical to pre-v3 builds.
type KeyResolver interface {
	// Allocate generates a fresh file key, wraps it for the owner derived
	// from the storage key, persists the wrap row, and returns the key UUID
	// (written into the v3 header) with the plaintext file key. It lazily
	// creates the owner's user key on first use.
	Allocate(ctx context.Context, storageKey string) (keyUUID [16]byte, fk []byte, err error)
	// Resolve unwraps the file key named by keyUUID (the owner's row
	// preferred) and returns it. Failures wrap ErrUnresolvableKey, never
	// ErrIntegrity.
	Resolve(ctx context.Context, keyUUID [16]byte) (fk []byte, err error)
}
