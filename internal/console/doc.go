// Package console serves the embedded minimal admin console (ADR-0080): a
// hand-written, build-step-free status/users/jobs page embedded via embed.FS
// and gated to members of the admin group. It is read-only by design — every
// route is GET/HEAD, so there is no CSRF surface.
package console
