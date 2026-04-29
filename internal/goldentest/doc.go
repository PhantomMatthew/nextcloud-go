// Package goldentest implements the wire-contract replay harness described in
// docs/plans/02-golden-harness.md.
//
// Cases live under testdata/golden/<area>/<group>/<NNN>-<slug>/ and consist of
// a case.yaml metadata file plus raw request.http and response.http byte
// fixtures. The runner loads a case, drives either an in-process http.Handler
// or a live HTTP endpoint with the captured request, normalizes both the
// captured and observed responses, and reports a unified diff on mismatch.
//
// This package is test infrastructure: bugs here corrupt every downstream
// signal. It is held to the same coverage bar as production code.
package goldentest
