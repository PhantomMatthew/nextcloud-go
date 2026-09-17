//go:build tinygo

package main

import "github.com/PhantomMatthew/nextcloud-go/pkg/pluginsdk"

//go:wasmexport ncgo_on_install
func onInstall() int32 {
	pluginsdk.Info("hello from wasm")
	return 0
}

func main() {}
