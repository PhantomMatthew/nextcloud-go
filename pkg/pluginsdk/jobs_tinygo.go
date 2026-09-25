//go:build tinygo

package pluginsdk

//go:wasmimport ncgo job_enqueue
func hostJobEnqueue(namePtr, nameLen, payloadPtr, payloadLen int32, runAtUnixMS int64) int32

// JobEnqueue schedules the plugin-local job name with payload, due at
// runAtUnixMS (milliseconds since epoch; <= 0 or past means now). Requires
// the jobs.register capability and an on_job entry point; the guest's
// ncgo_on_job receives name and payload verbatim.
func JobEnqueue(name string, payload []byte, runAtUnixMS int64) int32 {
	namePtr, nameLen := bytesPtr([]byte(name))
	payloadPtr, payloadLen := bytesPtr(payload)
	return hostJobEnqueue(namePtr, nameLen, payloadPtr, payloadLen, runAtUnixMS)
}

// JobArgs reads the name and payload passed to an on_job entry point.
func JobArgs(namePtr, nameLen, payloadPtr, payloadLen int32) (string, []byte) {
	return string(readAt(namePtr, nameLen)), readAt(payloadPtr, payloadLen)
}
