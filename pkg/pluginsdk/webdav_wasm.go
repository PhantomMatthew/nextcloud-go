//go:build wasm

package pluginsdk

//go:wasmimport ncgo webdav_register_prop
func hostWebDAVRegisterProp(namePtr, nameLen, getterPtr, getterLen, setterPtr, setterLen int32) int32

// WebDAVRegisterProp registers the WebDAV property name ("prefix:local")
// with the exported getter and setter function names; an empty setter
// registers a read-only property. Call it from a lifecycle hook only
// (on_install). Requires the webdav.props capability. The host invokes the
// getter as getter(path_ptr, path_len, out_ptr, out_max) -> i32 (the guest
// writes the raw value into out_ptr and returns its byte count) and the
// setter as setter(path_ptr, path_len, val_ptr, val_len) -> i32.
func WebDAVRegisterProp(name, getter, setter string) int32 {
	namePtr := allocString(name)
	getterPtr := allocString(getter)
	setterPtr, setterLen := bytesPtr([]byte(setter))
	return hostWebDAVRegisterProp(namePtr, int32(len(name)), getterPtr, int32(len(getter)), setterPtr, setterLen)
}

// WebDAVPropArgs reads the path passed to a prop getter or setter entry
// point, and (for setters) the value.
func WebDAVPropArgs(pathPtr, pathLen, ptr, length int32) (string, []byte) {
	return string(readAt(pathPtr, pathLen)), readAt(ptr, length)
}
