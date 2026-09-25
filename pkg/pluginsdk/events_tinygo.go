//go:build tinygo

package pluginsdk

//go:wasmimport ncgo event_publish
func hostEventPublish(topicPtr, topicLen, payloadPtr, payloadLen int32) int32

// EventPublish emits an event on the host bus. Requires an events.publish
// grant covering topic.
func EventPublish(topic string, payload []byte) int32 {
	topicPtr, topicLen := bytesPtr([]byte(topic))
	payloadPtr, payloadLen := bytesPtr(payload)
	return hostEventPublish(topicPtr, topicLen, payloadPtr, payloadLen)
}

// EventArgs reads the topic and payload passed to an on_event entry point.
func EventArgs(topicPtr, topicLen, payloadPtr, payloadLen int32) (string, []byte) {
	return string(readAt(topicPtr, topicLen)), readAt(payloadPtr, payloadLen)
}
