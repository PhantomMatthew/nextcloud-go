package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/internal/auth"
	"github.com/PhantomMatthew/nextcloud-go/internal/httpx"
	"github.com/PhantomMatthew/nextcloud-go/internal/ocs"
)

// routeBodyChunk is the guest buffer size for streaming response bodies.
const routeBodyChunk = 32 * 1024

// errRequestTooLarge marks a request body above maxPayloadArg; the HTTP
// layer answers 413 without invoking the plugin.
var errRequestTooLarge = errors.New("plugins: request body too large")

// RouteHandler returns an http.Handler dispatching requests for rec to the
// plugin's on_request entry point, streaming the guest response back. The
// first body chunk is read before the status goes out so a guest read
// failure can still be answered with 502; a failure mid-stream (status
// already on the wire) is logged and the connection closes.
func (p *Plugin) RouteHandler(rec RouteRecord) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pr, err := p.beginRequest(req)
		if err != nil {
			p.failRequest(w, req, err)
			return
		}
		bufPtr, err := pr.allocBodyBuf()
		if err != nil {
			pr.close()
			p.failRequest(w, req, err)
			return
		}
		first, err := pr.readChunk(bufPtr)
		if err != nil {
			pr.freeBodyBuf(bufPtr)
			pr.close()
			p.failRequest(w, req, err)
			return
		}
		hdr := w.Header()
		for _, h := range pr.headers {
			hdr.Add(h.name, h.value)
		}
		w.WriteHeader(pr.status)
		if len(first) > 0 {
			_, _ = w.Write(first)
		}
		if first != nil { // nil first chunk is EOF
			for {
				chunk, err := pr.readChunk(bufPtr)
				if err != nil {
					p.logRequest(req, "plugins: response stream failed", err)
					break
				}
				if chunk == nil {
					break
				}
				_, _ = w.Write(chunk)
			}
		}
		pr.freeBodyBuf(bufPtr)
		pr.close()
	})
}

// OCSHandler is RouteHandler with the plugin response wrapped in an OCS
// envelope for version: the guest body must be a JSON document that becomes
// the envelope's data element.
func (p *Plugin) OCSHandler(rec RouteRecord, version ocs.Version) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		pr, err := p.beginRequest(req)
		if err != nil {
			p.failRequest(w, req, err)
			return
		}
		var buf bytes.Buffer
		err = pr.streamBody(&buf)
		pr.close()
		if err != nil {
			p.failRequest(w, req, err)
			return
		}
		var data any
		if body := bytes.TrimSpace(buf.Bytes()); len(body) > 0 {
			if err := json.Unmarshal(body, &data); err != nil {
				p.failRequest(w, req, fmt.Errorf("plugins: ocs response is not JSON: %w", err))
				return
			}
			data = ocsValue(data)
		}
		success := pr.status >= 200 && pr.status < 300
		code := ocs.RespondServerError
		if success {
			code = ocs.StatusOKv1
			if version == ocs.V2 {
				code = ocs.StatusOKv2
			}
		}
		meta := ocs.Meta{StatusCode: code}
		if !success {
			meta.Status = "failure"
			meta.Message = "plugin request failed"
		}
		format := ocs.NegotiateFormat(req.URL.Query().Get("format"), req.Header.Get("Accept"))
		out, contentType, err := ocs.Render(version, format, meta, data)
		if err != nil {
			p.failRequest(w, req, fmt.Errorf("plugins: ocs render: %w", err))
			return
		}
		w.Header().Set("Content-Type", contentType)
		status := pr.status
		if success {
			status = ocs.Map(version, code)
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	})
}

// ocsValue converts decoded JSON into OCS-renderable values: raw maps are
// not allowed in OCS payloads, so objects become OrderedMaps with sorted
// keys.
func ocsValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(ocs.OrderedMap, 0, len(t))
		for _, k := range keys {
			out = append(out, ocs.K(k, ocsValue(t[k])))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = ocsValue(e)
		}
		return out
	default:
		return v
	}
}

// MountRoutes mounts every persisted route of the given started plugins.
// Plain routes are wrapped with routeMw; OCS endpoints are mounted under
// both /ocs/v1.php and /ocs/v2.php with the matching auth middleware. A
// per-plugin registry failure is logged and skipped (StartEnabled
// philosophy): one broken plugin must not prevent the others from mounting.
func MountRoutes(ctx context.Context, router *httpx.Router, ps []*Plugin, reg *Registry, routeMw, ocsV1Mw, ocsV2Mw httpx.Middleware, logger *slog.Logger) error {
	if router == nil || reg == nil {
		return errors.New("plugins: mount routes: nil router or registry")
	}
	mws := func(mw httpx.Middleware) []httpx.Middleware {
		if mw == nil {
			return nil
		}
		return []httpx.Middleware{mw}
	}
	for _, p := range ps {
		if p == nil {
			continue
		}
		recs, err := reg.RoutesForPlugin(ctx, p.manifest.Plugin.ID)
		if err != nil {
			if logger != nil {
				logger.ErrorContext(ctx, "plugins: list routes failed",
					slog.String("plugin.id", p.manifest.Plugin.ID), slog.String("error", err.Error()))
			}
			continue
		}
		for _, rec := range recs {
			switch rec.Kind {
			case "route":
				router.Handle(rec.Method, rec.Path, p.RouteHandler(rec), mws(routeMw)...)
			case "ocs":
				router.Handle(rec.Method, "/ocs/v1.php"+rec.Path, p.OCSHandler(rec, ocs.V1), mws(ocsV1Mw)...)
				router.Handle(rec.Method, "/ocs/v2.php"+rec.Path, p.OCSHandler(rec, ocs.V2), mws(ocsV2Mw)...)
			default:
				if logger != nil {
					logger.WarnContext(ctx, "plugins: unknown route kind, skipped",
						slog.String("plugin.id", p.manifest.Plugin.ID), slog.String("kind", rec.Kind),
						slog.String("path", rec.Path))
				}
			}
		}
	}
	return nil
}

// failRequest logs err and answers the request: 413 for an oversized body,
// 502 for everything the plugin did wrong.
func (p *Plugin) failRequest(w http.ResponseWriter, req *http.Request, err error) {
	if errors.Is(err, errRequestTooLarge) {
		http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		return
	}
	p.logRequest(req, "plugins: request dispatch failed", err)
	http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
}

func (p *Plugin) logRequest(req *http.Request, msg string, err error) {
	if p.host.logger == nil {
		return
	}
	p.host.logger.WarnContext(req.Context(), msg,
		slog.String("plugin.id", p.manifest.Plugin.ID),
		slog.String("path", req.URL.Path),
		slog.String("error", err.Error()))
}

// packRequest reads the body (capped at maxPayloadArg) and marshals the
// request as a MessagePack map. Deviation from spec §7: the body travels
// inline as body_bytes instead of a body_handle.
func packRequest(req *http.Request) ([]byte, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxPayloadArg+1))
		if err != nil {
			return nil, fmt.Errorf("plugins: read request body: %w", err)
		}
		if len(b) > maxPayloadArg {
			return nil, errRequestTooLarge
		}
		body = b
	}
	headers := make(map[string]string, len(req.Header))
	for k, vs := range req.Header {
		headers[k] = strings.Join(vs, ", ")
	}
	payload, err := msgpack.Marshal(map[string]any{
		"method":     req.Method,
		"path":       req.URL.Path,
		"query":      req.URL.RawQuery,
		"headers":    headers,
		"body_bytes": body,
	})
	if err != nil {
		return nil, fmt.Errorf("plugins: marshal request: %w", err)
	}
	return payload, nil
}

// routeHeader is one parsed plugin response header line.
type routeHeader struct{ name, value string }

// pendingResponse is an in-flight plugin request: the acquired instance, the
// guest response handle, and the status/headers already read. The caller
// streams the body and must call close.
type pendingResponse struct {
	p       *Plugin
	ctx     context.Context
	inst    *instance
	release func(broken bool)
	handle  int32
	status  int
	headers []routeHeader
	broken  bool
	closed  bool
}

// beginRequest packs the request, invokes the plugin's on_request entry
// point (alloc/write/call/free like deliverEvent), and reads the response
// status and headers. The body is streamed later on the same instance.
func (p *Plugin) beginRequest(req *http.Request) (*pendingResponse, error) {
	payload, err := packRequest(req)
	if err != nil {
		return nil, err
	}
	cc := CallContext{
		RequestID: httpx.RequestIDFromContext(req.Context()),
		Locale:    req.Header.Get("Accept-Language"),
		Deadline:  time.Now().Add(p.callTimeout()),
	}
	if principal, ok := auth.UserFromContext(req.Context()); ok {
		cc.UserID = principal.UID
	}
	callCtx, cancel := context.WithTimeout(withCall(WithCallContext(req.Context(), cc), p, false), p.callTimeout())
	inst, release, err := p.manager.acquire(callCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	pr := &pendingResponse{
		p:    p,
		ctx:  callCtx,
		inst: inst,
		release: func(broken bool) {
			release(broken)
			cancel()
		},
	}

	entry := p.manifest.EntryPoints.OnRequest
	fn := inst.mod.ExportedFunction(entry)
	alloc := inst.mod.ExportedFunction("ncgo_alloc")
	freeFn := inst.mod.ExportedFunction("ncgo_free")
	if entry == "" || fn == nil || alloc == nil || freeFn == nil {
		pr.release(false)
		return nil, fmt.Errorf("%w: %s", ErrMissingExport, entry)
	}
	for _, name := range []string{
		"ncgo_response_status", "ncgo_response_header_count", "ncgo_response_header_at",
		"ncgo_response_body_read", "ncgo_response_close",
	} {
		if inst.mod.ExportedFunction(name) == nil {
			pr.release(false)
			return nil, fmt.Errorf("%w: %s", ErrMissingExport, name)
		}
	}

	reqBuf, err := guestWrite(callCtx, inst.mod, alloc, payload)
	if err != nil {
		pr.release(true)
		return nil, err
	}
	results, callErr := fn.Call(callCtx,
		uint64(uint32(reqBuf.ptr)),  //nolint:gosec // G115: u32 bit pattern
		uint64(uint32(reqBuf.size)), //nolint:gosec // G115: bounded by maxPayloadArg
	)
	freeErr := guestFree(callCtx, freeFn, reqBuf)
	switch {
	case callErr != nil:
		pr.release(true)
		return nil, wrapTrap(callErr)
	case freeErr != nil:
		pr.release(true)
		return nil, freeErr
	}
	if len(results) == 0 {
		pr.release(false)
		return nil, errors.New("plugins: on_request returned no result")
	}
	packed := int64(results[0])                      //nolint:gosec // G115: intentional ABI unpacking
	code, handle := int32(packed>>32), int32(packed) //nolint:gosec // G115: intentional ABI unpacking
	if code != 0 {
		pr.release(false)
		return nil, &PluginError{Code: code}
	}
	pr.handle = handle

	status, err := pr.callExport("ncgo_response_status", uint64(uint32(handle))) //nolint:gosec // G115: u32 bit pattern
	if err != nil {
		pr.release(true)
		return nil, err
	}
	pr.status = int(int32(status)) //nolint:gosec // G115: plugin i32 return
	if pr.status < 100 || pr.status > 599 {
		pr.close()
		return nil, fmt.Errorf("plugins: invalid response status %d", pr.status)
	}
	if err := pr.readHeaders(); err != nil {
		pr.close()
		return nil, err
	}
	return pr, nil
}

// callExport invokes one guest export on the held instance; a trap marks the
// instance broken so close destroys it.
func (pr *pendingResponse) callExport(name string, args ...uint64) (uint64, error) {
	fn := pr.inst.mod.ExportedFunction(name)
	if fn == nil {
		return 0, fmt.Errorf("%w: %s", ErrMissingExport, name)
	}
	results, err := fn.Call(pr.ctx, args...)
	if err != nil {
		pr.broken = true
		return 0, wrapTrap(err)
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("plugins: %s returned no result", name)
	}
	return results[0], nil
}

// readHeaders pulls the "Name: Value" header lines out of the guest.
// Malformed lines are logged and skipped; Content-Length is ignored (the
// host sets it).
func (pr *pendingResponse) readHeaders() error {
	count, err := pr.callExport("ncgo_response_header_count", uint64(uint32(pr.handle))) //nolint:gosec // G115: u32 bit pattern
	if err != nil {
		return err
	}
	n := int32(count) //nolint:gosec // G115: plugin i32 return
	if n < 0 || n > 256 {
		return fmt.Errorf("plugins: invalid header count %d", n)
	}
	if n == 0 {
		return nil
	}
	alloc := pr.inst.mod.ExportedFunction("ncgo_alloc")
	freeFn := pr.inst.mod.ExportedFunction("ncgo_free")
	res, err := alloc.Call(pr.ctx, uint64(maxStringArg))
	if err != nil {
		pr.broken = true
		return wrapTrap(err)
	}
	bufPtr := int32(res[0]) //nolint:gosec // G115: guest pointer
	if bufPtr == 0 {
		return errors.New("plugins: guest alloc returned 0")
	}
	defer func() {
		if ferr := guestFree(pr.ctx, freeFn, guestBuf{ptr: bufPtr, size: maxStringArg}); ferr != nil {
			pr.broken = true
		}
	}()
	for i := int32(0); i < n; i++ {
		res, err := pr.callExport("ncgo_response_header_at",
			uint64(uint32(pr.handle)), //nolint:gosec // G115: u32 bit pattern
			uint64(uint32(i)),
			uint64(uint32(bufPtr)), //nolint:gosec // G115: u32 bit pattern
			uint64(uint32(maxStringArg)))
		if err != nil {
			return err
		}
		ln := int32(res) //nolint:gosec // G115: plugin i32 return
		if ln < 0 {
			return fmt.Errorf("plugins: header_at failed with %d", ln)
		}
		if ln > maxStringArg {
			return fmt.Errorf("plugins: header line too long %d", ln)
		}
		b, ok := pr.inst.mod.Memory().Read(uint32(bufPtr), uint32(ln)) //nolint:gosec // G115: bounds checked above
		if !ok {
			return errors.New("plugins: guest memory read failed")
		}
		name, value, found := strings.Cut(string(b), ": ")
		switch {
		case !found || name == "":
			pr.logWarn("plugins: malformed response header line skipped", slog.String("line", string(b)))
		case strings.EqualFold(name, "Content-Length"):
			// Ignored: the host sets Content-Length.
		default:
			pr.headers = append(pr.headers, routeHeader{name: name, value: value})
		}
	}
	return nil
}

// allocBodyBuf allocates the shared 32 KiB body buffer in the guest.
func (pr *pendingResponse) allocBodyBuf() (int32, error) {
	res, err := pr.callExport("ncgo_alloc", uint64(routeBodyChunk))
	if err != nil {
		return 0, err
	}
	bufPtr := int32(res) //nolint:gosec // G115: guest pointer
	if bufPtr == 0 {
		return 0, errors.New("plugins: guest alloc returned 0")
	}
	return bufPtr, nil
}

// freeBodyBuf releases the shared body buffer best-effort.
func (pr *pendingResponse) freeBodyBuf(bufPtr int32) {
	if ferr := guestFree(pr.ctx, pr.inst.mod.ExportedFunction("ncgo_free"), guestBuf{ptr: bufPtr, size: routeBodyChunk}); ferr != nil {
		pr.broken = true
	}
}

// readChunk reads the next body chunk into the guest buffer and copies it
// out; nil means EOF, a negative guest result is a PluginError.
func (pr *pendingResponse) readChunk(bufPtr int32) ([]byte, error) {
	res, err := pr.callExport("ncgo_response_body_read",
		uint64(uint32(pr.handle)), //nolint:gosec // G115: u32 bit pattern
		uint64(uint32(bufPtr)),    //nolint:gosec // G115: u32 bit pattern
		uint64(uint32(routeBodyChunk)))
	if err != nil {
		return nil, err
	}
	n := int32(res) //nolint:gosec // G115: plugin i32 return
	switch {
	case n < 0:
		return nil, &PluginError{Code: n}
	case n == 0:
		return nil, nil
	case n > routeBodyChunk:
		return nil, fmt.Errorf("plugins: body read overflow %d", n)
	}
	b, ok := pr.inst.mod.Memory().Read(uint32(bufPtr), uint32(n)) //nolint:gosec // G115: bounds checked above
	if !ok {
		return nil, errors.New("plugins: guest memory read failed")
	}
	return b, nil
}

// streamBody copies the guest response body to w in routeBodyChunk chunks,
// freeing the buffer afterwards.
func (pr *pendingResponse) streamBody(w io.Writer) error {
	bufPtr, err := pr.allocBodyBuf()
	if err != nil {
		return err
	}
	defer pr.freeBodyBuf(bufPtr)
	for {
		chunk, err := pr.readChunk(bufPtr)
		if err != nil {
			return err
		}
		if chunk == nil {
			return nil
		}
		if _, err := w.Write(chunk); err != nil {
			return fmt.Errorf("plugins: write response body: %w", err)
		}
	}
}

// close calls ncgo_response_close best-effort and releases the instance
// (destroying it when any call trapped).
func (pr *pendingResponse) close() {
	if pr.closed {
		return
	}
	pr.closed = true
	if _, err := pr.callExport("ncgo_response_close", uint64(uint32(pr.handle))); err != nil { //nolint:gosec // G115: u32 bit pattern
		pr.logWarn("plugins: response close failed", slog.String("error", err.Error()))
	}
	pr.release(pr.broken)
}

func (pr *pendingResponse) logWarn(msg string, args ...any) {
	if pr.p.host.logger != nil {
		pr.p.host.logger.WarnContext(pr.ctx, msg,
			append([]any{slog.String("plugin.id", pr.p.manifest.Plugin.ID)}, args...)...)
	}
}

// guestWrite allocates len(data) bytes in the guest and copies data in.
func guestWrite(ctx context.Context, mod api.Module, alloc api.Function, data []byte) (guestBuf, error) {
	if len(data) == 0 {
		return guestBuf{}, nil
	}
	res, err := alloc.Call(ctx, uint64(len(data)))
	if err != nil {
		return guestBuf{}, wrapTrap(err)
	}
	ptr := int32(res[0]) //nolint:gosec // G115: guest pointer
	if ptr == 0 {
		return guestBuf{}, errors.New("plugins: guest alloc returned 0")
	}
	if !mod.Memory().Write(uint32(ptr), data) { //nolint:gosec // G115: ptr checked positive above
		return guestBuf{}, errors.New("plugins: guest memory write failed")
	}
	return guestBuf{ptr: ptr, size: int32(len(data))}, nil //nolint:gosec // G115: bounded by maxPayloadArg
}

// guestFree releases one guest buffer best-effort.
func guestFree(ctx context.Context, freeFn api.Function, b guestBuf) error {
	if b.ptr == 0 {
		return nil
	}
	if _, err := freeFn.Call(ctx,
		uint64(uint32(b.ptr)),  //nolint:gosec // G115: u32 bit pattern
		uint64(uint32(b.size)), //nolint:gosec // G115: u32 bit pattern
	); err != nil {
		return wrapTrap(err)
	}
	return nil
}
