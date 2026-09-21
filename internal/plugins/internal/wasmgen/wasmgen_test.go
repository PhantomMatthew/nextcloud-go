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
		{"counter", CounterModule()},
		{"ctxuser", CtxUserModule()},
		{"cache-rt", CacheRoundTripModule("k", "v")},
		{"cache-incr", CacheIncrementModule("n", 5)},
		{"crypto-hash", CryptoHashModule("abc")},
		{"event-probe", EventProbeModule("files.x", -9)},
		{"route-probe", RouteProbeModule("/apps/x/y", -3)},
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
