package wasmgen

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
)

func TestModulesCompile(t *testing.T) {
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { _ = r.Close(ctx) })
	for _, tc := range []struct {
		name string
		bin  []byte
	}{
		{"hello", HelloModule("hello from wasm")},
		{"wasi", WASIModule()},
		{"noexports", NoExportsModule()},
		{"loop", LoopModule()},
		{"oob", OOBLogModule()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := r.CompileModule(ctx, tc.bin)
			if err != nil {
				t.Fatalf("compile: %v (len=%d)", err, len(tc.bin))
			}
			_ = compiled.Close(ctx)
		})
	}
}
