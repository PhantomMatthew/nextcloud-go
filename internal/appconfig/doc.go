// Package appconfig provides a generic application configuration store, the
// Nextcloud oc_appconfig analogue: string key/value rows scoped by appid.
// The plugin config host functions store under appid "plugin" with keys
// namespaced <plugin_id>.<key>; core and the Admin UI can reuse the same
// table with their own appids.
package appconfig
