//go:build wasm

package pluginsdk

//go:wasmimport ncgo route_register
func hostRouteRegister(methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen int32) int32

//go:wasmimport ncgo ocs_register
func hostOCSRegister(methodPtr, methodLen, pathPtr, pathLen, handlerPtr, handlerLen int32) int32

// RouteRegister registers an HTTP route: method, a path under the plugin's
// own /apps/<plugin_id>/ namespace, and the exported handler function name.
// Call it from a lifecycle hook only (on_install/on_upgrade). Requires the
// routes.register capability covering the path.
func RouteRegister(method, path, handlerName string) int32 {
	return hostRouteRegister(
		allocString(method), int32(len(method)),
		allocString(path), int32(len(path)),
		allocString(handlerName), int32(len(handlerName)),
	)
}

// OCSRegister registers an OCS endpoint: method, a path under the plugin's
// own /apps/<plugin_id>/ namespace, and the exported handler function name.
// Call it from a lifecycle hook only (on_install/on_upgrade). Requires the
// ocs.register capability covering the path.
func OCSRegister(method, path, handlerName string) int32 {
	return hostOCSRegister(
		allocString(method), int32(len(method)),
		allocString(path), int32(len(path)),
		allocString(handlerName), int32(len(handlerName)),
	)
}
