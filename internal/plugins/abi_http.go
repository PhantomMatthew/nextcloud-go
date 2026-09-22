package plugins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// Outbound HTTP timeout policy: TimeoutMS <= 0 gets the default; anything
// above the cap is clamped (never an error).
const (
	defaultHTTPTimeout = 10 * time.Second
	maxHTTPTimeout     = 30 * time.Second
)

// defaultMaxHTTPResponseBytes caps the body bytes one response may deliver
// to the guest (ADR-0059); HostConfig.MaxHTTPResponseBytes <= 0 selects it.
const defaultMaxHTTPResponseBytes int64 = 32 << 20

// errHTTPRedirectDenied marks a redirect target rejected by the plugin's
// http.outbound allowlist; client.Do wraps it in a *url.Error.
var errHTTPRedirectDenied = errors.New("plugins: redirect target not granted")

// errHTTPResponseTooLarge marks a response body that crossed the host's
// per-response byte cap (ADR-0059); http_response_body_read maps it to
// ErrTooLarge.
var errHTTPResponseTooLarge = errors.New("plugins: http response body exceeds limit")

// limitedBody caps the bytes a guest can pull from one response body: the
// read that pushes the running total past limit fails with
// errHTTPResponseTooLarge instead of delivering the chunk, and every later
// read keeps failing (the total only grows), so a guest cannot retry-loop
// around the cap. Close always reaches the wrapped body.
type limitedBody struct {
	body  io.ReadCloser
	limit int64
	total int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.total > b.limit {
		return 0, errHTTPResponseTooLarge
	}
	n, err := b.body.Read(p)
	b.total += int64(n)
	if b.total > b.limit {
		return 0, errHTTPResponseTooLarge
	}
	return n, err
}

func (b *limitedBody) Close() error { return b.body.Close() }

var httpAllowedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodDelete:  true,
	http.MethodPatch:   true,
	http.MethodOptions: true,
}

// httpOutboundRequest is the MessagePack shape passed to http_request.
type httpOutboundRequest struct {
	Method    string            `msgpack:"method"`
	URL       string            `msgpack:"url"`
	Headers   map[string]string `msgpack:"headers"`
	BodyBytes []byte            `msgpack:"body_bytes"`
	TimeoutMS int32             `msgpack:"timeout_ms"`
}

// httpResponseHandle owns one in-flight response: the body streams to the
// guest until http_response_close (or handle-table cleanup) closes it and
// cancels the request context. target is the allowlist-normalized request
// host and limit the response body byte cap (ADR-0059) — both kept for the
// cap-tripped security log.
type httpResponseHandle struct {
	resp   *http.Response
	cancel context.CancelFunc
	target string
	limit  int64
}

// Close releases the body and the request context.
func (r *httpResponseHandle) Close() error {
	err := r.resp.Body.Close()
	r.cancel()
	return err
}

// httpTarget normalizes a URL to the allowlist match form: the lowercase
// host alone when the port is the scheme default (or absent), else
// host:port. IPv6 literals are unbracketed by Hostname and can never match
// a grant (the manifest host:port grammar forbids colons in hosts).
func httpTarget(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	switch port := u.Port(); {
	case port == "", u.Scheme == "http" && port == "80", u.Scheme == "https" && port == "443":
		return host
	default:
		return host + ":" + port
	}
}

// httpTimeout resolves the per-request timeout: default when unset,
// clamped to maxHTTPTimeout otherwise.
func httpTimeout(ms int32) time.Duration {
	if ms <= 0 {
		return defaultHTTPTimeout
	}
	d := time.Duration(ms) * time.Millisecond
	if d > maxHTTPTimeout {
		return maxHTTPTimeout
	}
	return d
}

// mapHTTPErr translates outbound request failures into ABI codes.
func (h *Host) mapHTTPErr(ctx context.Context, err error) int32 {
	switch {
	case errors.Is(err, errHTTPRedirectDenied):
		return pluginsdk.ErrCodePermissionDenied
	case errors.Is(err, errEgressPrivateIP):
		// SSRF guard fired: the allowlist passed but the resolved target IP
		// is loopback/private/link-local/unspecified. This is a security
		// signal (literal internal IP or DNS rebinding), so warn loudly.
		if h.logger != nil {
			var pluginID string
			if info := callFromCtx(ctx); info.plugin != nil {
				pluginID = info.plugin.manifest.Plugin.ID
			}
			h.logger.WarnContext(ctx, "plugins: http_request blocked by private-IP egress guard",
				slog.String("plugin", pluginID),
				slog.String("error", err.Error()))
		}
		return pluginsdk.ErrCodePermissionDenied
	case errors.Is(err, context.DeadlineExceeded):
		return pluginsdk.ErrCodeTimeout
	case errors.Is(err, context.Canceled):
		return pluginsdk.ErrCodeCanceled
	default:
		if h.logger != nil {
			h.logger.WarnContext(ctx, "plugins: http_request failed",
				slog.String("error", err.Error()))
		}
		return pluginsdk.ErrCodeInternal
	}
}

func (h *Host) httpRequest(ctx context.Context, mod api.Module, reqPtr, reqLen int32) int64 {
	caps := pluginCaps(ctx)
	if !caps.hasHTTPOutbound() {
		return packI64(pluginsdk.ErrCodePermissionDenied, 0)
	}
	raw, code := readBytes(mod, reqPtr, reqLen)
	if code != pluginsdk.ErrCodeOK {
		return packI64(code, 0)
	}
	var req httpOutboundRequest
	if err := msgpack.Unmarshal(raw, &req); err != nil {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	if !httpAllowedMethods[req.Method] {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	if !caps.canHTTPOutbound(httpTarget(u)) {
		return packI64(pluginsdk.ErrCodePermissionDenied, 0)
	}
	// Per-plugin rate limit (ADR-0059): one token per http_request call —
	// the redirect chain inside client.Do rides on the same token. Both the
	// guarded and the unguarded client draw from the same bucket.
	var pluginID string
	if info := callFromCtx(ctx); info.plugin != nil {
		pluginID = info.plugin.manifest.Plugin.ID
	}
	if !h.allowHTTPRequest(pluginID) {
		if h.logger != nil {
			h.logger.WarnContext(ctx, "plugins: http_request rate limit exceeded",
				slog.String("plugin", pluginID),
				slog.Int("rate_per_minute", h.cfg.HTTPRatePerMinute))
		}
		return packI64(pluginsdk.ErrCodeQuotaExceeded, 0)
	}
	var body *bytes.Reader
	if len(req.BodyBytes) > 0 {
		body = bytes.NewReader(req.BodyBytes)
	} else {
		body = bytes.NewReader(nil)
	}
	reqCtx, cancel := context.WithTimeout(ctx, httpTimeout(req.TimeoutMS))
	hreq, err := http.NewRequestWithContext(reqCtx, req.Method, req.URL, body)
	if err != nil {
		cancel()
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, "Host") {
			continue // plugin may not spoof the Host header
		}
		hreq.Header.Set(k, v)
	}
	// Shallow-copy the per-plugin client (guarded unless the plugin holds
	// http.outbound_allow_private) so redirect re-validation does not mutate
	// it; every redirect target must pass the same allowlist.
	client := *h.httpClientFor(caps)
	prevCheck := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !caps.canHTTPOutbound(httpTarget(next.URL)) {
			return errHTTPRedirectDenied
		}
		if prevCheck != nil {
			return prevCheck(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := client.Do(hreq)
	if err != nil {
		cancel()
		return packI64(h.mapHTTPErr(ctx, err), 0)
	}
	// Cap the body bytes this response may deliver to the guest (ADR-0059);
	// the wrapping reader fails the crossing read with
	// errHTTPResponseTooLarge instead of silently truncating.
	limit := h.cfg.MaxHTTPResponseBytes
	resp.Body = &limitedBody{body: resp.Body, limit: limit}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
		cancel()
		return packI64(pluginsdk.ErrCodeInvalidArgument, 0)
	}
	rh := &httpResponseHandle{resp: resp, cancel: cancel, target: httpTarget(u), limit: limit}
	handle, err := tabs.add(handleHTTP, rh)
	if err != nil {
		if cerr := rh.Close(); cerr != nil {
			h.logHandleCleanup(cerr)
		}
		return packI64(pluginsdk.ErrCodeUnavailable, 0)
	}
	return packI64(pluginsdk.ErrCodeOK, handle)
}

// httpResponseFromHandle resolves a handleHTTP entry.
func (h *Host) httpResponseFromHandle(mod api.Module, handle int32) (*httpResponseHandle, int32) {
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return nil, pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.get(handle, handleHTTP)
	if !ok {
		return nil, pluginsdk.ErrCodeNotFound
	}
	rh, ok := v.(*httpResponseHandle)
	if !ok {
		return nil, pluginsdk.ErrCodeInternal
	}
	return rh, pluginsdk.ErrCodeOK
}

func (h *Host) httpResponseStatus(ctx context.Context, mod api.Module, handle int32) int32 {
	if !pluginCaps(ctx).hasHTTPOutbound() {
		return pluginsdk.ErrCodePermissionDenied
	}
	rh, code := h.httpResponseFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	return int32(rh.resp.StatusCode) //nolint:gosec // G115: status codes are 100-599
}

func (h *Host) httpResponseHeader(ctx context.Context, mod api.Module, handle, namePtr, nameLen, outPtr, outMax int32) int32 {
	if !pluginCaps(ctx).hasHTTPOutbound() {
		return pluginsdk.ErrCodePermissionDenied
	}
	name, code := readString(mod, namePtr, nameLen)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	rh, code := h.httpResponseFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	// Multiple values are joined with ", " (RFC 9110 list semantics); an
	// absent header writes nothing and returns 0.
	vals := rh.resp.Header.Values(name)
	if len(vals) == 0 {
		return pluginsdk.ErrCodeOK
	}
	return writeBytes(mod, outPtr, outMax, []byte(strings.Join(vals, ", ")))
}

func (h *Host) httpResponseBodyRead(ctx context.Context, mod api.Module, handle, bufPtr, bufMax int32) int32 {
	if !pluginCaps(ctx).hasHTTPOutbound() {
		return pluginsdk.ErrCodePermissionDenied
	}
	rh, code := h.httpResponseFromHandle(mod, handle)
	if code != pluginsdk.ErrCodeOK {
		return code
	}
	if bufMax < 0 || bufMax > maxPayloadArg {
		return pluginsdk.ErrCodeTooLarge
	}
	if bufMax == 0 {
		return pluginsdk.ErrCodeOK
	}
	buf := make([]byte, bufMax)
	n, err := rh.resp.Body.Read(buf)
	if n > 0 {
		return writeBytes(mod, bufPtr, bufMax, buf[:n])
	}
	if errors.Is(err, errHTTPResponseTooLarge) {
		// The read crossed the per-response byte cap: fail loudly (never
		// silently truncate) and warn — an oversized upstream response is a
		// resource-abuse signal worth operator attention (ADR-0059).
		if h.logger != nil {
			var pluginID string
			if info := callFromCtx(ctx); info.plugin != nil {
				pluginID = info.plugin.manifest.Plugin.ID
			}
			h.logger.WarnContext(ctx, "plugins: http response body exceeds host cap",
				slog.String("plugin", pluginID),
				slog.String("host", rh.target),
				slog.Int64("limit_bytes", rh.limit))
		}
		return pluginsdk.ErrCodeTooLarge
	}
	if errors.Is(err, io.EOF) {
		return pluginsdk.ErrCodeOK // EOF
	}
	if err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}

func (h *Host) httpResponseClose(ctx context.Context, mod api.Module, handle int32) int32 {
	if !pluginCaps(ctx).hasHTTPOutbound() {
		return pluginsdk.ErrCodePermissionDenied
	}
	tabs := h.handlesFor(mod)
	if tabs == nil {
		return pluginsdk.ErrCodeInvalidArgument
	}
	v, ok := tabs.remove(handle, handleHTTP)
	if !ok {
		return pluginsdk.ErrCodeNotFound
	}
	rh, ok := v.(*httpResponseHandle)
	if !ok {
		return pluginsdk.ErrCodeInvalidArgument
	}
	if err := rh.Close(); err != nil {
		return pluginsdk.ErrCodeInternal
	}
	return pluginsdk.ErrCodeOK
}
