//go:build tinygo

package main

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"
)

// createTable runs inside ncgo_on_install, the only place DDL is allowed.
// id INTEGER PRIMARY KEY auto-increments on SQLite; other dialects may need
// their own identity flavor (see README).
const createTable = `CREATE TABLE IF NOT EXISTS file_tags (
	id INTEGER PRIMARY KEY,
	user TEXT,
	path TEXT,
	tag TEXT,
	created_at INTEGER
)`

const insertTag = `INSERT INTO file_tags (user, path, tag, created_at) VALUES (?, ?, ?, ?)`

// invoiceSuffix marks uploads that should be auto-tagged.
const invoiceSuffix = ".invoice.pdf"

// uploadedPayload mirrors the msgpack map the host emits on files.uploaded.
type uploadedPayload struct {
	User    string `msgpack:"user"`
	Path    string `msgpack:"path"`
	Size    int64  `msgpack:"size"`
	Created bool   `msgpack:"created"`
}

//go:wasmexport ncgo_on_install
func onInstall() int32 {
	if _, code := pluginsdk.DBExec(createTable); code != pluginsdk.ErrCodeOK {
		pluginsdk.Error("file-tagger: create file_tags failed: code " + strconv.Itoa(int(code)))
		return code
	}
	pluginsdk.Info("file-tagger: file_tags table ready")
	return 0
}

//go:wasmexport ncgo_on_event
func onEvent(topicPtr, topicLen, payloadPtr, payloadLen int32) int32 {
	topic, payload := pluginsdk.EventArgs(topicPtr, topicLen, payloadPtr, payloadLen)
	if topic != "files.uploaded" {
		return 0
	}
	// Cheap pre-filter: the raw msgpack bytes contain the path verbatim, so a
	// non-matching upload is skipped without decoding.
	if !bytes.Contains(payload, []byte(invoiceSuffix)) {
		return 0
	}
	var ev uploadedPayload
	if err := msgpack.Unmarshal(payload, &ev); err != nil {
		// A malformed payload is logged, not propagated: returning non-zero
		// would only poison event delivery for a payload we cannot fix.
		pluginsdk.Warn("file-tagger: undecodable files.uploaded payload")
		return 0
	}
	if !strings.HasSuffix(ev.Path, invoiceSuffix) {
		return 0
	}
	// ABI v1 exposes no wall clock to freestanding guests, so created_at is
	// recorded as 0 until the host grows a clock binding.
	_, code := pluginsdk.DBExec(insertTag, ev.User, ev.Path, "invoice", int64(0))
	if code != pluginsdk.ErrCodeOK {
		pluginsdk.Error("file-tagger: tag insert for " + ev.Path + " failed: code " + strconv.Itoa(int(code)))
		return code
	}
	pluginsdk.Info("file-tagger: tagged " + ev.Path + " as invoice for " + ev.User)
	return 0
}

func main() {}
