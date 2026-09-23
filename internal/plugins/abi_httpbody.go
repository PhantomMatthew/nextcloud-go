package plugins

import (
	"context"
	"errors"
	"os"

	"github.com/tetratelabs/wazero/api"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// httpRequestBodyHandle is a guest-built outbound request body spooled to a
// temp file (kind handleStream): an upload body is a stream, so it shares the
// spec §8 64-stream budget with storage streams (ADR-0066). Sealed marks the
// spool consumable by http_request; disposed means the temp file is closed
// and removed, which Close makes idempotent because both the http_request
// consume-and-destroy path and the handle-table cleanup can reach it.
type httpRequestBodyHandle struct {
	f        *os.File
	written  int64
	sealed   bool
	disposed bool
}

// Close discards the spool: close the temp file and remove it.
func (b *httpRequestBodyHandle) Close() error {
	if b.disposed {
		return nil
	}
	b.disposed = true
	name := b.f.Name()
	err := b.f.Close()
	if rerr := os.Remove(name); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}

// httpRequestBodyFromHandle resolves an outbound request body handle: a
// missing id is ErrCodeNotFound, an id of any other handle type (storage
// stream, rows, outbound HTTP response) is ErrCodeInvalidArgument.
func (h *Host) httpRequestBodyFromHandle(mod api.Module, handle int32) (*httpRequestBodyHandle, int32) {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	e, ok := tabs.getAny(handle)
	if !ok {
		return nil, pluginsdk.ErrCodeNotFound
	}
	rb, ok := e.val.(*httpRequestBodyHandle)
	if !ok {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	return rb, pluginsdk.ErrCodeOK
}

// httpRequestBodyCreate opens a temp spool file for a streamed outbound
// request body. It is gated on the http.outbound capability like http_request
// itself: a body the plugin may never send is dead weight on disk. The
// per-target allowlist still gates the eventual http_request call.
func (h *Host) httpRequestBodyCreate(ctx context.Context, mod api.Module) int64 {
	if !pluginCaps(ctx).hasHTTPOutbound() {
		return packI64(pluginsdk.ErrCodePermissionDenied, 0)
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	f, err := os.CreateTemp(h.cfg.SpoolDir, "ncgo-plugin-http-body-*")
	if err != nil {
		return packI64(pluginsdk.ErrCodeInternal, 0)
	}
	rb := &httpRequestBodyHandle{f: f}
	handle, err := tabs.add(handleStream, rb)
	if err != nil {
		if cerr := rb.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
		return packI64(pluginsdk.ErrCodeUnavailable, 0)
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

// httpRequestBodyWrite appends one chunk to the spool. The running total is
// capped at HostConfig.MaxSpoolBytes — the same knob and default (1 GiB) as
// the storage write spools, so one operator setting sizes every plugin
// spool (ADR-0066); the crossing write fails with ErrCodeTooLarge.
func (h *Host) httpRequestBodyWrite(_ context.Context, mod api.Module, handle, bufPtr, bufLen int32) int32 {
	rb, code := h.httpRequestBodyFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if rb.sealed {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if bufLen < 0 || bufLen > maxPayloadArg {
		return pluginsdk.ErrCodeTooLarge
	}
	if rb.written+int64(bufLen) > h.cfg.MaxSpoolBytes {
		return pluginsdk.ErrCodeTooLarge
	}
	data, code := readBytes(mod, bufPtr, bufLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	n, err := rb.f.Write(data)
	rb.written += int64(n)
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return int32(n) //nolint:gosec // G115: bounded by maxPayloadArg
}

// httpRequestBodyClose seals the spool for consumption by http_request. The
// handle stays in the table — http_request removes and destroys it on use —
// but a sealed spool rejects further writes and repeated closes with
// ErrCodeInvalidArgument.
func (h *Host) httpRequestBodyClose(_ context.Context, mod api.Module, handle int32) int32 {
	rb, code := h.httpRequestBodyFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if rb.sealed {
		return pluginsdk.ErrCodeInvalidArgument
	}
	rb.sealed = true
	return pluginsdk.ErrCodeOK
}

// consumeHTTPRequestBody resolves a body_handle for http_request and opens
// the spool for reading. An unsealed spool is ErrCodeInvalidArgument (a
// half-written body must never go out) and stays in the table so the guest
// can seal it and retry. Consumption is destructive (ADR-0066): the handle
// leaves the table, and the returned cleanup — run by http_request once the
// request is done, success or failure — closes the read side and deletes the
// spool file. The read fd itself is owned and closed by http.Client once
// client.Do runs; the cleanup's Close covers only the pre-Do error paths.
func (h *Host) consumeHTTPRequestBody(mod api.Module, handle int32) (*os.File, int64, func(), int32) {
	rb, code := h.httpRequestBodyFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return nil, 0, nil, code
	}
	if !rb.sealed {
		return nil, 0, nil, pluginsdk.ErrCodeInvalidArgument
	}
	tabs := h.handlesFor(mod) // non-nil: resolution above found the table
	tabs.remove(handle, handleStream)
	f, err := os.Open(rb.f.Name())
	if err != nil {
		if cerr := rb.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
		return nil, 0, nil, pluginsdk.ErrCodeInternal
	}
	cleanup := func() {
		if cerr := f.Close(); cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			h.logHandleCleanup(cerr)
		}
		if cerr := rb.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
	}
	return f, rb.written, cleanup, pluginsdk.ErrCodeOK
}
