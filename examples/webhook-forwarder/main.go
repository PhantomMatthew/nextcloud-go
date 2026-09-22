//go:build tinygo

package main

import (
	"encoding/hex"
	"strconv"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// uploadedPayload mirrors the msgpack map the host emits on files.uploaded.
type uploadedPayload struct {
	User    string `msgpack:"user"`
	Path    string `msgpack:"path"`
	Size    int64  `msgpack:"size"`
	Created bool   `msgpack:"created"`
}

// configGetString reads a plugin-local config key; the bool reports whether
// the key exists and fit in buf.
func configGetString(key string, buf []byte) (string, bool) {
	n := pluginsdk.ConfigGet(key, buf)
	if n < 0 {
		return "", false
	}
	return string(buf[:n]), true
}

//go:wasmexport ncgo_on_event
func onEvent(topicPtr, topicLen, payloadPtr, payloadLen int32) int32 {
	topic, payload := pluginsdk.EventArgs(topicPtr, topicLen, payloadPtr, payloadLen)
	var ev uploadedPayload
	if err := msgpack.Unmarshal(payload, &ev); err != nil {
		pluginsdk.Warn("webhook-forwarder: undecodable payload on " + topic)
		return 0
	}

	// Not configured is not an error: a missing webhook.url skips quietly.
	urlBuf := make([]byte, 4096)
	url, ok := configGetString("webhook.url", urlBuf)
	if !ok {
		pluginsdk.Debug("webhook-forwarder: webhook.url not set; skipping " + topic)
		return 0
	}

	body := `{"event":` + strconv.Quote(topic) +
		`,"user":` + strconv.Quote(ev.User) +
		`,"path":` + strconv.Quote(ev.Path) +
		`,"size":` + strconv.FormatInt(ev.Size, 10) + `}`

	headers := map[string]string{"Content-Type": "application/json"}

	// Optional shared secret: sign the raw body with HMAC-SHA256.
	secretBuf := make([]byte, 1024)
	if secret, ok := configGetString("webhook.secret", secretBuf); ok {
		mac, code := pluginsdk.CryptoHMAC(pluginsdk.HashSHA256, []byte(secret), []byte(body))
		if code != pluginsdk.ErrCodeOK {
			pluginsdk.Error("webhook-forwarder: hmac failed: code " + strconv.Itoa(int(code)))
			return code
		}
		headers["X-Signature"] = hex.EncodeToString(mac)
	}

	resp, code := pluginsdk.HTTPDo(&pluginsdk.HTTPOutboundRequest{
		Method:    "POST",
		URL:       url,
		Headers:   headers,
		Body:      []byte(body),
		TimeoutMS: 5000,
	})
	if code != pluginsdk.ErrCodeOK {
		pluginsdk.Error("webhook-forwarder: POST " + url + " failed: code " + strconv.Itoa(int(code)))
		// Transport failures must not retry-storm the bus; setup errors
		// (denied host, bad request) are real bugs worth surfacing.
		if code == pluginsdk.ErrCodePermissionDenied || code == pluginsdk.ErrCodeInvalidArgument {
			return code
		}
		return 0
	}
	pluginsdk.Info("webhook-forwarder: POST " + url + " -> " + strconv.Itoa(int(resp.Status())))
	return resp.Close()
}

func main() {}
